package sub2api

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func oauthSource() OAuthSource {
	return OAuthSource{
		Email:            " user@example.test ",
		AccessToken:      "synthetic-access-token",
		RefreshToken:     "synthetic-refresh-token",
		IDToken:          "synthetic-id-token",
		ExpiresIn:        3600,
		ChatGPTAccountID: " account-synthetic ",
		ChatGPTUserID:    " user-synthetic ",
		PlanType:         " plus ",
	}
}

func jwtWithExpiry(t *testing.T, exp int64) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp})
	if err != nil {
		t.Fatal(err)
	}
	return "synthetic." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestBuildOpenAIOAuthAccountDefaults(t *testing.T) {
	account, err := BuildOpenAIOAuthAccount(oauthSource())
	if err != nil {
		t.Fatal(err)
	}
	if account.Concurrency != DefaultConcurrency || account.Priority != DefaultPriority || account.Extra.Source != DefaultSource {
		t.Fatalf("defaults = %+v", account)
	}
}

func TestBuildOpenAIOAuthAccountJWTExpiryAndShape(t *testing.T) {
	now := time.Unix(1_900_000_000, 123_456_789)
	source := oauthSource()
	source.AccessToken = jwtWithExpiry(t, 2_000_000_000)
	source.ExpiresIn = 60
	account, err := BuildOpenAIOAuthAccount(source, 17, 3, now)
	if err != nil {
		t.Fatalf("BuildOpenAIOAuthAccount() error = %v", err)
	}
	if account.Platform != "openai" || account.Type != "oauth" || account.Concurrency != 17 || account.Priority != 3 {
		t.Fatalf("account metadata = %+v", account)
	}
	if account.Credentials.ExpiresAt != "2033-05-18T03:33:20.000Z" || account.Credentials.ExpiresIn != 60 {
		t.Fatalf("expiry = %q/%d", account.Credentials.ExpiresAt, account.Credentials.ExpiresIn)
	}
	if account.Extra.LastRefresh != "2030-03-17T17:46:40.123Z" {
		t.Fatalf("last_refresh = %q", account.Extra.LastRefresh)
	}
	if account.Extra.EmailKey != "user_example_test" || account.Extra.Source != DefaultSource {
		t.Fatalf("extra = %+v", account.Extra)
	}
	encoded, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	credentials := object["credentials"].(map[string]any)
	wantCredentialKeys := []string{"access_token", "chatgpt_account_id", "chatgpt_user_id", "email", "expires_at", "expires_in", "plan_type", "refresh_token", "id_token"}
	gotKeys := make([]string, 0, len(credentials))
	for key := range credentials {
		gotKeys = append(gotKeys, key)
	}
	if !sameStringSet(gotKeys, wantCredentialKeys) {
		t.Fatalf("credential keys = %v, want %v", gotKeys, wantCredentialKeys)
	}
}

func TestBuildOpenAIOAuthAccountExpiryFallbacks(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	tests := []struct {
		name          string
		accessToken   string
		expiresIn     int
		wantExpiresAt string
		wantExpiresIn int
	}{
		{"expires in", "not-a-jwt", 3600, "2030-03-17T18:46:40.000Z", 3600},
		{"invalid JWT", "synthetic.%%%invalid%%%.signature", 120, "2030-03-17T17:48:40.000Z", 120},
		{"derive expires in", jwtWithExpiry(t, 1_900_000_090), 0, "2030-03-17T17:48:10.000Z", 90},
		{"minimum fallback", "not-a-jwt", -4, "2030-03-17T17:46:41.000Z", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := oauthSource()
			source.AccessToken = test.accessToken
			source.ExpiresIn = test.expiresIn
			account, err := BuildOpenAIOAuthAccount(source, 10, 1, now)
			if err != nil {
				t.Fatal(err)
			}
			if account.Credentials.ExpiresAt != test.wantExpiresAt || account.Credentials.ExpiresIn != test.wantExpiresIn {
				t.Fatalf("expiry = %q/%d", account.Credentials.ExpiresAt, account.Credentials.ExpiresIn)
			}
		})
	}
}

func TestOAuthSourceUnmarshalExpiresIn(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
	}{
		{`120`, 120},
		{`"90"`, 90},
		{`"invalid"`, 0},
	} {
		var source OAuthSource
		input := []byte(`{"email":"user@example.test","expires_in":` + test.value + `}`)
		if err := json.Unmarshal(input, &source); err != nil {
			t.Fatalf("Unmarshal(%s) error = %v", test.value, err)
		}
		if source.ExpiresIn != test.want {
			t.Fatalf("Unmarshal(%s) ExpiresIn = %d, want %d", test.value, source.ExpiresIn, test.want)
		}
	}
}

func TestBuildOpenAIOAuthAccountRequiredFieldsAndSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*OAuthSource)
		want   string
	}{
		{"email", func(s *OAuthSource) { s.Email = "" }, "邮箱"},
		{"access", func(s *OAuthSource) { s.AccessToken = "" }, "Access Token"},
		{"refresh", func(s *OAuthSource) { s.RefreshToken = "" }, "Refresh Token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := oauthSource()
			test.mutate(&source)
			if _, err := BuildOpenAIOAuthAccount(source, 10, 1, time.Now()); err == nil {
				t.Fatalf("BuildOpenAIOAuthAccount() returned nil error, want %s", test.want)
			}
		})
	}
	source := oauthSource()
	source.Source = "custom-codex-oauth"
	account, err := BuildOpenAIOAuthAccount(source, 10, 1, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if account.Extra.Source != "custom-codex-oauth" {
		t.Fatalf("source = %q", account.Extra.Source)
	}
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftSet := make(map[string]struct{}, len(left))
	for _, item := range left {
		leftSet[item] = struct{}{}
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, item := range right {
		rightSet[item] = struct{}{}
	}
	return reflect.DeepEqual(leftSet, rightSet)
}
