package mailcom

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAutomaticOAuthSecretDiscoveryUsesHTTPAssets(t *testing.T) {
	var configRequests atomic.Int32
	var baseURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/config/", func(w http.ResponseWriter, r *http.Request) {
		configRequests.Add(1)
		fmt.Fprint(w, `<script type="module" src="/config/build/root.js"></script>`)
	})
	mux.HandleFunc("/config/build/root.js", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `import{a}from"./auth.js";`)
	})
	mux.HandleFunc("/config/build/auth.js", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `const authentication={clientId:"mailcom_mailset_root_live",clientSecret:"discovered-secret"};`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL = server.URL
	_ = baseURL

	client, err := New(Config{Endpoints: Endpoints{OAuthConfigURL: server.URL + "/config/"}}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := client.oauthPublicSecret(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if secret != "discovered-secret" {
		t.Fatalf("secret=%q", secret)
	}
	if configRequests.Load() != 1 {
		t.Fatalf("config requests=%d", configRequests.Load())
	}
}

func TestAutomaticOAuthSecretDiscoveryIsSharedAcrossClients(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `const authentication={clientSecret:"shared-secret"};`)
	}))
	defer server.Close()
	configURL := server.URL + "/"
	invalidateOAuthPublicSecret(configURL)

	const count = 12
	var wait sync.WaitGroup
	wait.Add(count)
	for range count {
		go func() {
			defer wait.Done()
			client, err := New(Config{Endpoints: Endpoints{OAuthConfigURL: configURL}}, WithHTTPClient(server.Client()))
			if err != nil {
				t.Error(err)
				return
			}
			secret, err := client.oauthPublicSecret(context.Background(), false)
			if err != nil || secret != "shared-secret" {
				t.Errorf("secret=%q err=%v", secret, err)
			}
		}()
	}
	wait.Wait()
	if requests.Load() != 1 {
		t.Fatalf("requests=%d", requests.Load())
	}
}

func TestOAuthRequestUsesExplicitWebSecret(t *testing.T) {
	token := testToken(t, map[string]any{"exp": 4102444800, "auth_id": "auth-1"})
	var baseURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<input name="statistics" value="stats-1">`)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("username") != "user@musician.org" || r.Form.Get("password") != "mail-password" {
			t.Fatalf("login form=%v err=%v", r.Form, err)
		}
		w.Header().Set("Location", baseURL+"/halogin?ott=one-time-token")
		w.WriteHeader(http.StatusSeeOther)
	})
	mux.HandleFunc("/halogin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", baseURL+"/mailbox?sid=session-1")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/oauth", func(w http.ResponseWriter, r *http.Request) {
		encoded := strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || string(decoded) != mailClientID+":protocol-secret" {
			t.Fatalf("authorization=%q decoded=%q err=%v", encoded, decoded, err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token})
	})
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"mailListElements":[]}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	baseURL = server.URL

	client, err := New(Config{OAuthPublicSecret: "protocol-secret", Endpoints: Endpoints{
		LoginPageURL: server.URL + "/", LoginURL: server.URL + "/login", OAuthURL: server.URL + "/oauth",
		MailListURL: server.URL + "/list",
	}}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListMessages(context.Background(), Account{Email: "user@musician.org", Password: "mail-password"}, 1); err != nil {
		t.Fatal(err)
	}
}

func TestMailComDomainRecognitionAndAlias(t *testing.T) {
	if !IsAddress("User@Musician.org") || !IsAddress("user@mail.com") || IsAddress("user@example.org") {
		t.Fatal("mail.com domain recognition mismatch")
	}
	if got := AliasAddress("Rivers_Prattyor@musician.org", "abc123"); got != "rivers_prattyorabc123@musician.org" {
		t.Fatalf("alias=%q", got)
	}
}
