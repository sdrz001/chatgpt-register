package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"chatgpt-register/internal/domainmail"
	"chatgpt-register/internal/integrationcfg"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

type domainMailConfigInput struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
}

type domainMailGenerateInput struct {
	Count      int    `json:"count"`
	NamePrefix string `json:"name_prefix"`
	Domain     string `json:"domain"`
	ExpiryTime *int64 `json:"expiry_time"`
	CategoryID *uint  `json:"category_id"`
}

type domainMailGenerateItem struct {
	Email string `json:"email,omitempty"`
	ID    uint   `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

func (h *Handler) DomainMailConfig(c *gin.Context) {
	var input domainMailConfigInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	clientConfig, err := h.domainMailClientConfig(input.URL, input.APIKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	client, err := domainmail.New(clientConfig)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	remote, err := client.SystemConfig(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"domains": remote.Domains})
}

func (h *Handler) MailboxGenerate(c *gin.Context) {
	var input domainMailGenerateInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if input.Count < 1 || input.Count > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "生成数量必须在 1 到 100 之间"})
		return
	}
	if !h.categoryExists("mailbox", input.CategoryID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "邮箱分类不存在"})
		return
	}
	config, err := h.domainMailConfig()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if config.Source != domainmail.Provider {
		c.JSON(http.StatusBadRequest, gin.H{"error": "当前邮箱来源不是域名邮 API"})
		return
	}
	domain := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(input.Domain, "@")))
	if domain == "" {
		domain = config.Domain
	}
	if !strings.EqualFold(domain, config.Domain) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "邮箱域名与系统设置不一致"})
		return
	}
	expiryTime := config.ExpiryTime
	if input.ExpiryTime != nil {
		expiryTime = *input.ExpiryTime
	}
	if domain == "" || strings.ContainsAny(domain, " /@") || !domainmail.ValidExpiryTime(expiryTime) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "域名或有效期配置无效"})
		return
	}
	client, err := domainmail.New(config.Client)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	prefix := normalizeMailboxPrefix(input.NamePrefix)
	items := make([]domainMailGenerateItem, 0, input.Count)
	added, failed := 0, 0
	for range input.Count {
		name, nameErr := generatedMailboxName(prefix)
		if nameErr != nil {
			items = append(items, domainMailGenerateItem{Error: nameErr.Error()})
			failed++
			continue
		}
		remote, generateErr := client.Generate(c.Request.Context(), domainmail.GenerateRequest{
			Name: name, Domain: domain, ExpiryTime: expiryTime,
		})
		if generateErr != nil {
			items = append(items, domainMailGenerateItem{Email: name + "@" + domain, Error: generateErr.Error()})
			failed++
			if c.Request.Context().Err() != nil {
				break
			}
			continue
		}
		mailbox := models.Mailbox{
			Email: remote.Email, Provider: domainmail.Provider, RemoteMailboxID: remote.ID,
			Status: "verified", CategoryID: input.CategoryID, Note: "域名邮 API 生成",
		}
		if createErr := h.DB.Create(&mailbox).Error; createErr != nil {
			errorMessage := createErr.Error()
			if rollbackErr := client.DeleteMailbox(c.Request.Context(), remote.ID); rollbackErr != nil {
				errorMessage += "; 远端回滚失败: " + rollbackErr.Error()
			}
			items = append(items, domainMailGenerateItem{Email: remote.Email, Error: errorMessage})
			failed++
			continue
		}
		items = append(items, domainMailGenerateItem{Email: mailbox.Email, ID: mailbox.ID})
		added++
	}
	status := http.StatusOK
	if added == 0 {
		status = http.StatusBadGateway
	}
	c.JSON(status, gin.H{"added": added, "failed": failed, "items": items})
}

func (h *Handler) domainMailClientConfig(urlOverride, keyOverride string) (domainmail.Config, error) {
	values, err := integrationcfg.Load(h.DB)
	if err != nil {
		return domainmail.Config{}, err
	}
	baseURL := strings.TrimSpace(urlOverride)
	if baseURL == "" {
		baseURL = strings.TrimSpace(values["domain_mail_url"])
	}
	apiKey := strings.TrimSpace(keyOverride)
	if apiKey == "" {
		apiKey = strings.TrimSpace(values["domain_mail_api_key"])
	}
	config := domainmail.Config{BaseURL: baseURL, APIKey: apiKey}
	if _, err := domainmail.New(config); err != nil {
		return domainmail.Config{}, err
	}
	return config, nil
}

func (h *Handler) domainMailConfig() (integrationcfg.DomainMailConfig, error) {
	values, err := integrationcfg.Load(h.DB)
	if err != nil {
		return integrationcfg.DomainMailConfig{}, err
	}
	return values.DomainMail()
}

func normalizeMailboxPrefix(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		}
	}
	prefix := builder.String()
	if prefix == "" {
		prefix = "gpt"
	}
	if len(prefix) > 24 {
		prefix = prefix[:24]
	}
	return prefix
}

func generatedMailboxName(prefix string) (string, error) {
	bytes := make([]byte, 6)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("生成邮箱名称失败: %w", err)
	}
	return prefix + hex.EncodeToString(bytes), nil
}
