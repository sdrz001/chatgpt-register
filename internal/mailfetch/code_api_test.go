package mailfetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateCodeURL(t *testing.T) {
	for _, rawURL := range []string{"http://api.example.test/code", "https://another.example.test/v1/otp?user=a"} {
		if err := ValidateCodeURL(rawURL); err != nil {
			t.Fatalf("ValidateCodeURL(%q)=%v", rawURL, err)
		}
	}
	for _, rawURL := range []string{"", "/api/code", "ftp://api.example.test/code", "not-a-url"} {
		if !errors.Is(ValidateCodeURL(rawURL), ErrInvalidCodeURL) {
			t.Fatalf("ValidateCodeURL(%q) should fail", rawURL)
		}
	}
}

func TestExtractAPICodeFormats(t *testing.T) {
	tests := map[string]string{
		`123456`:                  "123456",
		`{"code":"234567"}`:       "234567",
		`{"data":{"otp":345678}}`: "345678",
		`{"result":{"verification_code":"456789"}}`: "456789",
		`{"message":"OpenAI code is 567890"}`:       "567890",
		`{"created_at":1735689600}`:                 "",
	}
	for body, expected := range tests {
		if actual := extractAPICode([]byte(body)); actual != expected {
			t.Fatalf("extractAPICode(%q)=%q want %q", body, actual, expected)
		}
	}
}

func TestExtractAPICodeFromICloudHTML(t *testing.T) {
	body := `<html><head><style>body{color:#202123}.main{color:#353740}</style></head><body>
		<p>输入此临时验证码以继续：</p>
		<p style="color:#5D5D5D">343142</p>
	</body></html>`
	if actual := extractAPICode([]byte(body)); actual != "343142" {
		t.Fatalf("extractAPICode HTML=%q want 343142", actual)
	}
}

func TestCodeAPIMessageLifecycle(t *testing.T) {
	code := "111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("username") != "user@example.test" || r.URL.Query().Get("password") != "secret-token" {
			t.Errorf("query=%v", r.URL.Query())
		}
		fmt.Fprintf(w, `{"data":{"verification_code":"%s"}}`, code)
	}))
	defer server.Close()

	client := New(WithHTTPClient(server.Client()))
	account := Account{Email: "user@example.test", CodeURL: server.URL + "/code?username=user%40example.test&password=secret-token"}
	if err := client.Verify(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	first, err := client.ListMessages(context.Background(), account, 20)
	if err != nil || len(first) != 1 {
		t.Fatalf("first=%v error=%v", first, err)
	}
	if first[0].Text != "111111" || !strings.Contains(first[0].Subject, "111111") {
		t.Fatalf("message=%+v", first[0])
	}
	for _, secret := range []string{"secret-token", account.CodeURL, "user@example.test"} {
		if strings.Contains(fmt.Sprintf("%+v", first[0]), secret) {
			t.Fatalf("message leaked %q: %+v", secret, first[0])
		}
	}
	body, err := client.GetMessage(context.Background(), account, first[0].ID)
	if err != nil || body.ID != first[0].ID || body.Text != "111111" {
		t.Fatalf("body=%+v error=%v", body, err)
	}
	code = "222222"
	second, err := client.ListMessages(context.Background(), account, 20)
	if err != nil || len(second) != 1 || second[0].Text != "222222" || second[0].ID == first[0].ID {
		t.Fatalf("second=%v error=%v", second, err)
	}
}

func TestCodeURLArchiveReturnsHistoricalMailBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("n") != "50" {
			t.Errorf("query=%v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><style>.amount{color:#202123}</style></head><body><div class="su">ChatGPT - 你的新套餐</div><div class="bd"><p>你已成功订阅 ChatGPT Plus。</p><span>ChatGPT Plus Subscription</span><span>$20.00</span></div></body></html>`)
	}))
	defer server.Close()

	client := New(WithHTTPClient(server.Client()))
	message, err := client.GetCodeURLArchive(context.Background(), Account{CodeURL: server.URL + "/mailbox?token=secret"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message.Text, "你已成功订阅 ChatGPT Plus") || !strings.Contains(message.HTML, "ChatGPT Plus Subscription") {
		t.Fatalf("message=%+v", message)
	}
	if strings.Contains(message.Text, "#202123") {
		t.Fatalf("visible text retained style content: %q", message.Text)
	}
}

func TestCodeAPIReadsCodeFromSameOriginIframe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mailbox/token":
			fmt.Fprint(w, `<html><body><iframe src="/mailbox/token/content"></iframe></body></html>`)
		case "/mailbox/token/content":
			fmt.Fprint(w, `<html><body><p>Enter this temporary verification code to continue:</p><strong>441211</strong></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(WithHTTPClient(server.Client()))
	messages, err := client.ListMessages(context.Background(), Account{CodeURL: server.URL + "/mailbox/token"}, 20)
	if err != nil || len(messages) != 1 || messages[0].Text != "441211" {
		t.Fatalf("messages=%v error=%v", messages, err)
	}
}

func TestCodeAPIDoesNotFollowCrossOriginIframe(t *testing.T) {
	foreignCalled := false
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignCalled = true
		fmt.Fprint(w, "441211")
	}))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `<html><body><iframe src="%s/content"></iframe></body></html>`, foreign.URL)
	}))
	defer server.Close()

	client := New(WithHTTPClient(server.Client()))
	messages, err := client.ListMessages(context.Background(), Account{CodeURL: server.URL + "/mailbox/token"}, 20)
	if err != nil || len(messages) != 0 || foreignCalled {
		t.Fatalf("messages=%v error=%v foreign_called=%v", messages, err, foreignCalled)
	}
}

func TestCodeAPINoCodeAndErrors(t *testing.T) {
	status := http.StatusOK
	body := `{"message":"waiting"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer server.Close()

	client := New(WithHTTPClient(server.Client()))
	account := Account{CodeURL: server.URL + "/code?password=private-value"}
	if err := client.Verify(context.Background(), account); err != nil {
		t.Fatalf("Verify waiting=%v", err)
	}
	messages, err := client.ListMessages(context.Background(), account, 20)
	if err != nil || len(messages) != 0 {
		t.Fatalf("messages=%v error=%v", messages, err)
	}
	status = http.StatusBadGateway
	_, err = client.ListMessages(context.Background(), account, 20)
	if !errors.Is(err, ErrCodeAPIFailed) || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("error leaked URL: %v", err)
	}
}
