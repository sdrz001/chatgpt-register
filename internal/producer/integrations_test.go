package producer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chatgpt-register/internal/codexoauth"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/smsactivate"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

const (
	integrationEmail = "user@example.test"
	integrationAT    = "synthetic-access-token"
	integrationRT    = "synthetic-refresh-token"
	integrationIDT   = "synthetic-id-token"
)

type fakeMailClient struct {
	list func(context.Context, mailfetch.Account, int) ([]mailfetch.Message, error)
	get  func(context.Context, mailfetch.Account, string) (mailfetch.Message, error)

	createAlias func(context.Context, mailfetch.Account, string) error
}

func (f fakeMailClient) ListMessages(ctx context.Context, account mailfetch.Account, limit int) ([]mailfetch.Message, error) {
	if f.list == nil {
		return []mailfetch.Message{}, nil
	}
	return f.list(ctx, account, limit)
}

func (f fakeMailClient) CreateAlias(ctx context.Context, account mailfetch.Account, address string) error {
	if f.createAlias == nil {
		return nil
	}
	return f.createAlias(ctx, account, address)
}

func (f fakeMailClient) GetMessage(ctx context.Context, account mailfetch.Account, id string) (mailfetch.Message, error) {
	if f.get == nil {
		return mailfetch.Message{}, fmt.Errorf("message not found")
	}
	return f.get(ctx, account, id)
}

func domainMailFetchProducer(t *testing.T, mail fakeMailClient) *Producer {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Setting{}); err != nil {
		t.Fatal(err)
	}
	settings := []models.Setting{
		{Key: "mailbox_source", Value: "domain_api"},
		{Key: "domain_mail_url", Value: "https://mail.example.test"},
		{Key: "domain_mail_api_key", Value: "secret"},
		{Key: "domain_mail_domain", Value: "example.test"},
	}
	if err := database.Create(&settings).Error; err != nil {
		t.Fatal(err)
	}
	return &Producer{db: database, mail: mail}
}

func integrationTestProducer(t *testing.T, serverURL string) (*Producer, models.Registration) {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Registration{}, &models.Setting{}); err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{
		"sub2api_url": serverURL, "sub2api_api_key": "admin-key", "sub2api_group_ids": "12,13",
		"sub2api_concurrency": "10", "sub2api_priority": "1", "sub2api_timeout": "30",
	}
	for key, value := range settings {
		if err := database.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	auth, err := json.Marshal(map[string]any{
		"auth_mode": "oauth", "email": integrationEmail,
		"access_token": integrationAT, "refresh_token": integrationRT, "id_token": integrationIDT,
		"expires_in": 3600, "account_id": "account-id", "chatgpt_user_id": "user-id", "plan_type": "free",
	})
	if err != nil {
		t.Fatal(err)
	}
	registration := models.Registration{
		Email: integrationEmail, Status: "registered", CodexStatus: "authorized",
		Sub2APIStatus: "not_imported", AuthData: string(auth), AccountID: "account-id", UserID: "user-id", PlanType: "free",
	}
	if err := database.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}
	return &Producer{db: database}, registration
}

func TestOAuthSourceFromRegistration(t *testing.T) {
	registration := models.Registration{
		Email: integrationEmail, AccountID: "fallback-account", UserID: "fallback-user", PlanType: "free",
		AuthData: `{"credentials":{"access_token":"` + integrationAT + `","refresh_token":"` + integrationRT + `","id_token":"` + integrationIDT + `","expires_in":"120"}}`,
	}
	source, err := oauthSourceFromRegistration(registration)
	if err != nil {
		t.Fatal(err)
	}
	if source.Email != integrationEmail || source.AccessToken != integrationAT || source.RefreshToken != integrationRT || source.ExpiresIn != 120 {
		t.Fatalf("source=%+v", source)
	}
	if source.ChatGPTAccountID != "fallback-account" || source.ChatGPTUserID != "fallback-user" || source.PlanType != "free" {
		t.Fatalf("fallbacks=%+v", source)
	}
}

func TestImportSub2APISuccessUpdatesIndependentState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "admin-key" {
			t.Errorf("x-api-key=%q", r.Header.Get("x-api-key"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts":
			if r.URL.Query().Get("page") == "1" {
				fmt.Fprintf(w, `{"data":{"items":[{"id":91,"name":"%s","platform":"openai","type":"oauth","credentials":{"email":"%s"}}]}}`, integrationEmail, integrationEmail)
			} else {
				fmt.Fprint(w, `{"data":{"items":[]}}`)
			}
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/admin/accounts/91":
			fmt.Fprint(w, `{"data":{}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/accounts/91":
			fmt.Fprintf(w, `{"data":{"id":91,"name":"%s","platform":"openai","type":"oauth","status":"active","group_ids":[12,13],"credentials":{"email":"%s"}}}`, integrationEmail, integrationEmail)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	producer, registration := integrationTestProducer(t, server.URL)
	result, err := producer.ImportSub2API(context.Background(), registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != "updated" || result.AccountID != 91 {
		t.Fatalf("result=%+v", result)
	}
	if err := producer.db.First(&registration, registration.ID).Error; err != nil {
		t.Fatal(err)
	}
	if registration.Status != "registered" || registration.CodexStatus != "authorized" || registration.Sub2APIStatus != "imported" || registration.Sub2APIAccountID == nil || *registration.Sub2APIAccountID != 91 {
		t.Fatalf("registration=%+v", registration)
	}
}

func codexIntegrationTestProducer(t *testing.T) (*Producer, models.Registration) {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Registration{}, &models.SMSActivation{}, &models.Mailbox{}, &models.Setting{}); err != nil {
		t.Fatal(err)
	}
	mailbox := models.Mailbox{Email: integrationEmail, Provider: "outlook", ClientID: "client", RefreshToken: "refresh", Status: "verified"}
	if err := database.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	registration := models.Registration{
		Email: integrationEmail, MailboxID: mailbox.ID, Password: "account-password", Proxy: "http://proxy-user:proxy-pass@proxy.example.test:8080",
		Status: "registered", CodexStatus: "pending", Sub2APIStatus: "not_imported",
		AuthData: `{"auth_mode":"access_token","access_token":"old-token","custom":"keep-me"}`,
	}
	if err := database.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}
	return &Producer{db: database, mail: fakeMailClient{}}, registration
}

func TestAuthorizeCodexMergesTokensWithoutBuyingPhone(t *testing.T) {
	producer, registration := codexIntegrationTestProducer(t)
	for key, value := range map[string]string{
		"sms_api_key": "key", "sms_country": "187", "sms_phone_attempts": "5",
	} {
		if err := producer.db.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	phoneCalls := 0
	producer.acquirePhone = func(context.Context, uint) (*codexoauth.PhoneSession, error) {
		phoneCalls++
		return nil, fmt.Errorf("unexpected phone purchase")
	}
	producer.authorizeCodex = func(_ context.Context, input codexoauth.Input) (codexoauth.Tokens, error) {
		if input.Email != registration.Email || input.Password != registration.Password || input.Proxy != registration.Proxy || input.AcquirePhone == nil || input.MaxPhoneAttempts != 5 {
			t.Fatalf("input=%+v", input)
		}
		return codexoauth.Tokens{
			Email: integrationEmail, AccessToken: integrationAT, RefreshToken: integrationRT, IDToken: integrationIDT,
			ExpiresIn: 3600, ChatGPTAccountID: "codex-account", ChatGPTUserID: "codex-user", PlanType: "plus",
		}, nil
	}
	tokens, err := producer.AuthorizeCodex(context.Background(), registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.RefreshToken != integrationRT || phoneCalls != 0 {
		t.Fatalf("tokens=%+v phoneCalls=%d", tokens, phoneCalls)
	}
	if err := producer.db.First(&registration, registration.ID).Error; err != nil {
		t.Fatal(err)
	}
	if registration.Status != "registered" || registration.CodexStatus != "authorized" || registration.CodexAuthorizedAt == nil || registration.AccountID != "codex-account" || registration.UserID != "codex-user" || registration.PlanType != "plus" {
		t.Fatalf("registration=%+v", registration)
	}
	var auth map[string]any
	if err := json.Unmarshal([]byte(registration.AuthData), &auth); err != nil {
		t.Fatal(err)
	}
	if auth["custom"] != "keep-me" || auth["access_token"] != integrationAT || auth["refresh_token"] != integrationRT || auth["id_token"] != integrationIDT || auth["auth_mode"] != "oauth" {
		t.Fatalf("auth=%v", auth)
	}
	var count int64
	if err := producer.db.Model(&models.SMSActivation{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("SMS activation count=%d error=%v", count, err)
	}
}

func TestFetchCodeAfterConsumesMessageID(t *testing.T) {
	calls := 0
	producer := &Producer{mail: fakeMailClient{list: func(context.Context, mailfetch.Account, int) ([]mailfetch.Message, error) {
		calls++
		oldMessage := mailfetch.Message{ID: "old", From: "noreply@openai.com", Subject: "Old code 111111", ReceivedAt: time.Now()}
		if calls == 1 {
			return []mailfetch.Message{oldMessage}, nil
		}
		return []mailfetch.Message{
			oldMessage,
			{ID: "new", From: "noreply@openai.com", Subject: "New code 222222", ReceivedAt: time.Now()},
		}, nil
	}}}
	ignored := map[string]struct{}{}
	mailbox := models.Mailbox{Email: integrationEmail}
	first, err := producer.fetchCodeAfter(context.Background(), mailbox, time.Now().Add(-time.Minute), ignored)
	if err != nil {
		t.Fatal(err)
	}
	second, err := producer.fetchCodeAfter(context.Background(), mailbox, time.Now().Add(-time.Minute), ignored)
	if err != nil {
		t.Fatal(err)
	}
	if first != "111111" || second != "222222" {
		t.Fatalf("first=%q second=%q ignored=%v", first, second, ignored)
	}
}

func TestFetchCodeAfterCodeURLWaitsForValueAfterSnapshot(t *testing.T) {
	oldMessage := mailfetch.Message{
		ID: "api-code-old", From: "codes.example.test", FromName: "验证码 API",
		Subject: "OpenAI verification code 333333", ReceivedAt: time.Now(), Text: "333333",
	}
	newMessage := mailfetch.Message{
		ID: "api-code-new", From: "codes.example.test", FromName: "验证码 API",
		Subject: "OpenAI verification code 444444", ReceivedAt: time.Now(), Text: "444444",
	}
	calls := 0
	producer := &Producer{mail: fakeMailClient{list: func(context.Context, mailfetch.Account, int) ([]mailfetch.Message, error) {
		calls++
		if calls == 1 {
			return []mailfetch.Message{oldMessage}, nil
		}
		return []mailfetch.Message{oldMessage, newMessage}, nil
	}}}
	ignored := map[string]struct{}{oldMessage.ID: {}}
	mailbox := models.Mailbox{Email: integrationEmail, CodeURL: "https://codes.example.test/latest"}
	code, err := producer.fetchCodeAfter(context.Background(), mailbox, time.Now(), ignored)
	if err != nil || code != "444444" || calls < 2 {
		t.Fatalf("code=%q calls=%d error=%v", code, calls, err)
	}
}

func TestFetchCodeAfterDomainMailWaitsForNewMessageID(t *testing.T) {
	oldMessage := mailfetch.Message{ID: "old-domain-message", From: "noreply@openai.com", Subject: "Old code 555555"}
	newMessage := mailfetch.Message{ID: "new-domain-message", From: "noreply@openai.com", Subject: "New code 666666"}
	calls := 0
	producer := domainMailFetchProducer(t, fakeMailClient{list: func(context.Context, mailfetch.Account, int) ([]mailfetch.Message, error) {
		calls++
		if calls == 1 {
			return []mailfetch.Message{oldMessage}, nil
		}
		return []mailfetch.Message{oldMessage, newMessage}, nil
	}})
	mailbox := models.Mailbox{Email: integrationEmail, Provider: "domain_api", RemoteMailboxID: "remote-box"}
	code, err := producer.fetchCodeAfter(context.Background(), mailbox, time.Now(), map[string]struct{}{oldMessage.ID: {}})
	if err != nil || code != "666666" || calls < 2 {
		t.Fatalf("code=%q calls=%d error=%v", code, calls, err)
	}
}

func TestFetchCodeAfterDomainMailReadsHTMLOnlyDetail(t *testing.T) {
	producer := domainMailFetchProducer(t, fakeMailClient{
		list: func(context.Context, mailfetch.Account, int) ([]mailfetch.Message, error) {
			return []mailfetch.Message{{ID: "new-domain-message", Subject: "Verify your email"}}, nil
		},
		get: func(context.Context, mailfetch.Account, string) (mailfetch.Message, error) {
			return mailfetch.Message{
				ID: "new-domain-message", From: "noreply@openai.com", Subject: "Verify your email",
				HTML: `<html><body><p>Your verification code is <strong>731942</strong></p></body></html>`,
			}, nil
		},
	})
	mailbox := models.Mailbox{Email: integrationEmail, Provider: "domain_api", RemoteMailboxID: "remote-box"}
	code, err := producer.fetchCodeAfter(context.Background(), mailbox, time.Now(), map[string]struct{}{})
	if err != nil || code != "731942" {
		t.Fatalf("code=%q error=%v", code, err)
	}
}

func TestFetchCodeAfterRejectsMessagesBeforeRegistrationStart(t *testing.T) {
	startedAt := time.Now()
	producer := &Producer{mail: fakeMailClient{list: func(context.Context, mailfetch.Account, int) ([]mailfetch.Message, error) {
		return []mailfetch.Message{
			{ID: "old", From: "noreply@openai.com", Subject: "Old code 111111", ReceivedAt: startedAt.Add(-time.Minute)},
			{ID: "new", From: "noreply@openai.com", Subject: "New code 222222", ReceivedAt: startedAt.Add(time.Second)},
		}, nil
	}}}
	code, err := producer.fetchCodeAfter(context.Background(), models.Mailbox{Email: integrationEmail}, startedAt, map[string]struct{}{})
	if err != nil || code != "222222" {
		t.Fatalf("code=%q error=%v", code, err)
	}
}

func TestAuthorizeCodexEmailOTPOnlyUsesMessagesAfterSnapshot(t *testing.T) {
	producer, registration := codexIntegrationTestProducer(t)
	calls := 0
	producer.mail = fakeMailClient{list: func(context.Context, mailfetch.Account, int) ([]mailfetch.Message, error) {
		calls++
		oldMessage := mailfetch.Message{ID: "old", From: "noreply@openai.com", Subject: "Old code 111111", ReceivedAt: time.Now().Add(time.Minute)}
		if calls == 1 {
			return []mailfetch.Message{oldMessage}, nil
		}
		newMessage := mailfetch.Message{ID: "new", From: "noreply@openai.com", Subject: "New code 222222", ReceivedAt: time.Now().Add(time.Minute)}
		return []mailfetch.Message{oldMessage, newMessage}, nil
	}}
	producer.authorizeCodex = func(ctx context.Context, input codexoauth.Input) (codexoauth.Tokens, error) {
		code, err := input.FetchEmailCode(ctx)
		if err != nil {
			return codexoauth.Tokens{}, err
		}
		if code != "222222" {
			return codexoauth.Tokens{}, fmt.Errorf("unexpected code %s", code)
		}
		return codexoauth.Tokens{
			Email: integrationEmail, AccessToken: integrationAT, RefreshToken: integrationRT, IDToken: integrationIDT,
			ExpiresIn: 3600,
		}, nil
	}
	if _, err := producer.AuthorizeCodex(context.Background(), registration.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizeCodexUnlocksAfterPanic(t *testing.T) {
	producer, registration := codexIntegrationTestProducer(t)
	producer.authorizeCodex = func(context.Context, codexoauth.Input) (codexoauth.Tokens, error) {
		panic("synthetic authorizer panic")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("AuthorizeCodex did not panic")
			}
		}()
		_, _ = producer.AuthorizeCodex(context.Background(), registration.ID)
	}()
	producer.authorizeCodex = func(context.Context, codexoauth.Input) (codexoauth.Tokens, error) {
		return codexoauth.Tokens{
			Email: integrationEmail, AccessToken: integrationAT, RefreshToken: integrationRT, IDToken: integrationIDT, ExpiresIn: 3600,
		}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := producer.AuthorizeCodex(ctx, registration.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizeCodexFailureIsRedactedAndIndependent(t *testing.T) {
	producer, registration := codexIntegrationTestProducer(t)
	producer.authorizeCodex = func(context.Context, codexoauth.Input) (codexoauth.Tokens, error) {
		return codexoauth.Tokens{}, fmt.Errorf("failed %s %s %s", registration.Email, registration.Password, registration.Proxy)
	}
	_, err := producer.AuthorizeCodex(context.Background(), registration.ID)
	if err == nil {
		t.Fatal("AuthorizeCodex returned nil error")
	}
	for _, secret := range []string{registration.Email, registration.Password, registration.Proxy} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("returned error leaked %q", secret)
		}
	}
	if err := producer.db.First(&registration, registration.ID).Error; err != nil {
		t.Fatal(err)
	}
	if registration.Status != "registered" || registration.CodexStatus != "failed" || registration.Sub2APIStatus != "not_imported" {
		t.Fatalf("registration=%+v", registration)
	}
	for _, secret := range []string{integrationEmail, "account-password", registration.Proxy} {
		if strings.Contains(registration.CodexError+registration.Log, secret) {
			t.Fatalf("database leaked %q", secret)
		}
	}
}

func TestAcquireSMSPhonePersistsLifecycle(t *testing.T) {
	producer, registration := codexIntegrationTestProducer(t)
	for key, value := range map[string]string{
		"sms_platform": "hero-sms", "sms_api_key": "key", "sms_country": "187", "sms_max_price": "0.5", "sms_timeout": "30",
	} {
		if err := producer.db.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var statuses []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			fmt.Fprint(w, "ACCESS_NUMBER:activation-1:12025550123")
		case "setStatus":
			statuses = append(statuses, r.URL.Query().Get("status"))
			fmt.Fprint(w, "ACCESS_READY")
		case "getStatus":
			fmt.Fprint(w, "STATUS_OK:654321")
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	producer.newSMSClient = func(config smsactivate.Config) (*smsactivate.Client, error) {
		return smsactivate.New(config, smsactivate.WithEndpoint(server.URL), smsactivate.WithHTTPClient(server.Client()))
	}
	session, err := producer.acquireSMSPhone(context.Background(), registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Number != "+12025550123" {
		t.Fatalf("number=%q", session.Number)
	}
	code, err := session.WaitCode(context.Background())
	if err != nil || code != "654321" {
		t.Fatalf("WaitCode=%q error=%v", code, err)
	}
	if err := session.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := session.Finish(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(statuses, ",") != "1,6" {
		t.Fatalf("statuses=%v", statuses)
	}
	var activation models.SMSActivation
	if err := producer.db.First(&activation).Error; err != nil {
		t.Fatal(err)
	}
	if activation.Status != "completed" || activation.ClosedAt == nil || activation.PhoneNumber != "+12025550123" || activation.ActivationID != "activation-1" {
		t.Fatalf("activation=%+v", activation)
	}
}

func TestSMSPhoneWaitCodeRequestsRetryForSameNumber(t *testing.T) {
	producer, registration := codexIntegrationTestProducer(t)
	for key, value := range map[string]string{
		"sms_platform": "hero-sms", "sms_api_key": "key", "sms_country": "187", "sms_max_price": "0.5", "sms_timeout": "30",
	} {
		if err := producer.db.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	allocations := 0
	statusChecks := 0
	var statuses []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			allocations++
			fmt.Fprint(w, "ACCESS_NUMBER:activation-1:12025550123")
		case "setStatus":
			statuses = append(statuses, r.URL.Query().Get("status"))
			fmt.Fprint(w, "ACCESS_READY")
		case "getStatus":
			statusChecks++
			if statusChecks == 1 {
				fmt.Fprint(w, "STATUS_OK:111111")
			} else {
				fmt.Fprint(w, "STATUS_WAIT_RETRY:222222")
			}
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	producer.newSMSClient = func(config smsactivate.Config) (*smsactivate.Client, error) {
		return smsactivate.New(config, smsactivate.WithEndpoint(server.URL), smsactivate.WithHTTPClient(server.Client()))
	}
	session, err := producer.acquireSMSPhone(context.Background(), registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := session.WaitCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.WaitCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "111111" || second != "222222" || allocations != 1 || strings.Join(statuses, ",") != "1,3" {
		t.Fatalf("first=%q second=%q allocations=%d statuses=%v", first, second, allocations, statuses)
	}
	if err := session.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireSMSPhoneBlocksConcurrentOrder(t *testing.T) {
	producer, registration := codexIntegrationTestProducer(t)
	for key, value := range map[string]string{
		"sms_platform": "hero-sms", "sms_api_key": "key", "sms_country": "187", "sms_max_price": "0.5", "sms_timeout": "30",
	} {
		if err := producer.db.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var allocations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			allocations++
			fmt.Fprintf(w, "ACCESS_NUMBER:activation-%d:1202555012%d", allocations, allocations)
		case "setStatus":
			fmt.Fprint(w, "ACCESS_CANCEL")
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	producer.newSMSClient = func(config smsactivate.Config) (*smsactivate.Client, error) {
		return smsactivate.New(config, smsactivate.WithEndpoint(server.URL), smsactivate.WithHTTPClient(server.Client()))
	}
	first, err := producer.acquireSMSPhone(context.Background(), registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := producer.acquireSMSPhone(context.Background(), registration.ID); err == nil || !strings.Contains(err.Error(), "进行中的接码订单") {
		t.Fatalf("second acquire error=%v", err)
	}
	if allocations != 1 {
		t.Fatalf("allocations=%d", allocations)
	}
	if err := first.Finish(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := producer.acquireSMSPhone(context.Background(), registration.ID); err != nil {
		t.Fatal(err)
	}
	if allocations != 2 {
		t.Fatalf("allocations after release=%d", allocations)
	}
}

func TestAcquireSMSPhoneRecoversCloseFailedOrder(t *testing.T) {
	producer, registration := codexIntegrationTestProducer(t)
	for key, value := range map[string]string{
		"sms_platform": "hero-sms", "sms_api_key": "key", "sms_country": "187", "sms_max_price": "0.5", "sms_timeout": "30",
	} {
		if err := producer.db.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	allocations := 0
	closeCalls := 0
	var events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			allocations++
			events = append(events, fmt.Sprintf("allocate:%d", allocations))
			fmt.Fprintf(w, "ACCESS_NUMBER:activation-%d:1202555012%d", allocations, allocations)
		case "setStatus":
			closeCalls++
			events = append(events, "close:"+r.URL.Query().Get("id"))
			if closeCalls == 1 {
				http.Error(w, "temporary close failure", http.StatusBadGateway)
				return
			}
			fmt.Fprint(w, "ACCESS_CANCEL")
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	producer.newSMSClient = func(config smsactivate.Config) (*smsactivate.Client, error) {
		return smsactivate.New(config, smsactivate.WithEndpoint(server.URL), smsactivate.WithHTTPClient(server.Client()))
	}
	first, err := producer.acquireSMSPhone(context.Background(), registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(context.Background(), false); err == nil {
		t.Fatal("first Finish returned nil error")
	}
	var failed models.SMSActivation
	if err := producer.db.First(&failed, "activation_id = ?", "activation-1").Error; err != nil {
		t.Fatal(err)
	}
	if failed.Status != "close_failed" {
		t.Fatalf("failed=%+v", failed)
	}
	if _, err := producer.acquireSMSPhone(context.Background(), registration.ID); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "allocate:1,close:activation-1,close:activation-1,allocate:2" {
		t.Fatalf("events=%v", events)
	}
	if err := producer.db.First(&failed, failed.ID).Error; err != nil {
		t.Fatal(err)
	}
	if failed.Status != "cancelled" || failed.ClosedAt == nil {
		t.Fatalf("recovered=%+v", failed)
	}
}

func TestAcquireSMSPhoneClosesHistoricalOrderFirst(t *testing.T) {
	producer, registration := codexIntegrationTestProducer(t)
	for key, value := range map[string]string{
		"sms_platform": "hero-sms", "sms_api_key": "key", "sms_country": "187", "sms_max_price": "0.5", "sms_timeout": "30",
	} {
		if err := producer.db.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	historical := models.SMSActivation{
		RegistrationID: registration.ID, Provider: "hero-sms", ActivationID: "old-activation", PhoneNumber: "+12025550120",
		CountryID: 187, Service: smsactivate.ServiceOpenAI, Status: "orphaned", CreatedAt: time.Now().Add(-3 * time.Minute),
	}
	if err := producer.db.Create(&historical).Error; err != nil {
		t.Fatal(err)
	}
	var events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "setStatus":
			events = append(events, "cancel:"+r.URL.Query().Get("id"))
			fmt.Fprint(w, "ACCESS_CANCEL")
		case "getNumber":
			events = append(events, "allocate")
			fmt.Fprint(w, "ACCESS_NUMBER:new-activation:12025550121")
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	producer.newSMSClient = func(config smsactivate.Config) (*smsactivate.Client, error) {
		return smsactivate.New(config, smsactivate.WithEndpoint(server.URL), smsactivate.WithHTTPClient(server.Client()))
	}
	if _, err := producer.acquireSMSPhone(context.Background(), registration.ID); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "cancel:old-activation,allocate" {
		t.Fatalf("events=%v", events)
	}
	if err := producer.db.First(&historical, historical.ID).Error; err != nil {
		t.Fatal(err)
	}
	if historical.Status != "cancelled" || historical.ClosedAt == nil {
		t.Fatalf("historical=%+v", historical)
	}
}

func TestRejectedSMSPhoneCreatesReplacementOrder(t *testing.T) {
	producer, registration := codexIntegrationTestProducer(t)
	for key, value := range map[string]string{
		"sms_platform": "hero-sms", "sms_api_key": "key", "sms_country": "187", "sms_max_price": "0.5", "sms_timeout": "30",
	} {
		if err := producer.db.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var allocations int
	var statuses []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			allocations++
			fmt.Fprintf(w, "ACCESS_NUMBER:activation-%d:1202555012%d", allocations, allocations)
		case "setStatus":
			statuses = append(statuses, r.URL.Query().Get("id")+":"+r.URL.Query().Get("status"))
			fmt.Fprint(w, "ACCESS_READY")
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	producer.newSMSClient = func(config smsactivate.Config) (*smsactivate.Client, error) {
		return smsactivate.New(config, smsactivate.WithEndpoint(server.URL), smsactivate.WithHTTPClient(server.Client()))
	}
	first, err := producer.acquireSMSPhone(context.Background(), registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	second, err := producer.acquireSMSPhone(context.Background(), registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Number == second.Number || allocations != 2 || strings.Join(statuses, ",") != "activation-1:8" {
		t.Fatalf("first=%q second=%q allocations=%d statuses=%v", first.Number, second.Number, allocations, statuses)
	}
	var rows []models.SMSActivation
	if err := producer.db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Status != "cancelled" || rows[0].ClosedAt == nil || rows[1].Status != "allocated" || rows[1].ClosedAt != nil {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestImportSub2APIFailureRedactsAndKeepsRegistration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"failed for %s with %s %s %s"}`, integrationEmail, integrationAT, integrationRT, integrationIDT)
	}))
	defer server.Close()

	producer, registration := integrationTestProducer(t, server.URL)
	_, err := producer.ImportSub2API(context.Background(), registration.ID)
	if err == nil {
		t.Fatal("ImportSub2API returned nil error")
	}
	for _, secret := range []string{integrationEmail, integrationAT, integrationRT, integrationIDT} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("returned error leaked %q: %v", secret, err)
		}
	}
	if err := producer.db.First(&registration, registration.ID).Error; err != nil {
		t.Fatal(err)
	}
	if registration.Status != "registered" || registration.CodexStatus != "authorized" || registration.Sub2APIStatus != "failed" {
		t.Fatalf("registration=%+v", registration)
	}
	for _, secret := range []string{integrationEmail, integrationAT, integrationRT, integrationIDT} {
		if strings.Contains(registration.Sub2APIError+registration.Log, secret) {
			t.Fatalf("database leaked %q", secret)
		}
	}
}
