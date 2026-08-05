package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"chatgpt-register/internal/emailalias"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
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

func mailboxAccount(mailbox models.Mailbox) mailfetch.Account {
	return mailfetch.Account{
		Email: mailbox.Email, Provider: mailbox.Provider, ClientID: mailbox.ClientID,
		RefreshToken: mailbox.RefreshToken, CodeURL: mailbox.CodeURL,
	}
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

func (h *Handler) MailboxList(c *gin.Context) {
	var items []models.Mailbox
	q := h.DB.Order("id desc")
	if s := c.Query("status"); s != "" {
		q = q.Where("status = ?", s)
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
		items[i].CodeURLConfigured = strings.TrimSpace(items[i].CodeURL) != ""
		items[i].RegisterCount = h.mailboxRegisterCount(items[i])
		items[i].RegisterLimit = registerLimit
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
	m := models.Mailbox{
		Email: strings.TrimSpace(in.Email), Password: in.Password, Provider: strings.TrimSpace(in.Provider),
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
	m.CodeURLConfigured = m.CodeURL != ""
	c.JSON(http.StatusCreated, m)
}

type mailboxImportItem struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
	CodeURL      string `json:"code_url"`
}

// MailboxImport 批量导入邮箱，重复 email 自动跳过。
func (h *Handler) MailboxImport(c *gin.Context) {
	var in struct {
		Items []mailboxImportItem `json:"items" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	added, skipped := 0, 0
	seen := map[string]bool{}
	for _, it := range in.Items {
		email := strings.TrimSpace(it.Email)
		if email == "" || !strings.Contains(email, "@") || seen[email] {
			skipped++
			continue
		}
		seen[email] = true
		var count int64
		h.DB.Model(&models.Mailbox{}).Where("email = ?", email).Count(&count)
		if count > 0 {
			skipped++
			continue
		}
		codeURL := strings.TrimSpace(it.CodeURL)
		if codeURL != "" && mailfetch.ValidateCodeURL(codeURL) != nil {
			skipped++
			continue
		}
		m := models.Mailbox{
			Email: email, Password: strings.TrimSpace(it.Password), ClientID: strings.TrimSpace(it.ClientID),
			RefreshToken: strings.TrimSpace(it.RefreshToken), CodeURL: codeURL, Status: "verifying",
		}
		if codeURL != "" {
			m.Provider = "api"
			m.Status = "verified"
			m.Password, m.ClientID, m.RefreshToken = "", "", ""
		}
		if err := h.DB.Create(&m).Error; err != nil {
			skipped++
			continue
		}
		added++
	}
	c.JSON(http.StatusOK, gin.H{"added": added, "skipped": skipped})
}

// MailboxVerify 校验单个邮箱凭据是否可用，更新状态为 verified / verify_failed。
func (h *Handler) MailboxVerify(c *gin.Context) {
	var m models.Mailbox
	if err := h.DB.First(&m, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邮箱不存在"})
		return
	}
	err := h.Mail.Verify(c.Request.Context(), mailboxAccount(m))
	if err != nil {
		m.Status = "verify_failed"
	} else {
		m.Status = "verified"
	}
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
	m.Email = strings.TrimSpace(in.Email)
	m.Password = in.Password
	m.Provider = strings.TrimSpace(in.Provider)
	m.ClientID = strings.TrimSpace(in.ClientID)
	m.RefreshToken = strings.TrimSpace(in.RefreshToken)
	if codeURL := strings.TrimSpace(in.CodeURL); codeURL != "" {
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
	m.CodeURLConfigured = m.CodeURL != ""
	c.JSON(http.StatusOK, m)
}

func (h *Handler) MailboxDelete(c *gin.Context) {
	if err := h.DB.Delete(&models.Mailbox{}, c.Param("id")).Error; err != nil {
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
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	msgs, err := h.Mail.ListMessages(c.Request.Context(), mailboxAccount(m), limit)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mailfetch.ErrMissingCreds) || errors.Is(err, mailfetch.ErrAuthFailed) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error(), "email": m.Email})
		return
	}
	c.JSON(http.StatusOK, gin.H{"email": m.Email, "items": msgs})
}

// MailboxMessage 按消息 ID 拉取单封邮件的完整正文，供点击后按需加载。
func (h *Handler) MailboxMessage(c *gin.Context) {
	var m models.Mailbox
	if err := h.DB.First(&m, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邮箱不存在"})
		return
	}
	msg, err := h.Mail.GetMessage(c.Request.Context(), mailboxAccount(m), c.Query("mid"))
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
