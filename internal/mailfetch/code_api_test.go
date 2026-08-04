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
