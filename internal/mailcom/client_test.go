package mailcom

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientListsAndReadsMessages(t *testing.T) {
	token := testToken(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "auth_id": "auth-1"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<input name="statistics" value="stats-1">`)
		case "/login":
			if err := r.ParseForm(); err != nil || r.Form.Get("username") != "user@mail.com" || r.Form.Get("password") != "password" {
				t.Fatalf("login form=%v err=%v", r.Form, err)
			}
			w.Header().Set("Location", "/callback?ott=one-time-token")
			w.WriteHeader(http.StatusSeeOther)
		case "/halogin":
			w.Header().Set("Location", "/landing?sid=session-1")
			w.WriteHeader(http.StatusFound)
		case "/oauth":
			if r.URL.Query().Get("sid") != "session-1" {
				t.Fatalf("sid=%q", r.URL.Query().Get("sid"))
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token})
		case "/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mailListElements":[{"rawData":{"attribute":{"mailIdentifier":"message-1","internalDate":"1710000000000"},"mailHeader":{"subject":"Your verification code","from":"OpenAI <noreply@openai.com>","to":["user@mail.com"],"date":"1710000000000"}}}]}`))
		case "/body/message-1/Body":
			fmt.Fprint(w, "Your verification code is 123456")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New(Config{
		OAuthPublicSecret: "public-secret",
		Endpoints: Endpoints{
			LoginPageURL: server.URL + "/",
			LoginURL:     server.URL + "/login",
			OAuthURL:     server.URL + "/oauth",
			MailListURL:  server.URL + "/list",
			MailBodyURL:  server.URL + "/body/{mail_id}/Body",
		},
	}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}

	account := Account{Email: "user@mail.com", Password: "password"}
	messages, err := client.ListMessages(context.Background(), account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != "message-1" || messages[0].Sender != "OpenAI <noreply@openai.com>" {
		t.Fatalf("messages=%+v", messages)
	}
	message, err := client.GetMessage(context.Background(), account, messages[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if message.Body != "Your verification code is 123456" {
		t.Fatalf("message=%+v", message)
	}
	if got := ExtractCode(message.Subject, message.Body); got != "123456" {
		t.Fatalf("code=%q", got)
	}
}

func TestExtractCodeRequiresUniqueGenericCandidate(t *testing.T) {
	if got := ExtractCode("", "Reference 1234 and invoice 5678"); got != "" {
		t.Fatalf("got=%q", got)
	}
	if got := ExtractCode("", "654321 is your login code"); got != "654321" {
		t.Fatalf("got=%q", got)
	}
}

func TestNewAllowsAutomaticOAuthSecretDiscovery(t *testing.T) {
	client, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if client.endpoints.OAuthConfigURL != defaultOAuthConfigURL {
		t.Fatalf("OAuthConfigURL=%q", client.endpoints.OAuthConfigURL)
	}
}

func testToken(t *testing.T, payload map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimRight(base64.RawURLEncoding.EncodeToString(data), "=")
	}
	return encode(map[string]string{"alg": "none"}) + "." + encode(payload) + ".signature"
}
