package handlers

import (
	"net/http"
	"strings"
	"time"

	"chatgpt-register/internal/models"
	"chatgpt-register/internal/smsactivate"

	"github.com/gin-gonic/gin"
)

func (h *Handler) SMSPlatformMeta(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"platform": "hero-sms",
		"platforms": []gin.H{
			{"id": "hero-sms", "label": "Hero SMS"},
			{"id": "smsbower", "label": "SMSBower"},
		},
		"service":           smsactivate.ServiceOpenAI,
		"default_country":   "random",
		"default_max_price": 0.5,
		"max_price_limit":   smsactivate.MaxPriceLimit,
		"countries":         smsactivate.Countries(),
	})
}

func (h *Handler) SMSPlatformBalance(c *gin.Context) {
	var in struct {
		Platform string `json:"platform"`
		APIKey   string `json:"api_key"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	apiKey := strings.TrimSpace(in.APIKey)
	if apiKey == "" {
		var setting models.Setting
		if err := h.DB.First(&setting, "key = ?", "sms_api_key").Error; err == nil {
			apiKey = strings.TrimSpace(setting.Value)
		}
	}
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写或先保存 SMS API Key"})
		return
	}
	platform := smsactivate.Platform(strings.TrimSpace(in.Platform))
	if platform == "" {
		platform = smsactivate.PlatformHeroSMS
	}
	client, err := smsactivate.New(smsactivate.Config{
		Platform: platform,
		APIKey:   apiKey,
		Country:  187,
		MaxPrice: 0.5,
		Timeout:  15 * time.Second,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	balance, err := client.GetBalance(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "balance": balance, "platform": platform})
}
