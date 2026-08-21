package mailcom

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveMobileOAuthMailbox(t *testing.T) {
	email := strings.TrimSpace(os.Getenv("MAILCOM_TEST_EMAIL"))
	password := os.Getenv("MAILCOM_TEST_PASSWORD")
	if email == "" || password == "" {
		t.Skip("mail.com live credentials are not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	liveHTTP := &http.Client{Timeout: 90 * time.Second, Transport: liveTraceTransport{t: t}}
	client, err := New(Config{}, WithHTTPClient(liveHTTP))
	if err != nil {
		t.Fatal(err)
	}
	messages, err := client.ListMessages(ctx, Account{Email: email, Password: password}, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mail.com mobile OAuth login succeeded; messages=%d", len(messages))
	if len(messages) == 0 {
		return
	}
	message, err := client.GetMessage(ctx, Account{Email: email, Password: password}, messages[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mail.com message body read succeeded; bytes=%d", len(message.Body))
}

func TestLiveSettingsOAuthListsAliases(t *testing.T) {
	email := strings.TrimSpace(os.Getenv("MAILCOM_TEST_EMAIL"))
	password := os.Getenv("MAILCOM_TEST_PASSWORD")
	if email == "" || password == "" {
		t.Skip("mail.com live credentials are not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	liveHTTP := &http.Client{Timeout: 90 * time.Second, Transport: liveTraceTransport{t: t}}
	client, err := New(Config{}, WithHTTPClient(liveHTTP))
	if err != nil {
		t.Fatal(err)
	}
	aliases, err := client.ListAliases(ctx, Account{Email: email, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mail.com settings OAuth alias list succeeded; aliases=%d", len(aliases))
}

type liveTraceTransport struct {
	t *testing.T
}

func (transport liveTraceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		transport.t.Logf("mail.com HTTP %s %s%s error=%T", request.Method, request.URL.Host, request.URL.EscapedPath(), err)
		return nil, err
	}
	transport.t.Logf("mail.com HTTP %s %s%s status=%d location=%s", request.Method, request.URL.Host, request.URL.EscapedPath(), response.StatusCode, sanitizedRedirectLocation(response.Header.Get("Location")))
	return response, nil
}

func TestMobileOAuthListsAndReadsMusicianOrgMessages(t *testing.T) {
	var serverURL string
	var oauthState string
	var challenge string
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("client_id") != mobileClientID || query.Get("redirect_uri") != mobileRedirectURI || query.Get("login_hint") != "user@musician.org" || query.Get("code_challenge_method") != "S256" {
			t.Fatalf("authorize query=%v", query)
		}
		oauthState = query.Get("state")
		challenge = query.Get("code_challenge")
		w.Header().Set("Location", serverURL+"/loginapp/oauth2?authcode-context=context-1")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/loginapp/oauth2", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "login")
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("username") != "user@musician.org" || r.Form.Get("password") != "mail-password" || r.Form.Get("service") != "oauth2" {
			t.Fatalf("login form=%v err=%v", r.Form, err)
		}
		w.Header().Set("Location", serverURL+"/authcode?authcode-context=context-1")
		w.WriteHeader(http.StatusSeeOther)
	})
	mux.HandleFunc("/authcode", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", mobileRedirectURI+"?code=authorization-code&state="+url.QueryEscape(oauthState))
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != mobileOAuthBasic {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(digest[:]) != challenge || r.Form.Get("code") != "authorization-code" {
				t.Fatalf("token form=%v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "initial-token", "refresh_token": "refresh-token", "expires_in": 300})
		case "refresh_token":
			if r.Form.Get("refresh_token") != "refresh-token" || r.Form.Get("scope") != mobileScope {
				t.Fatalf("refresh form=%v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mail-token", "refresh_token": "refresh-token", "expires_in": 3600})
		default:
			t.Fatalf("grant type=%q", r.Form.Get("grant_type"))
		}
	})
	mux.HandleFunc("/mailbox/Folder/INBOX/Mail", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mail-token" || r.Header.Get("X-Ui-App") != "mailcom.android.androidmail/9.8.0" || r.URL.Query().Get("amount") != "5" {
			t.Fatalf("headers=%v query=%v", r.Header, r.URL.Query())
		}
		fmt.Fprint(w, `{"mail":[{"mailURI":"/Mail/message-1","attribute":{"mailIdentifier":"message-1","internalDate":1710000000000},"mailHeader":{"subject":"Your verification code","from":"OpenAI <noreply@openai.com>","to":["user@musician.org"],"date":1710000000000}}]}`)
	})
	mux.HandleFunc("/mailbox/Mail/message-1/Body", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mail-token" || r.Header.Get("Accept") != "text/plain" {
			t.Fatalf("headers=%v", r.Header)
		}
		fmt.Fprint(w, "Your verification code is 654321")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	serverURL = server.URL

	client, err := New(Config{Endpoints: Endpoints{
		LoginURL: server.URL + "/login", MobileOAuthURL: server.URL, MobileMailboxURL: server.URL + "/mailbox",
	}}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	account := Account{Email: "user@musician.org", Password: "mail-password"}
	messages, err := client.ListMessages(context.Background(), account, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != "message-1" || messages[0].Recipients[0] != "user@musician.org" {
		t.Fatalf("messages=%+v", messages)
	}
	message, err := client.GetMessage(context.Background(), account, messages[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message.Body, "654321") {
		t.Fatalf("body=%q", message.Body)
	}
}
