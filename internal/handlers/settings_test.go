package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func settingsTestHandler(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "settings.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Setting{}, &models.ProxyPool{}, &models.Category{}, &models.Registration{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	h := &Handler{DB: database}
	r := gin.New()
	r.GET("/settings", h.SettingsGet)
	r.PUT("/settings", h.SettingsSave)
	r.GET("/proxy-pools", h.ProxyPoolList)
	r.GET("/proxy-pools/:id", h.ProxyPoolGet)
	r.POST("/proxy-pools", h.ProxyPoolCreate)
	r.PUT("/proxy-pools/:id", h.ProxyPoolUpdate)
	r.DELETE("/proxy-pools/:id", h.ProxyPoolDelete)
	r.PUT("/proxy-pools/default", h.ProxyPoolSetDefault)
	return h, r
}

func TestSettingsSecretsAreWriteOnly(t *testing.T) {
	h, r := settingsTestHandler(t)
	for key, value := range map[string]string{
		"sms_api_key":         "sms-secret",
		"sub2api_api_key":     "sub-secret",
		"sub2api_url":         "https://sub.example.test",
		"domain_mail_api_key": "domain-secret",
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
	if strings.Contains(response.Body.String(), "sms-secret") || strings.Contains(response.Body.String(), "sub-secret") || strings.Contains(response.Body.String(), "domain-secret") {
		t.Fatalf("secret leaked: %s", response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["sms_api_key_configured"] != "1" || body["sub2api_api_key_configured"] != "1" || body["domain_mail_api_key_configured"] != "1" {
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

func TestSettingsHidesAndIgnoresLegacyProxyKeys(t *testing.T) {
	handler, router := settingsTestHandler(t)
	if err := handler.DB.Create(&[]models.Setting{
		{Key: "proxy_enabled", Value: "1"},
		{Key: "proxy_list", Value: "user:secret@proxy.test:8080"},
	}).Error; err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "proxy_list") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("legacy proxy leaked: status=%d body=%s", response.Code, response.Body.String())
	}
	response = putSettings(router, `{"proxy_enabled":"0","proxy_list":"replacement.test:8080"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var setting models.Setting
	if err := handler.DB.First(&setting, "key = ?", "proxy_list").Error; err != nil || setting.Value != "user:secret@proxy.test:8080" {
		t.Fatalf("setting=%+v error=%v", setting, err)
	}
}

func TestSettingsDefaultBrowserBackendIsRod(t *testing.T) {
	for name, seedEmpty := range map[string]bool{"missing": false, "empty": true} {
		t.Run(name, func(t *testing.T) {
			handler, router := settingsTestHandler(t)
			if seedEmpty {
				if err := handler.DB.Create(&models.Setting{Key: "browser_backend", Value: ""}).Error; err != nil {
					t.Fatal(err)
				}
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["browser_backend"] != "rod" {
				t.Fatalf("browser_backend=%q", body["browser_backend"])
			}
		})
	}
}

func TestSettingsSaveBrowserBackend(t *testing.T) {
	handler, router := settingsTestHandler(t)
	response := putSettings(router, `{"browser_backend":"cloakbrowser","python_executable":"C:\\Python\\python.exe"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for key, want := range map[string]string{
		"browser_backend":   "cloakbrowser",
		"python_executable": `C:\Python\python.exe`,
	} {
		var setting models.Setting
		if err := handler.DB.First(&setting, "key = ?", key).Error; err != nil {
			t.Fatal(err)
		}
		if setting.Value != want {
			t.Fatalf("%s=%q want %q", key, setting.Value, want)
		}
	}
}

func TestSettingsRejectInvalidBrowserBackendAtomically(t *testing.T) {
	handler, router := settingsTestHandler(t)
	response := putSettings(router, `{"max_concurrency":"20","browser_backend":"other"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "browser_backend") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var count int64
	if err := handler.DB.Model(&models.Setting{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("partial settings persisted: count=%d error=%v", count, err)
	}
}

func TestSettingsRejectInvalidPythonExecutableAtomically(t *testing.T) {
	for name, value := range map[string]string{
		"cr":       "python\r.exe",
		"lf":       "python\n.exe",
		"nul":      "python\x00.exe",
		"too long": strings.Repeat("p", 1025),
	} {
		t.Run(name, func(t *testing.T) {
			handler, router := settingsTestHandler(t)
			body, err := json.Marshal(map[string]string{"max_concurrency": "20", "python_executable": value})
			if err != nil {
				t.Fatal(err)
			}
			response := putSettings(router, string(body))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "python_executable") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var count int64
			if err := handler.DB.Model(&models.Setting{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("partial settings persisted: count=%d error=%v", count, err)
			}
		})
	}
}

func TestSettingsDefaultATAutoCheckIsEnabled(t *testing.T) {
	_, router := settingsTestHandler(t)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["at_auto_check"] != "1" {
		t.Fatalf("at_auto_check=%q", body["at_auto_check"])
	}
}

func TestSettingsDisableAutoATCheckKeepsManualCheckAvailable(t *testing.T) {
	handler, router := settingsTestHandler(t)
	handler.setAutoATCheck(true)
	token := registrationTestJWT(t, "free", time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accounts":{"account-id":{"account":{"plan_type":"plus"}}}}`))
	}))
	defer server.Close()
	handler.ATCheckURL = server.URL
	handler.ATCheckClient = func(string) (*http.Client, error) { return server.Client(), nil }
	registration := models.Registration{
		Email: "manual-check@example.test", Status: "registered", ATStatus: "unchecked",
		AccountID: "account-id", AuthData: `{"access_token":"` + token + `"}`,
	}
	if err := handler.DB.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}

	response := putSettings(router, `{"at_auto_check":"0"}`)
	if response.Code != http.StatusOK || handler.autoATCheck.Load() {
		t.Fatalf("status=%d enabled=%v body=%s", response.Code, handler.autoATCheck.Load(), response.Body.String())
	}
	if handler.scheduleATCheck(registration, false) {
		t.Fatal("automatic check scheduled while disabled")
	}
	if !handler.scheduleATCheck(registration, true) {
		t.Fatal("manual check was not scheduled")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := handler.DB.First(&registration, registration.ID).Error; err != nil {
			t.Fatal(err)
		}
		if registration.ATStatus == "valid" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if registration.ATStatus != "valid" || registration.PlanType != "plus" {
		t.Fatalf("registration=%+v", registration)
	}
	var setting models.Setting
	if err := handler.DB.First(&setting, "key = ?", "at_auto_check").Error; err != nil || setting.Value != "0" {
		t.Fatalf("setting=%+v error=%v", setting, err)
	}
}

func TestSettingsDisableAutoATCheckCancelsActiveAutomaticCheck(t *testing.T) {
	handler, _ := settingsTestHandler(t)
	token := registrationTestJWT(t, "free", time.Now().Add(time.Hour))
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	handler.ATCheckURL = server.URL
	handler.ATCheckClient = func(string) (*http.Client, error) { return server.Client(), nil }
	handler.autoATCheck.Store(true)
	registration := models.Registration{
		Email: "cancel-auto@example.test", Status: "registered", ATStatus: "valid",
		AuthData: `{"access_token":"` + token + `"}`,
	}
	if err := handler.DB.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}
	if !handler.scheduleATCheck(registration, false) {
		t.Fatal("automatic check was not scheduled")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("automatic request did not start")
	}
	handler.setAutoATCheck(false)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("automatic request was not canceled")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := handler.DB.First(&registration, registration.ID).Error; err != nil {
			t.Fatal(err)
		}
		if registration.ATStatus == "valid" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status=%q", registration.ATStatus)
}

func TestSettingsRejectInvalidATAutoCheckWithoutChangingRuntime(t *testing.T) {
	handler, router := settingsTestHandler(t)
	handler.setAutoATCheck(true)
	response := putSettings(router, `{"at_auto_check":"yes"}`)
	if response.Code != http.StatusBadRequest || !handler.autoATCheck.Load() {
		t.Fatalf("status=%d enabled=%v body=%s", response.Code, handler.autoATCheck.Load(), response.Body.String())
	}
	var count int64
	if err := handler.DB.Model(&models.Setting{}).Where("key = ?", "at_auto_check").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("count=%d error=%v", count, err)
	}
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

func TestSettingsAllowZeroFissionCount(t *testing.T) {
	h, r := settingsTestHandler(t)
	response := putSettings(r, `{"fission_count":"0"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var setting models.Setting
	if err := h.DB.First(&setting, "key = ?", "fission_count").Error; err != nil {
		t.Fatal(err)
	}
	if setting.Value != "0" {
		t.Fatalf("fission_count=%q", setting.Value)
	}
	response = putSettings(r, `{"fission_count":"-1"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "0 到 100") {
		t.Fatalf("negative status=%d body=%s", response.Code, response.Body.String())
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

func TestSettingsRejectInvalidSMSPhoneAttempts(t *testing.T) {
	h, r := settingsTestHandler(t)
	response := putSettings(r, `{"codex_auto_authorize":"0","sms_platform":"hero-sms","sms_country":"187","sms_phone_attempts":"11"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "号码尝试上限") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var count int64
	if err := h.DB.Model(&models.Setting{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("partial settings persisted: count=%d error=%v", count, err)
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
