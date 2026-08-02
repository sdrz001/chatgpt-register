package codexoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	ClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	RedirectURI = "http://localhost:1455/auth/callback"
	Scope       = "openid profile email offline_access"
	AuthBase    = "https://auth.openai.com"
)

type Flow struct {
	Verifier  string
	Challenge string
	State     string
	Nonce     string
	DeviceID  string
}

type Tokens struct {
	Email            string `json:"email"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	ExpiresIn        int    `json:"expires_in"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	ChatGPTUserID    string `json:"chatgpt_user_id"`
	PlanType         string `json:"plan_type"`
	Sub              string `json:"sub"`
}

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

func NewFlow() (Flow, error) {
	verifier, err := randomURLSafe(64)
	if err != nil {
		return Flow{}, err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return Flow{}, err
	}
	nonce, err := randomURLSafe(32)
	if err != nil {
		return Flow{}, err
	}
	device, err := randomUUID()
	if err != nil {
		return Flow{}, err
	}
	digest := sha256.Sum256([]byte(verifier))
	return Flow{
		Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(digest[:]),
		State: state, Nonce: nonce, DeviceID: device,
	}, nil
}

func (f Flow) AuthorizationURL(email string) string {
	query := url.Values{
		"issuer":                {AuthBase},
		"client_id":             {ClientID},
		"audience":              {"https://api.openai.com/v1"},
		"redirect_uri":          {RedirectURI},
		"device_id":             {f.DeviceID},
		"prompt":                {"login"},
		"screen_hint":           {"login"},
		"max_age":               {"0"},
		"login_hint":            {strings.TrimSpace(email)},
		"scope":                 {Scope},
		"response_type":         {"code"},
		"response_mode":         {"query"},
		"state":                 {f.State},
		"nonce":                 {f.Nonce},
		"code_challenge":        {f.Challenge},
		"code_challenge_method": {"S256"},
	}
	return AuthBase + "/api/accounts/authorize?" + query.Encode()
}

func Exchange(ctx context.Context, client HTTPClient, endpoint string, flow Flow, code string) (Tokens, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = AuthBase + "/oauth/token"
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {strings.TrimSpace(code)},
		"redirect_uri": {RedirectURI}, "client_id": {ClientID}, "code_verifier": {flow.Verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, fmt.Errorf("创建 Token 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Tokens{}, fmt.Errorf("Token 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Tokens{}, fmt.Errorf("读取 Token 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Tokens{}, fmt.Errorf("Token 交换失败（HTTP %d）", resp.StatusCode)
	}
	var payload struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		IDToken      string          `json:"id_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Tokens{}, fmt.Errorf("Token 响应格式错误")
	}
	tokens := Tokens{
		AccessToken: strings.TrimSpace(payload.AccessToken), RefreshToken: strings.TrimSpace(payload.RefreshToken),
		IDToken: strings.TrimSpace(payload.IDToken), ExpiresIn: parseExpiresIn(payload.ExpiresIn),
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return Tokens{}, fmt.Errorf("Token 响应缺少 Access Token 或 Refresh Token")
	}
	claims := decodeClaims(tokens.IDToken)
	accessClaims := decodeClaims(tokens.AccessToken)
	profileClaims, _ := claims["https://api.openai.com/profile"].(map[string]any)
	authClaims, _ := accessClaims["https://api.openai.com/auth"].(map[string]any)
	tokens.Email = firstString(claims, "email")
	if tokens.Email == "" {
		tokens.Email = firstString(profileClaims, "email")
	}
	tokens.Sub = firstString(claims, "sub")
	tokens.ChatGPTAccountID = firstString(authClaims, "chatgpt_account_id")
	tokens.ChatGPTUserID = firstString(authClaims, "chatgpt_user_id")
	tokens.PlanType = firstString(authClaims, "chatgpt_plan_type")
	if tokens.PlanType == "" {
		tokens.PlanType = "free"
	}
	return tokens, nil
}

func parseExpiresIn(raw json.RawMessage) int {
	var value int
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value, _ = strconv.Atoi(strings.TrimSpace(text))
	}
	return value
}

func decodeClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if json.Unmarshal(body, &claims) != nil {
		return map[string]any{}
	}
	return claims
}

func firstString(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := claims[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func randomURLSafe(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func randomUUID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buffer[0:4], buffer[4:6], buffer[6:8], buffer[8:10], buffer[10:16]), nil
}
