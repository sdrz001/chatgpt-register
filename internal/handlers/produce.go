package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"chatgpt-register/internal/models"
	"chatgpt-register/internal/producer"

	"github.com/gin-gonic/gin"
)

// exportBundle 是导出的顶层结构：一个文件包含多个账号。
type exportBundle struct {
	ExportedAt string          `json:"exported_at"`
	Proxies    []any           `json:"proxies"`
	Accounts   []exportAccount `json:"accounts"`
}

type exportAccount struct {
	Name        string         `json:"name"`
	Platform    string         `json:"platform"`
	Type        string         `json:"type"`
	Credentials map[string]any `json:"credentials"`
}

// buildCredentials 把库里存的 auth.json 映射成导出用的 credentials。
// 兼容新 access_token 结构与旧 agent_identity 结构。
func buildCredentials(authData, email string) map[string]any {
	var parsed map[string]any
	_ = json.Unmarshal([]byte(authData), &parsed)
	if parsed == nil {
		parsed = map[string]any{}
	}

	// 优先顶层字段（新格式）；旧格式回退到 agent_identity 嵌套。
	src := parsed
	if ai, ok := parsed["agent_identity"].(map[string]any); ok && ai != nil {
		if _, hasAT := parsed["access_token"]; !hasAT {
			src = ai
		}
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := src[k].(string); ok && s != "" {
				return s
			}
			if s, ok := parsed[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	planType := str("plan_type")
	if planType == "" {
		planType = "free"
	}
	em := str("email")
	if em == "" {
		em = email
	}
	authMode := str("auth_mode")
	if authMode == "" {
		if str("access_token") != "" {
			authMode = "accessToken"
		} else {
			authMode = "agentIdentity"
		}
	}
	out := map[string]any{
		"auth_mode":          authMode,
		"access_token":       str("access_token"),
		"chatgpt_account_id": str("account_id", "chatgpt_account_id"),
		"chatgpt_user_id":    str("chatgpt_user_id", "user_id"),
		"email":              em,
		"plan_type":          planType,
	}
	for _, key := range []string{"refresh_token", "id_token", "expires_at"} {
		if value := str(key); value != "" {
			out[key] = value
		}
	}
	if value, ok := src["expires_in"]; ok {
		out["expires_in"] = value
	} else if value, ok := parsed["expires_in"]; ok {
		out["expires_in"] = value
	}
	if v := str("agent_private_key"); v != "" {
		out["agent_private_key"] = v
	}
	if v := str("agent_runtime_id"); v != "" {
		out["agent_runtime_id"] = v
	}
	return out
}

type produceInput struct {
	Count       int    `json:"count"`
	Scope       string `json:"scope"`
	CategoryID  *uint  `json:"category_id"`
	MailboxIDs  []uint `json:"mailbox_ids"`
	ProxyPoolID *uint  `json:"proxy_pool_id"`
}

// Produce 启动一次注册任务。
func (h *Handler) Produce(c *gin.Context) {
	var in produceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	scope, err := h.validateProduceScope(in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.applyProduceProxyPool(&scope, in.ProxyPoolID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	browserStatus := h.checkBrowserBackend(c.Request.Context())
	if !browserStatus.Ready {
		message := browserStatus.Message
		if browserStatus.Error != "" {
			message += "：" + browserStatus.Error
		}
		c.JSON(http.StatusConflict, gin.H{"error": message})
		return
	}
	if err := h.Producer.Start(in.Count, scope); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) validateProduceScope(in produceInput) (producer.Scope, error) {
	switch in.Scope {
	case "", "all":
		return producer.Scope{}, nil
	case "category":
		if in.CategoryID == nil {
			return producer.Scope{}, fmt.Errorf("请选择邮箱分组")
		}
		var count int64
		h.DB.Model(&models.Category{}).Where("id = ? AND scope = ?", *in.CategoryID, "mailbox").Count(&count)
		if count == 0 {
			return producer.Scope{}, fmt.Errorf("邮箱分组不存在")
		}
		return producer.Scope{CategoryID: in.CategoryID}, nil
	case "mailboxes":
		ids := uniqueMailboxIDs(in.MailboxIDs)
		if len(ids) == 0 {
			return producer.Scope{}, fmt.Errorf("请选择至少一个邮箱")
		}
		var count int64
		h.DB.Model(&models.Mailbox{}).Where("id IN ? AND status = ?", ids, "verified").Count(&count)
		if count != int64(len(ids)) {
			return producer.Scope{}, fmt.Errorf("所选邮箱不存在或尚未验证")
		}
		return producer.Scope{MailboxIDs: ids}, nil
	default:
		return producer.Scope{}, fmt.Errorf("注册范围无效")
	}
}

func (h *Handler) applyProduceProxyPool(scope *producer.Scope, requestedID *uint) error {
	poolID := h.defaultProxyPoolID()
	if requestedID != nil {
		poolID = *requestedID
	}
	if poolID == 0 {
		return nil
	}
	var pool models.ProxyPool
	if err := h.DB.First(&pool, poolID).Error; err != nil {
		return fmt.Errorf("所选代理池不存在")
	}
	proxies, err := proxyPoolLines(pool.Proxies)
	if err != nil {
		return err
	}
	if len(proxies) == 0 {
		return fmt.Errorf("所选代理池没有可用代理")
	}
	scope.ProxyPoolID = pool.ID
	scope.ProxyPoolName = pool.Name
	scope.Proxies = append([]string(nil), proxies...)
	return nil
}

func uniqueMailboxIDs(ids []uint) []uint {
	seen := make(map[uint]struct{}, len(ids))
	unique := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })
	return unique
}

// ProduceStatus 返回生产进度（待生产/在跑/已注册/失败/日志）。
func (h *Handler) ProduceStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.Producer.Snapshot())
}

// BrowserStatus 返回当前所选浏览器后端的就绪状态。
func (h *Handler) BrowserStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.checkBrowserBackend(c.Request.Context()))
}

// ProduceStop 停止生产。
func (h *Handler) ProduceStop(c *gin.Context) {
	h.Producer.Stop()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// RegistrationLog 返回单个账号的执行日志。
func (h *Handler) RegistrationLog(c *gin.Context) {
	var reg models.Registration
	if err := h.DB.First(&reg, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"email": reg.Email, "status": reg.Status,
		"note":     strings.ToValidUTF8(reg.Note, "�"),
		"log":      strings.ToValidUTF8(reg.Log, "�"),
		"has_shot": len(reg.Shot) > 0,
	})
}

// RegistrationShot 返回单个账号注册失败时保存的页面截图(PNG)。
func (h *Handler) RegistrationShot(c *gin.Context) {
	var reg models.Registration
	if err := h.DB.First(&reg, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if len(reg.Shot) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "暂无异常截图"})
		return
	}
	c.Data(http.StatusOK, "image/png", reg.Shot)
}

// SetShipped 禁止手动切换出库状态。
// 出库状态只能由下载接口自动标记，避免库存状态被人工改乱。
func (h *Handler) SetShipped(c *gin.Context) {
	c.JSON(http.StatusForbidden, gin.H{"error": "出库状态已锁定，只能由下载操作自动更新"})
}

func (h *Handler) RegistrationMailboxLinks(c *gin.Context) {
	var in struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择账号"})
		return
	}

	var regs []models.Registration
	if err := h.DB.Select("id", "email", "password", "registration_flow", "two_factor_enabled", "two_factor_secret", "mailbox_id").Where("id IN ?", in.IDs).Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	regByID := make(map[uint]models.Registration, len(regs))
	mailboxIDs := make([]uint, 0, len(regs))
	for _, reg := range regs {
		regByID[reg.ID] = reg
		if reg.MailboxID != 0 {
			mailboxIDs = append(mailboxIDs, reg.MailboxID)
		}
	}

	var mailboxes []models.Mailbox
	if len(mailboxIDs) > 0 {
		if err := h.DB.Select("id", "code_url").Where("id IN ?", mailboxIDs).Find(&mailboxes).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	mailboxByID := make(map[uint]models.Mailbox, len(mailboxes))
	for _, mailbox := range mailboxes {
		mailboxByID[mailbox.ID] = mailbox
	}

	type mailboxLink struct {
		RegistrationID uint   `json:"registration_id"`
		Email          string `json:"email"`
		Password       string `json:"-"`
		TOTPSecret     string `json:"-"`
		CodeURL        string `json:"code_url"`
		Line           string `json:"line"`
	}
	items := make([]mailboxLink, 0, len(in.IDs))
	for _, id := range in.IDs {
		reg, exists := regByID[id]
		if !exists {
			continue
		}
		mailbox, exists := mailboxByID[reg.MailboxID]
		codeURL := strings.TrimSpace(mailbox.CodeURL)
		if !exists || codeURL == "" {
			continue
		}
		password := ""
		totpSecret := ""
		if strings.EqualFold(strings.TrimSpace(reg.RegistrationFlow), "password") {
			password = strings.TrimSpace(reg.Password)
			if reg.TwoFactorEnabled {
				totpSecret = strings.TrimSpace(reg.TwoFactorSecret)
			}
		}
		parts := []string{reg.Email}
		if password != "" {
			parts = append(parts, password)
		}
		if totpSecret != "" {
			parts = append(parts, totpSecret)
		}
		parts = append(parts, codeURL)
		items = append(items, mailboxLink{
			RegistrationID: reg.ID, Email: reg.Email, Password: password,
			TOTPSecret: totpSecret, CodeURL: codeURL, Line: strings.Join(parts, "----"),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"items":   items,
		"count":   len(items),
		"skipped": len(in.IDs) - len(items),
	})
}

func (h *Handler) AccessTokens(c *gin.Context) {
	var in struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择账号"})
		return
	}

	var regs []models.Registration
	if err := h.DB.Where("id IN ? AND status = ? AND auth_data <> ''", in.IDs, "registered").
		Order("id").Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	tokens := make([]string, 0, len(regs))
	for _, reg := range regs {
		credentials := buildCredentials(reg.AuthData, reg.Email)
		token, _ := credentials["access_token"].(string)
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	if len(tokens) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选账号没有可复制的 AT"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"tokens":  tokens,
		"count":   len(tokens),
		"skipped": len(in.IDs) - len(tokens),
	})
}

// Download 下载选中账号的 auth.json：单个→对象，多个→数组；下载即标记出库。
// 请求体：{ "ids": [1,2,3] }。
func (h *Handler) Download(c *gin.Context) {
	var in struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择账号"})
		return
	}

	var regs []models.Registration
	if err := h.DB.Where("id IN ? AND status = ? AND auth_data <> ''", in.IDs, "registered").
		Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(regs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选账号没有可下载的已注册数据"})
		return
	}

	accounts := make([]exportAccount, 0, len(regs))
	ids := make([]uint, 0, len(regs))
	for _, r := range regs {
		accounts = append(accounts, exportAccount{
			Name:        r.Email,
			Platform:    "openai",
			Type:        "oauth",
			Credentials: buildCredentials(r.AuthData, r.Email),
		})
		ids = append(ids, r.ID)
	}

	// 下载即出库
	h.DB.Model(&models.Registration{}).Where("id IN ?", ids).Update("shipped", true)

	bundle := exportBundle{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Proxies:    []any{},
		Accounts:   accounts,
	}
	out, _ := json.MarshalIndent(bundle, "", "  ")
	c.Header("Content-Disposition", "attachment; filename=auth.json")
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}
