package handlers

import (
	"net/http"
	"strings"

	"chatgpt-register/internal/integrationcfg"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/sub2api"

	"github.com/gin-gonic/gin"
)

func (h *Handler) Sub2APIGroups(c *gin.Context) {
	var in struct {
		URL    string `json:"url"`
		APIKey string `json:"api_key"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	values, err := integrationcfg.Load(h.DB)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	config, err := values.Sub2API(strings.TrimSpace(in.URL), strings.TrimSpace(in.APIKey))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	groups, err := sub2api.ListGroups(c.Request.Context(), config)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "groups": groups})
}

func (h *Handler) RegistrationCodexAuthorize(c *gin.Context) {
	var registration models.Registration
	if err := h.DB.Select("id", "status").First(&registration, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	tokens, err := h.Producer.AuthorizeCodex(c.Request.Context(), registration.ID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok": true, "status": "authorized", "email": tokens.Email,
		"expires_in": tokens.ExpiresIn, "account_id": tokens.ChatGPTAccountID,
	})
}

func (h *Handler) RegistrationSub2APIImport(c *gin.Context) {
	var registration models.Registration
	if err := h.DB.Select("id", "status").First(&registration, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	result, err := h.Producer.ImportSub2API(c.Request.Context(), registration.ID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}
