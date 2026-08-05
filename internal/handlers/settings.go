package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"chatgpt-register/internal/integrationcfg"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 内部保留 key（如 JWT 密钥），不允许通过设置接口读取或修改
var reservedSettingKeys = map[string]bool{
	"jwt_secret":                 true,
	"sms_api_key_configured":     true,
	"sub2api_api_key_configured": true,
}

var secretSettingKeys = map[string]string{
	"sms_api_key":     "sms_api_key_configured",
	"sub2api_api_key": "sub2api_api_key_configured",
}

var allowedSettingKeys = map[string]bool{
	"max_concurrency": true, "fission_count": true, "headless": true, "at_auto_check": true,
	"browser_backend": true, "python_executable": true,
	"proxy_enabled": true, "proxy_list": true,
	"codex_auto_authorize": true, "sms_platform": true, "sms_api_key": true,
	"sms_country": true, "sms_random_countries": true, "sms_max_price": true, "sms_timeout": true, "sms_phone_attempts": true,
	"sub2api_auto_import": true, "sub2api_url": true, "sub2api_api_key": true,
	"sub2api_group_ids": true, "sub2api_concurrency": true, "sub2api_priority": true, "sub2api_timeout": true,
}

func (h *Handler) SettingsGet(c *gin.Context) {
	var items []models.Setting
	if err := h.DB.Find(&items).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := map[string]string{}
	for _, s := range items {
		if reservedSettingKeys[s.Key] {
			continue
		}
		if configuredKey, secret := secretSettingKeys[s.Key]; secret {
			if strings.TrimSpace(s.Value) != "" {
				out[configuredKey] = "1"
			} else {
				out[configuredKey] = "0"
			}
			continue
		}
		out[s.Key] = s.Value
	}
	for _, configuredKey := range secretSettingKeys {
		if _, exists := out[configuredKey]; !exists {
			out[configuredKey] = "0"
		}
	}
	if _, exists := out["at_auto_check"]; !exists {
		out["at_auto_check"] = "1"
	}
	if strings.TrimSpace(out["browser_backend"]) == "" {
		out["browser_backend"] = "rod"
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) SettingsSave(c *gin.Context) {
	var in map[string]string
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	values, err := integrationcfg.Load(h.DB)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	updates := make(map[string]string, len(in))
	for key, value := range in {
		if reservedSettingKeys[key] || !allowedSettingKeys[key] {
			continue
		}
		if key != "python_executable" {
			value = strings.TrimSpace(value)
		}
		if _, secret := secretSettingKeys[key]; secret && value == "" {
			continue
		}
		updates[key] = value
		values[key] = value
	}
	if err := validateSettings(values, updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.DB.Transaction(func(tx *gorm.DB) error {
		for key, value := range updates {
			setting := models.Setting{Key: key, Value: value}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
			}).Create(&setting).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if value, changed := updates["at_auto_check"]; changed {
		h.setAutoATCheck(value == "1")
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func validateSettings(values integrationcfg.Values, updates map[string]string) error {
	for _, key := range []string{"headless", "proxy_enabled", "at_auto_check", "codex_auto_authorize", "sub2api_auto_import"} {
		if value, changed := updates[key]; changed && value != "0" && value != "1" {
			return fmt.Errorf("%s 必须是 0 或 1", key)
		}
	}
	if backend, changed := updates["browser_backend"]; changed && backend != "rod" && backend != "cloakbrowser" {
		return fmt.Errorf("browser_backend 必须是 rod 或 cloakbrowser")
	}
	if python, changed := updates["python_executable"]; changed {
		if len(python) > 1024 {
			return fmt.Errorf("python_executable 最长 1024 个字符")
		}
		if strings.ContainsAny(python, "\r\n\x00") {
			return fmt.Errorf("python_executable 不得包含 CR、LF 或 NUL")
		}
	}
	for _, setting := range []struct {
		key      string
		fallback int
		minimum  int
		maximum  int
		label    string
	}{
		{"max_concurrency", 10, 1, 100, "最大并发数"},
		{"fission_count", 5, 0, 100, "裂变数量"},
	} {
		if _, changed := updates[setting.key]; !changed {
			continue
		}
		value := setting.fallback
		if strings.TrimSpace(values[setting.key]) != "" {
			parsed, err := strconv.Atoi(values[setting.key])
			if err != nil {
				return fmt.Errorf("%s必须是整数", setting.label)
			}
			value = parsed
		}
		if value < setting.minimum || value > setting.maximum {
			return fmt.Errorf("%s必须在 %d 到 %d 之间", setting.label, setting.minimum, setting.maximum)
		}
	}
	if touchesAny(updates, "codex_auto_authorize", "sms_platform", "sms_api_key", "sms_country", "sms_random_countries", "sms_max_price", "sms_timeout", "sms_phone_attempts") {
		smsValues := cloneSettings(values)
		if !values.CodexAutoAuthorize() && strings.TrimSpace(smsValues["sms_api_key"]) == "" {
			smsValues["sms_api_key"] = "not-enabled"
		}
		if _, err := smsValues.SMS(); err != nil {
			return err
		}
	}
	if touchesAny(updates, "sub2api_auto_import", "sub2api_url", "sub2api_api_key", "sub2api_group_ids", "sub2api_concurrency", "sub2api_priority", "sub2api_timeout") {
		subValues := cloneSettings(values)
		if !values.Sub2APIAutoImport() {
			if strings.TrimSpace(subValues["sub2api_url"]) == "" {
				subValues["sub2api_url"] = "https://not-enabled.invalid"
			}
			if strings.TrimSpace(subValues["sub2api_api_key"]) == "" {
				subValues["sub2api_api_key"] = "not-enabled"
			}
		}
		if _, err := subValues.Sub2API(); err != nil {
			return err
		}
	}
	return nil
}

func cloneSettings(values integrationcfg.Values) integrationcfg.Values {
	clone := make(integrationcfg.Values, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func touchesAny(values map[string]string, keys ...string) bool {
	for _, key := range keys {
		if _, exists := values[key]; exists {
			return true
		}
	}
	return false
}
