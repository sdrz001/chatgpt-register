package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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
	if err := database.AutoMigrate(&models.Category{}, &models.Registration{}, &models.SMSActivation{}, &models.Mailbox{}, &models.Setting{}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{DB: database, Producer: producer.New(database, mailfetch.New())}
	router := gin.New()
	router.GET("/registrations", handler.List)
	router.GET("/registrations/:id", handler.Get)
	router.GET("/sms-platform/meta", handler.SMSPlatformMeta)
	router.POST("/registrations/:id/codex-authorize", handler.RegistrationCodexAuthorize)
	router.POST("/registrations/:id/sub2api-import", handler.RegistrationSub2APIImport)
	router.POST("/registrations/at-check", handler.RegistrationATCheck)
	router.GET("/categories", handler.CategoryList)
	router.POST("/categories", handler.CategoryCreate)
	router.PUT("/categories/:id", handler.CategoryUpdate)
	router.DELETE("/categories/:id", handler.CategoryDelete)
	router.POST("/categories/assign", handler.CategoryAssign)
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

func registrationTestJWT(t *testing.T, plan string, expiresAt time.Time) string {
	t.Helper()
	claims := map[string]any{
		"exp":                         expiresAt.Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": plan, "chatgpt_account_id": "account-id"},
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func TestCheckRegistrationATUsesOnlinePlanAndRejectsExpiredToken(t *testing.T) {
	token := registrationTestJWT(t, "free", time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("ChatGPT-Account-ID") != "account-id" {
			t.Errorf("headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"accounts":{"account-id":{"account":{"plan_type":"self_serve_business_usage_based","is_default":true}}}}`)
	}))
	defer server.Close()
	handler := &Handler{ATCheckURL: server.URL, ATCheckClient: func(string) (*http.Client, error) { return server.Client(), nil }}
	result := handler.checkRegistrationAT(context.Background(), models.Registration{
		Email: "user@example.test", AccountID: "account-id", AuthData: `{"access_token":"` + token + `"}`,
	})
	if result.Status != "valid" || result.PlanType != "business" || result.ExpiresAt == nil || result.Error != "" {
		t.Fatalf("result=%+v", result)
	}

	expired := registrationTestJWT(t, "plus", time.Now().Add(-time.Minute))
	result = handler.checkRegistrationAT(context.Background(), models.Registration{AuthData: `{"access_token":"` + expired + `"}`})
	if result.Status != "invalid" || result.PlanType != "plus" || !strings.Contains(result.Error, "过期") {
		t.Fatalf("expired result=%+v", result)
	}
}

func TestAuthDataWithPlanUpdatesExportedCredentials(t *testing.T) {
	updated := authDataWithPlan(`{"plan_type":"free","credentials":{"access_token":"secret","plan_type":"free"}}`, "plus")
	var root map[string]any
	if err := json.Unmarshal([]byte(updated), &root); err != nil {
		t.Fatal(err)
	}
	credentials, _ := root["credentials"].(map[string]any)
	if root["plan_type"] != "plus" || credentials["plan_type"] != "plus" || credentials["access_token"] != "secret" {
		t.Fatalf("auth=%v", root)
	}
}

func TestPlanTypeFromAccountsResponseSkipsExpiredWorkspace(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	body := []byte(fmt.Sprintf(`{"accounts":{"expired-business":{"account":{"plan_type":"self_serve_business_usage_based","is_default":true},"entitlement":{"expires_at":%q}},"personal":{"account":{"plan_type":"free"}}}}`, expired))
	if plan := planTypeFromAccountsResponse(body, "expired-business"); plan != "free" {
		t.Fatalf("plan=%q", plan)
	}
}

func TestCheckRegistrationATSeparatesRejectionFromGatewayError(t *testing.T) {
	token := registrationTestJWT(t, "plus", time.Now().Add(time.Hour))
	for _, test := range []struct {
		name, contentType, wantStatus string
		code                          int
	}{
		{name: "unauthorized", code: http.StatusUnauthorized, contentType: "application/json", wantStatus: "invalid"},
		{name: "cloudflare", code: http.StatusForbidden, contentType: "text/html", wantStatus: "error"},
		{name: "rate limit", code: http.StatusTooManyRequests, contentType: "application/json", wantStatus: "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.code)
			}))
			defer server.Close()
			handler := &Handler{ATCheckURL: server.URL, ATCheckClient: func(string) (*http.Client, error) { return server.Client(), nil }}
			result := handler.checkRegistrationAT(context.Background(), models.Registration{AuthData: `{"access_token":"` + token + `"}`})
			if result.Status != test.wantStatus {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestCategoryLifecycleAssignsAndPreservesAccounts(t *testing.T) {
	handler, router := integrationHandlerTestRouter(t)
	registration := models.Registration{Email: "category@example.test", Status: "registered"}
	if err := handler.DB.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/categories", strings.NewReader(`{"scope":"account","name":"Plus 库存"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(create, request)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var category models.Category
	if err := json.Unmarshal(create.Body.Bytes(), &category); err != nil {
		t.Fatal(err)
	}
	assign := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/categories/assign", strings.NewReader(fmt.Sprintf(`{"scope":"account","ids":[%d],"category_id":%d}`, registration.ID, category.ID)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(assign, request)
	if assign.Code != http.StatusOK {
		t.Fatalf("assign status=%d body=%s", assign.Code, assign.Body.String())
	}
	list := httptest.NewRecorder()
	router.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/registrations?category_id="+strconv.FormatUint(uint64(category.ID), 10), nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "Plus 库存") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	deleted := httptest.NewRecorder()
	router.ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/categories/"+strconv.FormatUint(uint64(category.ID), 10), nil))
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if err := handler.DB.First(&registration, registration.ID).Error; err != nil || registration.CategoryID != nil {
		t.Fatalf("registration=%+v error=%v", registration, err)
	}
}

func TestCategoryScopeCannotBeCrossAssigned(t *testing.T) {
	handler, router := integrationHandlerTestRouter(t)
	category := models.Category{Scope: "mailbox", Name: "邮箱分类"}
	registration := models.Registration{Email: "scope@example.test", Status: "registered"}
	if err := handler.DB.Create(&category).Error; err != nil {
		t.Fatal(err)
	}
	if err := handler.DB.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/categories/assign", strings.NewReader(fmt.Sprintf(`{"scope":"account","ids":[%d],"category_id":%d}`, registration.ID, category.ID)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
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
