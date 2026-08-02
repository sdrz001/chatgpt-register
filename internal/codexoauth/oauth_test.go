package codexoauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func TestNewFlowAndAuthorizationURL(t *testing.T) {
	flow, err := NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	if flow.Verifier == "" || flow.State == "" || flow.Nonce == "" || flow.DeviceID == "" {
		t.Fatalf("flow=%+v", flow)
	}
	digest := sha256.Sum256([]byte(flow.Verifier))
	if flow.Challenge != base64.RawURLEncoding.EncodeToString(digest[:]) {
		t.Fatal("PKCE challenge mismatch")
	}
	parsed, err := url.Parse(flow.AuthorizationURL(" user@example.test "))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"client_id": ClientID, "redirect_uri": RedirectURI, "scope": Scope,
		"response_type": "code", "code_challenge_method": "S256", "login_hint": "user@example.test",
		"state": flow.State, "nonce": flow.Nonce, "device_id": flow.DeviceID, "code_challenge": flow.Challenge,
	} {
		if query.Get(key) != want {
			t.Fatalf("%s=%q want %q", key, query.Get(key), want)
		}
	}
}

func TestExchangePostsPKCEAndDecodesClaims(t *testing.T) {
	accessToken := testJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-id", "chatgpt_user_id": "user-id", "chatgpt_plan_type": "plus",
		},
	})
	idToken := testJWT(t, map[string]any{"sub": "sub-id", "email": "user@example.test"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("request=%s content-type=%q", r.Method, r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"grant_type": "authorization_code", "code": "auth-code", "redirect_uri": RedirectURI,
			"client_id": ClientID, "code_verifier": "verifier",
		} {
			if r.Form.Get(key) != want {
				t.Errorf("%s=%q want %q", key, r.Form.Get(key), want)
			}
		}
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"refresh-token","id_token":%q,"expires_in":"3600"}`, accessToken, idToken)
	}))
	defer server.Close()

	tokens, err := Exchange(context.Background(), server.Client(), server.URL, Flow{Verifier: "verifier"}, "auth-code")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != accessToken || tokens.RefreshToken != "refresh-token" || tokens.IDToken != idToken || tokens.ExpiresIn != 3600 {
		t.Fatalf("tokens=%+v", tokens)
	}
	if tokens.Email != "user@example.test" || tokens.Sub != "sub-id" || tokens.ChatGPTAccountID != "account-id" || tokens.ChatGPTUserID != "user-id" || tokens.PlanType != "plus" {
		t.Fatalf("claims=%+v", tokens)
	}
}

func TestExchangeErrorsDoNotEchoResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"secret-response"}`)
	}))
	defer server.Close()
	_, err := Exchange(context.Background(), server.Client(), server.URL, Flow{Verifier: "verifier"}, "code")
	if err == nil || strings.Contains(err.Error(), "secret-response") {
		t.Fatalf("error=%v", err)
	}
}
