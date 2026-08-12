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
	if err := database.AutoMigrate(&models.Setting{}, &models.ProxyPool{}, &models.Category{}, &models.Mailbox{}, &models.Registration{}); err != nil {
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
	return requestProduceBody(router, `{"count":0}`)
}

func requestProduceBody(router *gin.Engine, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/produce", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	return response
}

func TestValidateProduceScope(t *testing.T) {
	handler, _ := produceTestHandler(t, nil)
	mailboxCategory := models.Category{Scope: "mailbox", Name: "邮箱组"}
	accountCategory := models.Category{Scope: "account", Name: "账户组"}
	if err := handler.DB.Create(&mailboxCategory).Error; err != nil {
		t.Fatal(err)
	}
	if err := handler.DB.Create(&accountCategory).Error; err != nil {
		t.Fatal(err)
	}
	verified := models.Mailbox{Email: "verified@example.test", Status: "verified", CategoryID: &mailboxCategory.ID}
	unverified := models.Mailbox{Email: "pending@example.test", Status: "unverified"}
	if err := handler.DB.Create(&verified).Error; err != nil {
		t.Fatal(err)
	}
	if err := handler.DB.Create(&unverified).Error; err != nil {
		t.Fatal(err)
	}

	all, err := handler.validateProduceScope(produceInput{Scope: "all"})
	if err != nil || all.CategoryID != nil || len(all.MailboxIDs) != 0 {
		t.Fatalf("all=%+v error=%v", all, err)
	}
	category, err := handler.validateProduceScope(produceInput{Scope: "category", CategoryID: &mailboxCategory.ID})
	if err != nil || category.CategoryID == nil || *category.CategoryID != mailboxCategory.ID {
		t.Fatalf("category=%+v error=%v", category, err)
	}
	selected, err := handler.validateProduceScope(produceInput{Scope: "mailboxes", MailboxIDs: []uint{verified.ID, verified.ID}})
	if err != nil || len(selected.MailboxIDs) != 1 || selected.MailboxIDs[0] != verified.ID {
		t.Fatalf("selected=%+v error=%v", selected, err)
	}
	for name, input := range map[string]produceInput{
		"missing category": {Scope: "category"},
		"wrong category":   {Scope: "category", CategoryID: &accountCategory.ID},
		"empty mailboxes":  {Scope: "mailboxes"},
		"unverified":       {Scope: "mailboxes", MailboxIDs: []uint{unverified.ID}},
		"unknown":          {Scope: "other"},
	} {
		if _, err := handler.validateProduceScope(input); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestApplyProduceProxyPoolUsesExplicitOrDefaultSnapshot(t *testing.T) {
	handler, _ := produceTestHandler(t, nil)
	pool := models.ProxyPool{Name: "日本池", Proxies: "proxy-a.test:8080\nproxy-a.test:8080\nproxy-b.test:8080"}
	if err := handler.DB.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}
	if err := handler.setDefaultProxyPoolID(handler.DB, pool.ID); err != nil {
		t.Fatal(err)
	}
	var defaultScope producer.Scope
	if err := handler.applyProduceProxyPool(&defaultScope, nil); err != nil {
		t.Fatal(err)
	}
	if defaultScope.ProxyPoolID != pool.ID || defaultScope.ProxyPoolName != pool.Name || strings.Join(defaultScope.Proxies, ",") != "proxy-a.test:8080,proxy-b.test:8080" {
		t.Fatalf("default scope=%+v", defaultScope)
	}
	pool.Proxies = "changed.test:8080"
	if err := handler.DB.Save(&pool).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Join(defaultScope.Proxies, ",") != "proxy-a.test:8080,proxy-b.test:8080" {
		t.Fatalf("scope was not a snapshot: %+v", defaultScope)
	}
	zero := uint(0)
	direct := producer.Scope{}
	if err := handler.applyProduceProxyPool(&direct, &zero); err != nil || direct.ProxyPoolID != 0 || len(direct.Proxies) != 0 {
		t.Fatalf("direct=%+v error=%v", direct, err)
	}
}

func TestApplyProduceProxyPoolRejectsMissingOrEmptyPool(t *testing.T) {
	handler, _ := produceTestHandler(t, nil)
	missing := uint(999)
	if err := handler.applyProduceProxyPool(&producer.Scope{}, &missing); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("missing error=%v", err)
	}
	empty := models.ProxyPool{Name: "空池"}
	if err := handler.DB.Create(&empty).Error; err != nil {
		t.Fatal(err)
	}
	if err := handler.applyProduceProxyPool(&producer.Scope{}, &empty.ID); err == nil || !strings.Contains(err.Error(), "没有可用代理") {
		t.Fatalf("empty error=%v", err)
	}
}

func TestProduceRejectsScopeBeforeBrowserCheck(t *testing.T) {
	_, router := produceTestHandler(t, browserboot.New())
	response := requestProduceBody(router, `{"count":1,"scope":"mailboxes","mailbox_ids":[]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "请选择至少一个邮箱") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
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
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "注册数量必须") {
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
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "注册数量必须") {
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
