package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"chatgpt-register/internal/domainmail"
	"chatgpt-register/internal/emailalias"
	"chatgpt-register/internal/integrationcfg"
	"chatgpt-register/internal/mailcom"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type mailboxInput struct {
	Email        string `json:"email" binding:"required"`
	Password     string `json:"password"`
	Provider     string `json:"provider"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
	CodeURL      string `json:"code_url"`
	Status       string `json:"status"`
	CategoryID   *uint  `json:"category_id"`
	Note         string `json:"note"`
}

var mailboxStatuses = map[string]bool{
	"unverified":    true,
	"verifying":     true,
	"verify_failed": true,
	"verified":      true,
}

func validMailboxStatus(s string) bool {
	return s == "" || mailboxStatuses[s]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func validateMailComInput(provider, email, password, codeURL string) error {
	if !strings.EqualFold(strings.TrimSpace(provider), mailcom.Provider) {
		return nil
	}
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("mail.com 邮箱密码不能为空")
	}
	if strings.TrimSpace(codeURL) != "" {
		return fmt.Errorf("mail.com 不能同时配置取码 API 地址")
	}
	if !mailcom.IsAddress(email) {
		return fmt.Errorf("该地址不属于已知的 mail.com 邮箱域名")
	}
	return nil
}

func (h *Handler) mailboxAccount(mailbox models.Mailbox) (mailfetch.Account, error) {
	account := mailfetch.Account{
		Email: mailbox.Email, Password: mailbox.Password, Provider: mailbox.Provider, ClientID: mailbox.ClientID,
		RefreshToken: mailbox.RefreshToken, CodeURL: mailbox.CodeURL, RemoteMailboxID: mailbox.RemoteMailboxID,
	}
	provider := strings.TrimSpace(mailbox.Provider)
	if strings.EqualFold(provider, mailcom.Provider) {
		values, err := integrationcfg.Load(h.DB)
		if err != nil {
			return account, fmt.Errorf("加载 mail.com 设置: %w", err)
		}
		config, err := values.MailCom()
		if err != nil {
			return account, err
		}
		account.MailComOAuthPublicSecret = config.OAuthPublicSecret
		return account, nil
	}
	if !strings.EqualFold(provider, domainmail.Provider) {
		return account, nil
	}
	values, err := integrationcfg.Load(h.DB)
	if err != nil {
		return account, fmt.Errorf("加载域名邮设置: %w", err)
	}
	config, err := values.DomainMail()
	if err != nil {
		return account, err
	}
	account.DomainMailBaseURL = config.Client.BaseURL
	account.DomainMailAPIKey = config.Client.APIKey
	return account, nil
}

func (h *Handler) MailboxOptions(c *gin.Context) {
	var items []struct {
		ID    uint   `json:"id"`
		Email string `json:"email"`
	}
	if err := h.DB.Model(&models.Mailbox{}).Select("id", "email").Where("status = ?", "verified").Order("email, id").Scan(&items).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}

func setMailboxCredentialFlags(mailbox *models.Mailbox) {
	mailbox.PasswordConfigured = strings.TrimSpace(mailbox.Password) != ""
	mailbox.ClientIDConfigured = strings.TrimSpace(mailbox.ClientID) != ""
	mailbox.RefreshTokenConfigured = strings.TrimSpace(mailbox.RefreshToken) != ""
	mailbox.CodeURLConfigured = strings.TrimSpace(mailbox.CodeURL) != ""
}

func (h *Handler) MailboxList(c *gin.Context) {
	var items []models.Mailbox
	q := h.DB.Order("id desc")
	if s := c.Query("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	if s := c.Query("registration_status"); s == "unregistered" {
		q = q.Where("NOT EXISTS (SELECT 1 FROM registrations WHERE registrations.mailbox_id = mailboxes.id OR registrations.email = mailboxes.email)")
	} else if s == "registered" {
		q = q.Where("EXISTS (SELECT 1 FROM registrations WHERE registrations.mailbox_id = mailboxes.id OR registrations.email = mailboxes.email)")
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
		q = q.Where("email LIKE ? OR provider LIKE ? OR note LIKE ?", like, like, like)
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
	q.Model(&models.Mailbox{}).Count(&total)
	if err := q.Preload("Category").Offset((page - 1) * size).Limit(size).Find(&items).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	registerLimit := 1 + h.fissionCount()
	for i := range items {
		setMailboxCredentialFlags(&items[i])
		items[i].RegisterCount = h.mailboxRegisterCount(items[i])
		items[i].RegisterLimit = registerLimit
		if strings.EqualFold(strings.TrimSpace(items[i].Provider), domainmail.Provider) {
			items[i].RegisterLimit = 1
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "total": total, "page": page, "size": size})
}

func (h *Handler) fissionCount() int {
	var s models.Setting
	if err := h.DB.Where("key = ?", "fission_count").First(&s).Error; err != nil {
		return 5
	}
	n, err := strconv.Atoi(strings.TrimSpace(s.Value))
	if err != nil || n < 0 {
		return 5
	}
	return n
}

func (h *Handler) mailboxRegisterCount(m models.Mailbox) int {
	var n int64
	q := h.DB.Model(&models.Registration{}).Where("mailbox_id = ? OR email = ?", m.ID, m.Email)
	for _, pattern := range emailalias.LikePatterns(m.Email) {
		q = q.Or("email LIKE ? ESCAPE '\\'", pattern)
	}
	q.Count(&n)
	return int(n)
}

func (h *Handler) MailboxCreate(c *gin.Context) {
	var in mailboxInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validMailboxStatus(in.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	if !h.categoryExists("mailbox", in.CategoryID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "邮箱分类不存在"})
		return
	}
	codeURL := strings.TrimSpace(in.CodeURL)
	if codeURL != "" {
		if err := mailfetch.ValidateCodeURL(codeURL); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if err := validateMailComInput(in.Provider, in.Email, in.Password, codeURL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.EqualFold(strings.TrimSpace(in.Provider), domainmail.Provider) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "域名邮必须通过 API 生成"})
		return
	}
	provider := strings.TrimSpace(in.Provider)
	if strings.EqualFold(provider, mailcom.Provider) {
		provider = mailcom.Provider
	}
	m := models.Mailbox{
		Email: strings.TrimSpace(in.Email), Password: in.Password, Provider: provider,
		ClientID: strings.TrimSpace(in.ClientID), RefreshToken: strings.TrimSpace(in.RefreshToken), CodeURL: codeURL,
		Status: in.Status, CategoryID: in.CategoryID, Note: in.Note,
	}
	if codeURL != "" {
		m.Provider = "api"
		m.Password, m.ClientID, m.RefreshToken = "", "", ""
	}
	if m.Status == "" {
		m.Status = "unverified"
		if codeURL != "" {
			m.Status = "verified"
		}
	}
	if err := h.DB.Create(&m).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	setMailboxCredentialFlags(&m)
	c.JSON(http.StatusCreated, m)
}

type mailboxImportItem struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	Provider     string `json:"provider"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
	CodeURL      string `json:"code_url"`
}

// MailboxImport 批量导入邮箱，重复 email 自动跳过。
func (h *Handler) MailboxImport(c *gin.Context) {
	var in struct {
		Items      []mailboxImportItem `json:"items" binding:"required"`
		CategoryID *uint               `json:"category_id"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !h.categoryExists("mailbox", in.CategoryID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "邮箱分类不存在或作用域不匹配"})
		return
	}

	added, duplicates, invalid := 0, 0, 0
	seen := map[string]bool{}
	candidates := make([]models.Mailbox, 0, len(in.Items))
	candidateKeys := make([]string, 0, len(in.Items))
	for _, it := range in.Items {
		email := strings.TrimSpace(it.Email)
		key := strings.ToLower(email)
		if email == "" || !strings.Contains(email, "@") {
			invalid++
			continue
		}
		if seen[key] {
			duplicates++
			continue
		}
		seen[key] = true
		provider := strings.TrimSpace(it.Provider)
		if provider == "" && strings.TrimSpace(it.Password) != "" && strings.TrimSpace(it.ClientID) == "" && strings.TrimSpace(it.RefreshToken) == "" && mailcom.IsAddress(email) {
			provider = mailcom.Provider
		}
		codeURL := strings.TrimSpace(it.CodeURL)
		if codeURL != "" && mailfetch.ValidateCodeURL(codeURL) != nil {
			invalid++
			continue
		}
		if validateMailComInput(provider, email, it.Password, codeURL) != nil {
			invalid++
			continue
		}
		m := models.Mailbox{
			Email: email, Password: strings.TrimSpace(it.Password), Provider: provider,
			ClientID: strings.TrimSpace(it.ClientID), RefreshToken: strings.TrimSpace(it.RefreshToken), CodeURL: codeURL,
			Status: "verifying", CategoryID: in.CategoryID,
		}
		if strings.EqualFold(m.Provider, "mailcom") {
			m.Provider = mailcom.Provider
		}
		if codeURL != "" {
			m.Provider = "api"
			m.Status = "verified"
			m.Password, m.ClientID, m.RefreshToken = "", "", ""
		}
		candidates = append(candidates, m)
		candidateKeys = append(candidateKeys, key)
	}
	if len(candidates) > 0 {
		existing := make([]string, 0)
		if err := h.DB.Model(&models.Mailbox{}).Where("LOWER(email) IN ?", candidateKeys).Pluck("email", &existing).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "批量导入失败", "detail": err.Error()})
			return
		}
		existingKeys := make(map[string]struct{}, len(existing))
		for _, email := range existing {
			existingKeys[strings.ToLower(email)] = struct{}{}
		}
		pending := candidates[:0]
		for _, candidate := range candidates {
			if _, exists := existingKeys[strings.ToLower(candidate.Email)]; exists {
				duplicates++
				continue
			}
			pending = append(pending, candidate)
		}
		candidates = pending
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if len(candidates) == 0 {
			return nil
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&candidates, 100)
		if result.Error != nil {
			return result.Error
		}
		added = int(result.RowsAffected)
		duplicates += len(candidates) - added
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "批量导入失败", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"added": added, "skipped": duplicates + invalid, "duplicates": duplicates, "invalid": invalid})
}

// MailboxVerify 校验单个邮箱凭据是否可用，更新状态为 verified / verify_failed。
func (h *Handler) MailboxVerify(c *gin.Context) {
	var m models.Mailbox
	if err := h.DB.First(&m, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邮箱不存在"})
		return
	}
	account, accountErr := h.mailboxAccount(m)
	if accountErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": accountErr.Error()})
		return
	}
	if err := h.Mail.Verify(c.Request.Context(), account); err != nil {
		m.Status = "verify_failed"
		h.DB.Model(&m).Update("status", m.Status)
		c.JSON(http.StatusOK, gin.H{"id": m.ID, "status": m.Status})
		return
	}
	m.Status = "verified"
	h.DB.Model(&m).Update("status", m.Status)
	c.JSON(http.StatusOK, gin.H{"id": m.ID, "status": m.Status})
}

func (h *Handler) MailboxUpdate(c *gin.Context) {
	var m models.Mailbox
	if err := h.DB.First(&m, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var in mailboxInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validMailboxStatus(in.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	if !h.categoryExists("mailbox", in.CategoryID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "邮箱分类不存在"})
		return
	}
	if strings.EqualFold(strings.TrimSpace(m.Provider), domainmail.Provider) {
		if !strings.EqualFold(strings.TrimSpace(in.Email), m.Email) || !strings.EqualFold(strings.TrimSpace(in.Provider), domainmail.Provider) || strings.TrimSpace(in.CodeURL) != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "域名邮地址和服务商由 API 管理"})
			return
		}
		if in.Status != "" {
			m.Status = in.Status
		}
		m.CategoryID = in.CategoryID
		m.Note = in.Note
		if err := h.DB.Save(&m).Error; err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		setMailboxCredentialFlags(&m)
		c.JSON(http.StatusOK, m)
		return
	}
	oldProvider := strings.TrimSpace(m.Provider)
	m.Email = strings.TrimSpace(in.Email)
	provider := strings.TrimSpace(in.Provider)
	mailComProvider := strings.EqualFold(provider, mailcom.Provider)
	if mailComProvider {
		provider = mailcom.Provider
	}
	providerChanged := !strings.EqualFold(oldProvider, provider)
	effectivePassword := strings.TrimSpace(in.Password)
	if effectivePassword == "" && !providerChanged {
		effectivePassword = strings.TrimSpace(m.Password)
	}
	codeURL := strings.TrimSpace(in.CodeURL)
	if err := validateMailComInput(provider, m.Email, effectivePassword, codeURL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.EqualFold(provider, domainmail.Provider) && m.RemoteMailboxID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "域名邮必须通过 API 生成"})
		return
	}
	m.Provider = provider
	if providerChanged {
		m.Password, m.ClientID, m.RefreshToken, m.CodeURL = "", "", "", ""
	}
	if strings.TrimSpace(in.Password) != "" {
		m.Password = in.Password
	}
	if strings.TrimSpace(in.ClientID) != "" {
		m.ClientID = strings.TrimSpace(in.ClientID)
	}
	if strings.TrimSpace(in.RefreshToken) != "" {
		m.RefreshToken = strings.TrimSpace(in.RefreshToken)
	}
	if !strings.EqualFold(provider, domainmail.Provider) {
		m.RemoteMailboxID = ""
	}
	if mailComProvider {
		m.CodeURL = ""
		m.ClientID = ""
		m.RefreshToken = ""
	}
	if codeURL != "" {
		if err := mailfetch.ValidateCodeURL(codeURL); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		m.CodeURL = codeURL
		m.Provider = "api"
		m.Password, m.ClientID, m.RefreshToken = "", "", ""
	}
	if in.Status != "" {
		m.Status = in.Status
	}
	m.CategoryID = in.CategoryID
	m.Note = in.Note
	if err := h.DB.Save(&m).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	setMailboxCredentialFlags(&m)
	c.JSON(http.StatusOK, m)
}

func (h *Handler) MailboxDelete(c *gin.Context) {
	var mailbox models.Mailbox
	if err := h.DB.First(&mailbox, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邮箱不存在"})
		return
	}
	if strings.EqualFold(strings.TrimSpace(mailbox.Provider), domainmail.Provider) {
		account, err := h.mailboxAccount(mailbox)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		client, err := domainmail.New(domainmail.Config{BaseURL: account.DomainMailBaseURL, APIKey: account.DomainMailAPIKey})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := client.DeleteMailbox(c.Request.Context(), mailbox.RemoteMailboxID); err != nil && !errors.Is(err, domainmail.ErrNotFound) {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
	}
	if err := h.DB.Delete(&mailbox).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// MailboxMessages 取件：拉某个邮箱收件箱最新邮件，含完整 HTML 正文，供网页弹窗轮询展示。
func (h *Handler) MailboxMessages(c *gin.Context) {
	var m models.Mailbox
	if err := h.DB.First(&m, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邮箱不存在"})
		return
	}
	setMailboxCredentialFlags(&m)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	account, err := h.mailboxAccount(m)
	if err == nil {
		var msgs []mailfetch.Message
		msgs, err = h.Mail.ListMessages(c.Request.Context(), account, limit)
		if err == nil {
			c.JSON(http.StatusOK, gin.H{"email": m.Email, "items": msgs})
			return
		}
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mailfetch.ErrMissingCreds) || errors.Is(err, mailfetch.ErrAuthFailed) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error(), "email": m.Email})
		return
	}
}

// MailboxMessage 按消息 ID 拉取单封邮件的完整正文，供点击后按需加载。
func (h *Handler) MailboxMessage(c *gin.Context) {
	var m models.Mailbox
	if err := h.DB.First(&m, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邮箱不存在"})
		return
	}
	account, err := h.mailboxAccount(m)
	var msg mailfetch.Message
	if err == nil {
		msg, err = h.Mail.GetMessage(c.Request.Context(), account, c.Query("mid"))
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mailfetch.ErrMissingCreds) || errors.Is(err, mailfetch.ErrAuthFailed) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error(), "email": m.Email})
		return
	}
	c.JSON(http.StatusOK, msg)
}
