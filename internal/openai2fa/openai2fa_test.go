package openai2fa

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestTOTPRFC6238Vector(t *testing.T) {
	code, err := TOTP("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(59, 0))
	if err != nil {
		t.Fatal(err)
	}
	if code != "287082" {
		t.Fatalf("code=%q", code)
	}
}

func TestEnableEnrollsActivatesAndVerifies(t *testing.T) {
	var calls []string
	var activation map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		if request.Header.Get("Authorization") != "Bearer ACCESS" || request.Header.Get("OAI-Device-ID") != "DEVICE" {
			t.Fatalf("headers=%v", request.Header)
		}
		if cookie, err := request.Cookie("__Secure-next-auth.session-token"); err != nil || cookie.Value != "SESSION" {
			t.Fatalf("cookie=%v error=%v", cookie, err)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch len(calls) {
		case 1:
			_, _ = writer.Write([]byte(`{"mfa_enabled":false}`))
		case 2:
			_, _ = writer.Write([]byte(`{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"ENROLL","factor":{"id":"FACTOR"}}`))
		case 3:
			if err := json.NewDecoder(request.Body).Decode(&activation); err != nil {
				t.Fatal(err)
			}
			_, _ = writer.Write([]byte(`{"success":true,"recovery_codes":["RECOVERY-1"]}`))
		case 4:
			_, _ = writer.Write([]byte(`{"mfa_enabled":true,"native_default_factor_id":"FACTOR"}`))
		default:
			t.Fatalf("unexpected call %d", len(calls))
		}
	}))
	defer server.Close()

	client := &Client{
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Now:        func() time.Time { return time.Unix(59, 0) },
	}
	result, err := client.Enable(context.Background(), Session{
		AccessToken: "ACCESS",
		DeviceID:    "DEVICE",
		Cookies:     []Cookie{{Name: "__Secure-next-auth.session-token", Value: "SESSION"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"GET /backend-api/accounts/mfa_info",
		"POST /backend-api/accounts/mfa/enroll",
		"POST /backend-api/accounts/mfa/user/activate_enrollment",
		"GET /backend-api/accounts/mfa_info",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls=%v", calls)
	}
	if activation["code"] != "287082" || activation["session_id"] != "ENROLL" || activation["factor_type"] != "totp" {
		t.Fatalf("activation=%v", activation)
	}
	if result.Secret != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" || result.FactorID != "FACTOR" || !reflect.DeepEqual(result.RecoveryCodes, []string{"RECOVERY-1"}) {
		t.Fatalf("result=%+v", result)
	}
}

func TestEnableRejectsAlreadyEnabledWithoutSecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"mfa_enabled":true}`))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTPClient: server.Client()}
	if _, err := client.Enable(context.Background(), Session{AccessToken: "ACCESS"}); err == nil {
		t.Fatal("expected already-enabled error")
	}
}
