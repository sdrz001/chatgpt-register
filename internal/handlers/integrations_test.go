package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/producer"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func integrationHandlerTestRouter(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Registration{}, &models.SMSActivation{}, &models.Mailbox{}, &models.Setting{}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{DB: database, Producer: producer.New(database, mailfetch.New())}
	router := gin.New()
	router.GET("/registrations", handler.List)
	router.GET("/registrations/:id", handler.Get)
	router.GET("/sms-platform/meta", handler.SMSPlatformMeta)
	router.POST("/registrations/:id/codex-authorize", handler.RegistrationCodexAuthorize)
	router.POST("/registrations/:id/sub2api-import", handler.RegistrationSub2APIImport)
	return handler, router
}

func TestSMSPlatformMetaContainsPublicCatalogOnly(t *testing.T) {
	_, router := integrationHandlerTestRouter(t)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sms-platform/meta", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(strings.ToLower(response.Body.String()), "api_key") {
		t.Fatalf("meta leaked secret field: %s", response.Body.String())
	}
	var body struct {
		Service                string           `json:"service"`
		ServiceLabel           string           `json:"service_label"`
		MinimumPhoneAttempts   int              `json:"minimum_phone_attempts"`
		DefaultRandomCountries string           `json:"default_random_countries"`
		DefaultPhoneAttempts   int              `json:"default_phone_attempts"`
		MaximumPhoneAttempts   int              `json:"maximum_phone_attempts"`
		Countries              []map[string]any `json:"countries"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Service != "dr" || body.ServiceLabel != "OpenAI" || body.DefaultRandomCountries != "187,16,36,43" || body.MinimumPhoneAttempts != 1 || body.DefaultPhoneAttempts != 3 || body.MaximumPhoneAttempts != 10 || len(body.Countries) < 190 {
		t.Fatalf("meta=%+v", body)
	}
}

func TestRegistrationResponsesHidePasswordAndOAuthData(t *testing.T) {
	handler, router := integrationHandlerTestRouter(t)
	registration := models.Registration{
		Email: "user@example.test", Password: "account-password", Status: "registered",
		AuthData:    `{"access_token":"access","refresh_token":"refresh","id_token":"id"}`,
		CodexStatus: "authorized", Sub2APIStatus: "not_imported",
	}
	if err := handler.DB.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/registrations", "/registrations/" + strconv.FormatUint(uint64(registration.ID), 10)} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		for _, secret := range []string{"account-password", "access", "refresh", "\"id_token\""} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("%s leaked %q: %s", path, secret, response.Body.String())
			}
		}
	}
}

func TestIntegrationActionsRejectUnregisteredAccount(t *testing.T) {
	handler, router := integrationHandlerTestRouter(t)
	registration := models.Registration{Email: "pending@example.test", Password: "password", Status: "pending"}
	if err := handler.DB.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatUint(uint64(registration.ID), 10)
	for _, path := range []string{
		"/registrations/" + id + "/codex-authorize",
		"/registrations/" + id + "/sub2api-import",
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "尚未注册成功") {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}
