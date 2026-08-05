package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chatgpt-register/internal/browserboot"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/producer"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func produceTestHandler(t *testing.T, browser *browserboot.Manager) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Setting{}, &models.Mailbox{}, &models.Registration{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	handler := &Handler{
		DB:       database,
		Browser:  browser,
		Producer: producer.New(database, mailfetch.New()),
	}
	router := gin.New()
	router.GET("/browser/status", handler.BrowserStatus)
	router.POST("/produce", handler.Produce)
	return handler, router
}

func resetBrowserHealthTestState(t *testing.T) {
	t.Helper()
	originalChecker := checkCloakBrowser
	originalNow := browserHealthNow
	originalRodSnapshot := rodBrowserSnapshot
	originalRodReady := rodBrowserReady
	browserHealthCache.Lock()
	browserHealthCache.entries = make(map[string]cachedBrowserBackendStatus)
	browserHealthCache.Unlock()
	t.Cleanup(func() {
		checkCloakBrowser = originalChecker
		browserHealthNow = originalNow
		rodBrowserSnapshot = originalRodSnapshot
		rodBrowserReady = originalRodReady
		browserHealthCache.Lock()
		browserHealthCache.entries = make(map[string]cachedBrowserBackendStatus)
		browserHealthCache.Unlock()
	})
}

func saveBrowserSettings(t *testing.T, handler *Handler, values map[string]string) {
	t.Helper()
	for key, value := range values {
		if err := handler.DB.Save(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func requestBrowserStatus(t *testing.T, router *gin.Engine) (int, browserBackendStatus) {
	t.Helper()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/browser/status", nil))
	var status browserBackendStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v body=%s", err, response.Body.String())
	}
	return response.Code, status
}

func requestProduce(router *gin.Engine) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/produce", strings.NewReader(`{"count":0}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	return response
}

func TestBrowserStatusDefaultsToRodAndGatesProduce(t *testing.T) {
	resetBrowserHealthTestState(t)
	_, router := produceTestHandler(t, browserboot.New())

	code, status := requestBrowserStatus(t, router)
	if code != http.StatusOK || status.Backend != browserBackendRod || status.Ready {
		t.Fatalf("code=%d status=%+v", code, status)
	}
	if status.Phase != "checking" {
		t.Fatalf("phase=%q", status.Phase)
	}

	response := requestProduce(router)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "正在检查浏览器") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestBrowserStatusRodReadyUsesManagerSnapshot(t *testing.T) {
	resetBrowserHealthTestState(t)
	browser := browserboot.New()
	rodBrowserReady = func(manager *browserboot.Manager) bool {
		if manager != browser {
			t.Fatal("unexpected Rod manager")
		}
		return true
	}
	rodBrowserSnapshot = func(manager *browserboot.Manager) browserboot.Status {
		if manager != browser {
			t.Fatal("unexpected Rod manager")
		}
		return browserboot.Status{Ready: true, Phase: "ready", Message: "fixture Rod ready"}
	}
	_, router := produceTestHandler(t, browser)

	code, status := requestBrowserStatus(t, router)
	if code != http.StatusOK || status.Backend != browserBackendRod || !status.Ready || status.Phase != "ready" {
		t.Fatalf("code=%d status=%+v", code, status)
	}
	if status.Message != "fixture Rod ready" {
		t.Fatalf("message=%q", status.Message)
	}
	response := requestProduce(router)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "生产数量必须") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestBrowserStatusCloakBrowserReadyAndProduceUsesSameCheck(t *testing.T) {
	resetBrowserHealthTestState(t)
	handler, router := produceTestHandler(t, nil)
	saveBrowserSettings(t, handler, map[string]string{
		"browser_backend":   browserBackendCloakBrowser,
		"python_executable": `C:\fixture\python.exe`,
	})
	calls := 0
	checkCloakBrowser = func(_ context.Context, pythonExecutable string) browserBackendStatus {
		calls++
		if pythonExecutable != `C:\fixture\python.exe` {
			t.Fatalf("python=%q", pythonExecutable)
		}
		return browserBackendStatus{Ready: true, Phase: "ready", Message: "fixture ready"}
	}

	code, status := requestBrowserStatus(t, router)
	if code != http.StatusOK || status.Backend != browserBackendCloakBrowser || !status.Ready {
		t.Fatalf("code=%d status=%+v", code, status)
	}
	response := requestProduce(router)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "生产数量必须") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if calls != 1 {
		t.Fatalf("health calls=%d want=1", calls)
	}
}

func TestBrowserStatusCloakBrowserFailureGatesProduce(t *testing.T) {
	resetBrowserHealthTestState(t)
	handler, router := produceTestHandler(t, nil)
	saveBrowserSettings(t, handler, map[string]string{"browser_backend": browserBackendCloakBrowser})
	checkCloakBrowser = func(context.Context, string) browserBackendStatus {
		return browserBackendStatus{Phase: "error", Message: "CloakBrowser 依赖缺失", Error: "cloakbrowser_import"}
	}

	code, status := requestBrowserStatus(t, router)
	if code != http.StatusOK || status.Backend != browserBackendCloakBrowser || status.Ready || status.Error != "cloakbrowser_import" {
		t.Fatalf("code=%d status=%+v", code, status)
	}
	response := requestProduce(router)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "CloakBrowser 依赖缺失") || !strings.Contains(response.Body.String(), "cloakbrowser_import") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCloakBrowserHealthCacheUsesBackendAndPythonKey(t *testing.T) {
	resetBrowserHealthTestState(t)
	handler, _ := produceTestHandler(t, nil)
	saveBrowserSettings(t, handler, map[string]string{
		"browser_backend":   browserBackendCloakBrowser,
		"python_executable": "python-a",
	})
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	browserHealthNow = func() time.Time { return now }
	calls := 0
	checkCloakBrowser = func(_ context.Context, pythonExecutable string) browserBackendStatus {
		calls++
		return browserBackendStatus{Ready: true, Phase: "ready", Message: pythonExecutable}
	}

	if status := handler.checkBrowserBackend(context.Background()); status.Message != "python-a" {
		t.Fatalf("first status=%+v", status)
	}
	if status := handler.checkBrowserBackend(context.Background()); status.Message != "python-a" || calls != 1 {
		t.Fatalf("cached status=%+v calls=%d", status, calls)
	}
	saveBrowserSettings(t, handler, map[string]string{"python_executable": "python-b"})
	if status := handler.checkBrowserBackend(context.Background()); status.Message != "python-b" || calls != 2 {
		t.Fatalf("switched status=%+v calls=%d", status, calls)
	}
	now = now.Add(browserHealthCacheTTL + time.Millisecond)
	if status := handler.checkBrowserBackend(context.Background()); status.Message != "python-b" || calls != 3 {
		t.Fatalf("expired status=%+v calls=%d", status, calls)
	}
}

func TestParseCloakBrowserHealthJSONLV1(t *testing.T) {
	message, err := parseCloakBrowserHealth([]byte("\n{\"version\":1,\"type\":\"health\",\"request_id\":\"health\",\"ok\":false,\"code\":\"cloakbrowser_import\",\"message\":\"dependency missing\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if message.OK || message.Code != "cloakbrowser_import" || message.Message != "dependency missing" {
		t.Fatalf("message=%+v", message)
	}
	if _, err := parseCloakBrowserHealth([]byte(`{"version":2,"type":"health","request_id":"health","ok":true}`)); err == nil {
		t.Fatal("expected protocol validation error")
	}
}

func TestBuildCredentialsKeepsOAuthTokens(t *testing.T) {
	auth, err := json.Marshal(map[string]any{
		"auth_mode":       "oauth",
		"access_token":    "access-token",
		"refresh_token":   "refresh-token",
		"id_token":        "id-token",
		"expires_in":      3600,
		"expires_at":      "2026-08-02T05:00:00Z",
		"account_id":      "account-id",
		"chatgpt_user_id": "user-id",
		"email":           "oauth@example.test",
		"plan_type":       "free",
	})
	if err != nil {
		t.Fatal(err)
	}

	got := buildCredentials(string(auth), "fallback@example.test")
	for key, want := range map[string]string{
		"auth_mode":          "oauth",
		"access_token":       "access-token",
		"refresh_token":      "refresh-token",
		"id_token":           "id-token",
		"expires_at":         "2026-08-02T05:00:00Z",
		"chatgpt_account_id": "account-id",
		"chatgpt_user_id":    "user-id",
		"email":              "oauth@example.test",
		"plan_type":          "free",
	} {
		if value, _ := got[key].(string); value != want {
			t.Fatalf("%s=%q want=%q", key, value, want)
		}
	}
	if got["expires_in"] != float64(3600) {
		t.Fatalf("expires_in=%v", got["expires_in"])
	}
}

func TestBuildCredentialsKeepsLegacyAgentIdentity(t *testing.T) {
	auth := `{"auth_mode":"agent_identity","agent_identity":{"agent_runtime_id":"runtime-id","agent_private_key":"private-key","account_id":"account-id","email":"legacy@example.test"}}`
	got := buildCredentials(auth, "fallback@example.test")
	if got["agent_runtime_id"] != "runtime-id" || got["agent_private_key"] != "private-key" {
		t.Fatalf("legacy credentials=%v", got)
	}
}
