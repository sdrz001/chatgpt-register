package handlers

import (
	"encoding/json"
	"testing"
)

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
