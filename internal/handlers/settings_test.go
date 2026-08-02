package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func settingsTestHandler(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Setting{}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: database}
	r := gin.New()
	r.GET("/settings", h.SettingsGet)
	r.PUT("/settings", h.SettingsSave)
	return h, r
}

func TestSettingsSecretsAreWriteOnly(t *testing.T) {
	h, r := settingsTestHandler(t)
	for key, value := range map[string]string{
		"sms_api_key":     "sms-secret",
		"sub2api_api_key": "sub-secret",
		"sub2api_url":     "https://sub.example.test",
	} {
		if err := h.DB.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "sms-secret") || strings.Contains(response.Body.String(), "sub-secret") {
		t.Fatalf("secret leaked: %s", response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["sms_api_key_configured"] != "1" || body["sub2api_api_key_configured"] != "1" {
		t.Fatalf("configured flags=%v", body)
	}
	if body["sub2api_url"] != "https://sub.example.test" {
		t.Fatalf("url=%q", body["sub2api_url"])
	}
}

func putSettings(r *gin.Engine, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(response, request)
	return response
}

func TestSettingsValidationIsAtomic(t *testing.T) {
	h, r := settingsTestHandler(t)
	response := putSettings(r, `{"max_concurrency":"20","sms_api_key":"key","codex_auto_authorize":"1","sms_country":"999"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var count int64
	if err := h.DB.Model(&models.Setting{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("partial settings persisted: %d", count)
	}
}

func TestSettingsDisabledIntegrationsAllowPreconfiguration(t *testing.T) {
	_, r := settingsTestHandler(t)
	for _, body := range []string{
		`{"codex_auto_authorize":"0","sms_platform":"hero-sms","sms_country":"187","sms_max_price":"0.5","sms_timeout":"180"}`,
		`{"sub2api_auto_import":"0","sub2api_url":"https://sub.example.test","sub2api_group_ids":"1,2","sub2api_concurrency":"10","sub2api_priority":"1","sub2api_timeout":"60"}`,
	} {
		response := putSettings(r, body)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestSettingsEnabledIntegrationsRequireSecrets(t *testing.T) {
	_, r := settingsTestHandler(t)
	for _, body := range []string{
		`{"codex_auto_authorize":"1","sms_platform":"hero-sms","sms_country":"187","sms_max_price":"0.5","sms_timeout":"180"}`,
		`{"sub2api_auto_import":"1","sub2api_url":"https://sub.example.test","sub2api_concurrency":"10","sub2api_priority":"1","sub2api_timeout":"60"}`,
	} {
		response := putSettings(r, body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestSettingsBlankSecretPreservesStoredValue(t *testing.T) {
	h, r := settingsTestHandler(t)
	if err := h.DB.Create(&models.Setting{Key: "sms_api_key", Value: "stored-secret"}).Error; err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(`{"sms_api_key":"  ","sms_platform":"hero-sms"}`))
	request.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var secret models.Setting
	if err := h.DB.First(&secret, "key = ?", "sms_api_key").Error; err != nil {
		t.Fatal(err)
	}
	if secret.Value != "stored-secret" {
		t.Fatalf("secret=%q", secret.Value)
	}
	var platform models.Setting
	if err := h.DB.First(&platform, "key = ?", "sms_platform").Error; err != nil {
		t.Fatal(err)
	}
	if platform.Value != "hero-sms" {
		t.Fatalf("platform=%q", platform.Value)
	}
}
