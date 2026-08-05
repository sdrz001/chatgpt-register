package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"chatgpt-register/internal/auth"
	"chatgpt-register/internal/browserboot"
	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/producer"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Handler struct {
	DB             *gorm.DB
	Mail           *mailfetch.Client
	Auth           *auth.Service
	Producer       *producer.Producer
	Browser        *browserboot.Manager
	ATCheckURL     string
	ATCheckClient  func(string) (*http.Client, error)
	autoATCheck    atomic.Bool
	atCheckMu      sync.Mutex
	atChecking     map[uint]bool
	autoATCheckOps map[uint]context.CancelFunc
	atCheckSlots   chan struct{}
}

func New(db *gorm.DB, authSvc *auth.Service, browser *browserboot.Manager) *Handler {
	mail := mailfetch.New()
	handler := &Handler{
		DB: db, Mail: mail, Auth: authSvc, Producer: producer.New(db, mail), Browser: browser,
		ATCheckURL:     "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=0",
		ATCheckClient:  func(proxy string) (*http.Client, error) { return newProxyHTTPClient(proxy, 20*time.Second) },
		atChecking:     make(map[uint]bool),
		autoATCheckOps: make(map[uint]context.CancelFunc),
		atCheckSlots:   make(chan struct{}, 3),
	}
	handler.setAutoATCheck(handler.autoATCheckSetting())
	go handler.runATChecker()
	return handler
}

func (h *Handler) autoATCheckSetting() bool {
	var settings []models.Setting
	if h.DB.Where("key = ?", "at_auto_check").Limit(1).Find(&settings).Error != nil || len(settings) == 0 {
		return true
	}
	return strings.TrimSpace(settings[0].Value) != "0"
}

func (h *Handler) setAutoATCheck(enabled bool) {
	wasEnabled := h.autoATCheck.Swap(enabled)
	if enabled {
		if !wasEnabled {
			go h.schedulePendingATChecks()
		}
		return
	}
	h.atCheckMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(h.autoATCheckOps))
	for _, cancel := range h.autoATCheckOps {
		cancels = append(cancels, cancel)
	}
	h.atCheckMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

type registrationInput struct {
	Email      string `json:"email" binding:"required"`
	Password   string `json:"password"`
	Username   string `json:"username"`
	Status     string `json:"status"`
	CategoryID *uint  `json:"category_id"`
	Note       string `json:"note"`
}

func validStatus(s string) bool {
	return s == "" || s == "pending" || s == "registering" ||
		s == "registered" || s == "register_failed" || s == "already_registered"
}

func (h *Handler) List(c *gin.Context) {
	var regs []models.Registration
	q := h.DB.Order("created_at desc, id desc")

	if s := c.Query("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	if s := c.Query("at_status"); s != "" {
		q = q.Where("at_status = ?", s)
	}
	if s := c.Query("plan_type"); s != "" {
		q = q.Where("LOWER(plan_type) = ?", strings.ToLower(s))
	}
	if s := c.Query("category_id"); s != "" {
		if s == "uncategorized" {
			q = q.Where("category_id IS NULL")
		} else if categoryID, err := strconv.ParseUint(s, 10, 64); err == nil {
			q = q.Where("category_id = ?", categoryID)
		}
	}
	if kw := c.Query("q"); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("email LIKE ? OR username LIKE ? OR note LIKE ?", like, like, like)
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}

	var total int64
	q.Model(&models.Registration{}).Count(&total)
	if err := q.Preload("Category").Offset((page - 1) * size).Limit(size).Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 列表不返回 auth_data / log（含私钥、体积大），仅在下载/日志接口按需返回
	for i := range regs {
		if regs[i].Status == "registered" && strings.TrimSpace(regs[i].AuthData) == "" {
			regs[i].ATStatus = "missing"
		} else if h.autoATCheck.Load() {
			h.scheduleATCheck(regs[i], false)
		}
		regs[i].AuthData = ""
		regs[i].Log = ""
	}
	c.JSON(http.StatusOK, gin.H{"data": regs, "total": total, "page": page, "size": size})
}

func (h *Handler) Get(c *gin.Context) {
	var reg models.Registration
	if err := h.DB.First(&reg, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	// 不在通用接口返回私钥/完整 auth，仅下载接口按需返回
	reg.AuthData = ""
	c.JSON(http.StatusOK, reg)
}

func (h *Handler) Create(c *gin.Context) {
	var in registrationInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validStatus(in.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	if !h.categoryExists("account", in.CategoryID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账户分类不存在"})
		return
	}
	reg := models.Registration{
		Email:    in.Email,
		Password: in.Password,
		Username: in.Username,
		Status:   in.Status, CategoryID: in.CategoryID, Note: in.Note,
	}
	if reg.Status == "" {
		reg.Status = "pending"
	}
	if err := h.DB.Create(&reg).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, reg)
}

func (h *Handler) Update(c *gin.Context) {
	var reg models.Registration
	if err := h.DB.First(&reg, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var in registrationInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validStatus(in.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	if !h.categoryExists("account", in.CategoryID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账户分类不存在"})
		return
	}
	reg.Email = in.Email
	reg.Password = in.Password
	reg.Username = in.Username
	if in.Status != "" {
		reg.Status = in.Status
	}
	reg.CategoryID = in.CategoryID
	reg.Note = in.Note
	if err := h.DB.Save(&reg).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, reg)
}

func (h *Handler) Delete(c *gin.Context) {
	if err := h.DB.Delete(&models.Registration{}, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) runATChecker() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		h.schedulePendingATChecks()
	}
}

func (h *Handler) schedulePendingATChecks() {
	if !h.autoATCheck.Load() {
		return
	}
	var registrations []models.Registration
	cutoff := time.Now().Add(-30 * time.Minute)
	if h.DB.Where("status = ? AND auth_data <> '' AND (at_checked_at IS NULL OR at_checked_at < ?)", "registered", cutoff).
		Order("at_checked_at, id").Limit(200).Find(&registrations).Error != nil {
		return
	}
	for _, registration := range registrations {
		h.scheduleATCheck(registration, false)
	}
}

type atCheckResult struct {
	Status    string
	PlanType  string
	Error     string
	ExpiresAt *time.Time
}

func (h *Handler) scheduleATCheck(registration models.Registration, manual bool) bool {
	if registration.Status != "registered" || strings.TrimSpace(registration.AuthData) == "" {
		return false
	}
	if !manual {
		if !h.autoATCheck.Load() || registration.ATCheckedAt != nil && time.Since(*registration.ATCheckedAt) < 30*time.Minute {
			return false
		}
	}
	ctx := context.Background()
	var cancel context.CancelFunc
	if !manual {
		ctx, cancel = context.WithCancel(ctx)
	}
	previousStatus := registration.ATStatus
	if previousStatus == "" || previousStatus == "checking" {
		previousStatus = "unchecked"
	}
	h.atCheckMu.Lock()
	if !manual && !h.autoATCheck.Load() {
		h.atCheckMu.Unlock()
		cancel()
		return false
	}
	if h.atChecking == nil {
		h.atChecking = make(map[uint]bool)
	}
	if h.atCheckSlots == nil {
		h.atCheckSlots = make(chan struct{}, 3)
	}
	if h.atChecking[registration.ID] {
		h.atCheckMu.Unlock()
		if cancel != nil {
			cancel()
		}
		return false
	}
	if h.autoATCheckOps == nil {
		h.autoATCheckOps = make(map[uint]context.CancelFunc)
	}
	h.atChecking[registration.ID] = true
	if cancel != nil {
		h.autoATCheckOps[registration.ID] = cancel
	}
	slots := h.atCheckSlots
	h.atCheckMu.Unlock()
	updated := h.DB.Model(&models.Registration{}).Where("id = ? AND auth_data = ?", registration.ID, registration.AuthData).
		Updates(map[string]any{"at_status": "checking", "at_error": ""})
	if updated.Error != nil || updated.RowsAffected == 0 {
		if cancel != nil {
			cancel()
		}
		h.finishATCheck(registration.ID)
		return false
	}
	go func(id uint) {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			h.restoreCanceledATCheck(id, registration.AuthData, previousStatus)
			h.finishATCheck(id)
			return
		}
		defer func() {
			<-slots
			h.finishATCheck(id)
		}()
		var current models.Registration
		if h.DB.First(&current, id).Error != nil {
			h.restoreCanceledATCheck(id, registration.AuthData, previousStatus)
			return
		}
		result := h.checkRegistrationAT(ctx, current)
		if ctx.Err() != nil {
			h.restoreCanceledATCheck(id, current.AuthData, previousStatus)
			return
		}
		now := time.Now()
		updates := map[string]any{
			"at_status": result.Status, "at_error": result.Error,
			"at_checked_at": now, "at_expires_at": result.ExpiresAt,
		}
		if result.PlanType != "" {
			updates["plan_type"] = result.PlanType
			updates["auth_data"] = authDataWithPlan(current.AuthData, result.PlanType)
		}
		h.DB.Model(&models.Registration{}).Where("id = ? AND auth_data = ?", id, current.AuthData).Updates(updates)
	}(registration.ID)
	return true
}

func (h *Handler) restoreCanceledATCheck(id uint, authData, previousStatus string) {
	h.DB.Model(&models.Registration{}).Where("id = ? AND auth_data = ?", id, authData).
		Updates(map[string]any{"at_status": previousStatus, "at_error": ""})
}

func (h *Handler) finishATCheck(id uint) {
	h.atCheckMu.Lock()
	delete(h.atChecking, id)
	delete(h.autoATCheckOps, id)
	h.atCheckMu.Unlock()
}

func (h *Handler) checkRegistrationAT(ctx context.Context, registration models.Registration) atCheckResult {
	credentials := buildCredentials(registration.AuthData, registration.Email)
	token, _ := credentials["access_token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return atCheckResult{Status: "missing", Error: "账号没有 AT"}
	}
	planType, expiresAt, _ := codexreg.AccessTokenDetails(token)
	planType = normalizePlanType(planType)
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return atCheckResult{Status: "invalid", PlanType: planType, Error: "AT 已过期", ExpiresAt: expiresAt}
	}
	clientFactory := h.ATCheckClient
	if clientFactory == nil {
		clientFactory = func(proxy string) (*http.Client, error) { return newProxyHTTPClient(proxy, 20*time.Second) }
	}
	client, err := clientFactory(registration.Proxy)
	if err != nil {
		return atCheckResult{Status: "error", PlanType: planType, Error: "代理配置错误", ExpiresAt: expiresAt}
	}
	endpoint := strings.TrimSpace(h.ATCheckURL)
	if endpoint == "" {
		endpoint = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=0"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return atCheckResult{Status: "error", PlanType: planType, Error: "检测请求创建失败", ExpiresAt: expiresAt}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/150.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://chatgpt.com/")
	if registration.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", registration.AccountID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return atCheckResult{Status: "error", PlanType: planType, Error: "AT 检测网络异常", ExpiresAt: expiresAt}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return atCheckResult{Status: "error", PlanType: planType, Error: "AT 检测响应读取失败", ExpiresAt: expiresAt}
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if onlinePlan := planTypeFromAccountsResponse(body, registration.AccountID); onlinePlan != "" {
			planType = onlinePlan
		}
		return atCheckResult{Status: "valid", PlanType: planType, ExpiresAt: expiresAt}
	case http.StatusUnauthorized:
		return atCheckResult{Status: "invalid", PlanType: planType, Error: "服务端已拒绝该 AT", ExpiresAt: expiresAt}
	case http.StatusForbidden:
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
			return atCheckResult{Status: "invalid", PlanType: planType, Error: "账户或 AT 已被服务端停用", ExpiresAt: expiresAt}
		}
		return atCheckResult{Status: "error", PlanType: planType, Error: "AT 检测被网关拦截", ExpiresAt: expiresAt}
	default:
		return atCheckResult{Status: "error", PlanType: planType, Error: fmt.Sprintf("AT 检测返回 HTTP %d", resp.StatusCode), ExpiresAt: expiresAt}
	}
}

func authDataWithPlan(authData, planType string) string {
	var root map[string]any
	if json.Unmarshal([]byte(authData), &root) != nil || root == nil {
		return authData
	}
	root["plan_type"] = planType
	if credentials, ok := root["credentials"].(map[string]any); ok {
		credentials["plan_type"] = planType
	}
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return authData
	}
	return string(encoded)
}

func planTypeFromAccountsResponse(body []byte, preferredAccountID string) string {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	if accountPlan, ok := root["account_plan"].(map[string]any); ok {
		if plan := normalizePlanType(firstMapString(accountPlan, "plan_type", "subscription_plan")); plan != "" {
			return plan
		}
	}
	accounts, _ := root["accounts"].(map[string]any)
	bestPlan, bestScore := "", -1
	for id, raw := range accounts {
		entry, _ := raw.(map[string]any)
		if !usableAccountCandidate(entry, time.Now()) {
			continue
		}
		account, _ := entry["account"].(map[string]any)
		if account == nil {
			account = entry
		}
		if value, _ := account["is_deactivated"].(bool); value {
			continue
		}
		plan := normalizePlanType(firstMapString(account, "plan_type", "subscription_plan"))
		if entitlement, ok := entry["entitlement"].(map[string]any); ok {
			if entitlementPlan := normalizePlanType(firstMapString(entitlement, "subscription_plan", "plan_type")); entitlementPlan != "" {
				plan = entitlementPlan
			}
		}
		if plan == "" {
			continue
		}
		score := planPriority(plan)
		if id == preferredAccountID || firstMapString(account, "id", "account_id") == preferredAccountID {
			score += 1000
		}
		if isDefault, _ := account["is_default"].(bool); isDefault {
			score += 100
		}
		if score > bestScore {
			bestPlan, bestScore = plan, score
		}
	}
	return bestPlan
}

func usableAccountCandidate(entry map[string]any, now time.Time) bool {
	if boolMapValue(entry, "deactivated", "is_deactivated", "disabled", "is_disabled") {
		return false
	}
	if account, ok := entry["account"].(map[string]any); ok && boolMapValue(account, "deactivated", "is_deactivated", "disabled", "is_disabled") {
		return false
	}
	entitlement, _ := entry["entitlement"].(map[string]any)
	expiresAt := firstMapString(entitlement, "expires_at", "active_until")
	if expiresAt == "" {
		return true
	}
	expiry, err := time.Parse(time.RFC3339, expiresAt)
	return err != nil || expiry.After(now)
}

func boolMapValue(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := values[key].(bool); ok && value {
			return true
		}
	}
	return false
}

func firstMapString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizePlanType(value string) string {
	return codexreg.NormalizePlanType(value)
}

func planPriority(plan string) int {
	switch plan {
	case "enterprise":
		return 80
	case "business", "team":
		return 70
	case "edu":
		return 60
	case "pro":
		return 50
	case "plus":
		return 40
	case "go":
		return 30
	case "free":
		return 10
	default:
		return 1
	}
}

func (h *Handler) RegistrationATCheck(c *gin.Context) {
	var input struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || len(input.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择要检测的账户"})
		return
	}
	var registrations []models.Registration
	if err := h.DB.Where("id IN ?", input.IDs).Find(&registrations).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	scheduled := 0
	for _, registration := range registrations {
		if h.scheduleATCheck(registration, true) {
			scheduled++
		}
	}
	c.JSON(http.StatusAccepted, gin.H{"scheduled": scheduled, "count": len(registrations)})
}

func validCategoryScope(scope string) bool {
	return scope == "account" || scope == "mailbox"
}

func (h *Handler) categoryExists(scope string, categoryID *uint) bool {
	if categoryID == nil {
		return true
	}
	var count int64
	h.DB.Model(&models.Category{}).Where("id = ? AND scope = ?", *categoryID, scope).Count(&count)
	return count == 1
}

func (h *Handler) CategoryList(c *gin.Context) {
	scope := strings.TrimSpace(c.Query("scope"))
	if !validCategoryScope(scope) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "分类作用域错误"})
		return
	}
	var categories []models.Category
	if err := h.DB.Where("scope = ?", scope).Order("name, id").Find(&categories).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range categories {
		if scope == "account" {
			h.DB.Model(&models.Registration{}).Where("category_id = ?", categories[i].ID).Count(&categories[i].ItemCount)
		} else {
			h.DB.Model(&models.Mailbox{}).Where("category_id = ?", categories[i].ID).Count(&categories[i].ItemCount)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": categories})
}

func (h *Handler) CategoryCreate(c *gin.Context) {
	var input struct {
		Scope string `json:"scope"`
		Name  string `json:"name"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	input.Scope = strings.TrimSpace(input.Scope)
	input.Name = strings.TrimSpace(input.Name)
	if !validCategoryScope(input.Scope) || input.Name == "" || len([]rune(input.Name)) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "分类名称需为 1 到 64 个字符"})
		return
	}
	var count int64
	h.DB.Model(&models.Category{}).Where("scope = ? AND LOWER(name) = ?", input.Scope, strings.ToLower(input.Name)).Count(&count)
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "分类名称已存在"})
		return
	}
	category := models.Category{Scope: input.Scope, Name: input.Name}
	if err := h.DB.Create(&category).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "分类名称已存在"})
		return
	}
	c.JSON(http.StatusCreated, category)
}

func (h *Handler) CategoryUpdate(c *gin.Context) {
	var category models.Category
	if err := h.DB.First(&category, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "分类不存在"})
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len([]rune(input.Name)) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "分类名称需为 1 到 64 个字符"})
		return
	}
	var count int64
	h.DB.Model(&models.Category{}).Where("scope = ? AND LOWER(name) = ? AND id <> ?", category.Scope, strings.ToLower(input.Name), category.ID).Count(&count)
	if count > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "分类名称已存在"})
		return
	}
	if err := h.DB.Model(&category).Update("name", input.Name).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "分类名称已存在"})
		return
	}
	category.Name = input.Name
	c.JSON(http.StatusOK, category)
}

func (h *Handler) CategoryDelete(c *gin.Context) {
	var category models.Category
	if err := h.DB.First(&category, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "分类不存在"})
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if category.Scope == "account" {
			if err := tx.Model(&models.Registration{}).Where("category_id = ?", category.ID).Update("category_id", nil).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Model(&models.Mailbox{}).Where("category_id = ?", category.ID).Update("category_id", nil).Error; err != nil {
				return err
			}
		}
		return tx.Delete(&category).Error
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) CategoryAssign(c *gin.Context) {
	var input struct {
		Scope      string `json:"scope"`
		IDs        []uint `json:"ids"`
		CategoryID *uint  `json:"category_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || len(input.IDs) == 0 || !validCategoryScope(input.Scope) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "归类参数错误"})
		return
	}
	if !h.categoryExists(input.Scope, input.CategoryID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "分类不存在或作用域不匹配"})
		return
	}
	query := h.DB.Model(&models.Registration{})
	if input.Scope == "mailbox" {
		query = h.DB.Model(&models.Mailbox{})
	}
	var categoryID any
	if input.CategoryID != nil {
		categoryID = *input.CategoryID
	}
	if err := query.Where("id IN ?", input.IDs).Update("category_id", categoryID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "count": len(input.IDs)})
}

func (h *Handler) Stats(c *gin.Context) {
	reg := func(where ...any) int64 {
		var n int64
		q := h.DB.Model(&models.Registration{})
		if len(where) > 0 {
			q = q.Where(where[0], where[1:]...)
		}
		q.Count(&n)
		return n
	}

	total := reg()
	pending := reg("status = ?", "pending")
	registering := reg("status = ?", "registering")
	registered := reg("status = ?", "registered")
	registerFailed := reg("status = ?", "register_failed")
	shipped := reg("shipped = ?", true)
	mother := reg("is_mother = ?", true)
	fission := reg("is_mother = ?", false)

	var mailboxes, mailboxVerified int64
	h.DB.Model(&models.Mailbox{}).Count(&mailboxes)
	h.DB.Model(&models.Mailbox{}).Where("status = ?", "verified").Count(&mailboxVerified)

	// 已注册但未出库（可下载库存）
	unshipped := registered - shipped
	if unshipped < 0 {
		unshipped = 0
	}

	// 套餐分布
	type kv struct {
		PlanType string
		N        int64
	}
	var plans []kv
	h.DB.Model(&models.Registration{}).
		Select("plan_type, count(*) as n").
		Where("status = ? AND plan_type <> ''", "registered").
		Group("plan_type").Scan(&plans)
	planBreak := make(map[string]int64, len(plans))
	for _, p := range plans {
		planBreak[p.PlanType] = p.N
	}

	// 近 7 天已注册产量趋势
	type day struct {
		D string
		N int64
	}
	var days []day
	h.DB.Model(&models.Registration{}).
		Select("strftime('%Y-%m-%d', created_at) as d, count(*) as n").
		Where("status = ?", "registered").
		Group("d").Order("d desc").Limit(7).Scan(&days)
	trend := make([]gin.H, 0, len(days))
	for i := len(days) - 1; i >= 0; i-- {
		trend = append(trend, gin.H{"date": days[i].D, "count": days[i].N})
	}

	prog := h.Producer.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"total": total, "pending": pending, "registering": registering,
		"registered": registered, "register_failed": registerFailed,
		"shipped": shipped, "unshipped": unshipped,
		"mother": mother, "fission": fission,
		"mailboxes": mailboxes, "mailbox_verified": mailboxVerified,
		"plans": planBreak, "trend": trend,
		"running": prog.Running, "produce_target": prog.Target,
		"produce_pending": prog.Pending, "produce_running": prog.RunningNum,
		"produce_registered": prog.Registered, "produce_failed": prog.Failed,
		"produce_message": prog.Message,
	})
}
