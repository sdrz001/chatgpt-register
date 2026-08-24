package openai2fa

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const (
	baseURL      = "https://chatgpt.com"
	mfaInfoPath  = "/backend-api/accounts/mfa_info"
	enrollPath   = "/backend-api/accounts/mfa/enroll"
	activatePath = "/backend-api/accounts/mfa/user/activate_enrollment"
)

type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

type Session struct {
	AccessToken string
	DeviceID    string
	Cookies     []Cookie
	Proxy       string
}

type Result struct {
	Secret        string
	RecoveryCodes []string
	FactorID      string
}

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Now        func() time.Time
}

func Enable(ctx context.Context, session Session) (Result, error) {
	client, err := NewClient(session.Proxy)
	if err != nil {
		return Result{}, err
	}
	return client.Enable(ctx, session)
}

func NewClient(rawProxy string) (*Client, error) {
	transport, err := proxyTransport(rawProxy)
	if err != nil {
		return nil, err
	}
	return &Client{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		Now: time.Now,
	}, nil
}

func (c *Client) Enable(ctx context.Context, session Session) (Result, error) {
	token := strings.TrimSpace(session.AccessToken)
	if token == "" {
		return Result{}, fmt.Errorf("missing ChatGPT Web access token")
	}
	deviceID := strings.TrimSpace(session.DeviceID)
	if deviceID == "" {
		deviceID = randomHex(16)
	}
	before, err := c.request(ctx, http.MethodGet, mfaInfoPath, token, deviceID, session.Cookies, nil)
	if err != nil {
		return Result{}, err
	}
	if mfaEnabled(before) {
		return Result{}, fmt.Errorf("2FA is already enabled but no TOTP secret is available")
	}
	enroll, err := c.request(ctx, http.MethodPost, enrollPath, token, deviceID, session.Cookies, map[string]any{"factor_type": "totp"})
	if err != nil {
		return Result{}, err
	}
	secret, _ := enroll["secret"].(string)
	secret = normalizeSecret(secret)
	if secret == "" {
		return Result{}, fmt.Errorf("2FA enrollment returned no TOTP secret")
	}
	sessionID, _ := enroll["session_id"].(string)
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Result{}, fmt.Errorf("2FA enrollment returned no session ID")
	}
	factor, _ := enroll["factor"].(map[string]any)
	factorID, _ := factor["id"].(string)
	code, err := TOTP(secret, c.now())
	if err != nil {
		return Result{}, err
	}
	activated, err := c.request(ctx, http.MethodPost, activatePath, token, deviceID, session.Cookies, map[string]any{
		"code": code, "factor_type": "totp", "session_id": sessionID,
	})
	if err != nil {
		return Result{}, err
	}
	if success, exists := activated["success"].(bool); exists && !success {
		return Result{}, fmt.Errorf("2FA activation was rejected")
	}
	after, err := c.request(ctx, http.MethodGet, mfaInfoPath, token, deviceID, session.Cookies, nil)
	if err != nil {
		return Result{}, err
	}
	if !mfaEnabled(after) {
		return Result{}, fmt.Errorf("2FA activation was not confirmed")
	}
	if value, _ := after["native_default_factor_id"].(string); strings.TrimSpace(value) != "" {
		factorID = value
	}
	return Result{Secret: secret, RecoveryCodes: recoveryCodes(enroll, activated, after), FactorID: strings.TrimSpace(factorID)}, nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) request(ctx context.Context, method, path, token, deviceID string, cookies []Cookie, body map[string]any) (map[string]any, error) {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + path
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		var payload io.Reader
		if encoded != nil {
			payload = strings.NewReader(string(encoded))
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("OAI-Device-ID", deviceID)
		req.Header.Set("Origin", "https://chatgpt.com")
		req.Header.Set("Referer", "https://chatgpt.com/#settings/Security")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/150.0.0.0 Safari/537.36")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for _, cookie := range cookies {
			if strings.TrimSpace(cookie.Name) != "" && cookie.Value != "" {
				req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
			}
		}
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			lastErr = err
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
				continue
			}
			return nil, err
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode >= 500 && attempt < 3 {
			time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
			continue
		}
		var result map[string]any
		if err := json.Unmarshal(responseBody, &result); err != nil {
			result = map[string]any{}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return result, nil
		}
		detail := strings.TrimSpace(string(responseBody))
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return nil, fmt.Errorf("2FA %s failed: HTTP %d: %s", path, resp.StatusCode, detail)
	}
	return nil, fmt.Errorf("2FA request failed: %v", lastErr)
}

func TOTP(secret string, at time.Time) (string, error) {
	normalized := normalizeSecret(secret)
	if normalized == "" {
		return "", fmt.Errorf("empty TOTP secret")
	}
	padding := strings.Repeat("=", (8-len(normalized)%8)%8)
	key, err := base32.StdEncoding.DecodeString(normalized + padding)
	if err != nil {
		return "", fmt.Errorf("decode TOTP secret: %w", err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	number := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", number%1_000_000), nil
}

func normalizeSecret(secret string) string {
	var result strings.Builder
	for _, char := range strings.ToUpper(secret) {
		if char >= 'A' && char <= 'Z' || char >= '2' && char <= '7' {
			result.WriteRune(char)
		}
	}
	return result.String()
}

func mfaEnabled(info map[string]any) bool {
	for _, key := range []string{"mfa_enabled", "mfa_enabled_v2"} {
		if enabled, _ := info[key].(bool); enabled {
			return true
		}
	}
	return false
}

func recoveryCodes(values ...map[string]any) []string {
	seen := map[string]struct{}{}
	var result []string
	var visit func(any, string)
	visit = func(value any, key string) {
		switch item := value.(type) {
		case map[string]any:
			for childKey, child := range item {
				visit(child, strings.ToLower(childKey))
			}
		case []any:
			for _, child := range item {
				visit(child, key)
			}
		default:
			if !strings.Contains(key, "recovery") && !strings.Contains(key, "backup") {
				return
			}
			text := strings.TrimSpace(fmt.Sprint(item))
			if text == "" {
				return
			}
			if _, exists := seen[text]; !exists {
				seen[text] = struct{}{}
				result = append(result, text)
			}
		}
	}
	for _, value := range values {
		visit(value, "")
	}
	return result
}

func randomHex(size int) string {
	buffer := make([]byte, size)
	if _, err := cryptorand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(buffer)*2)
	for index, value := range buffer {
		encoded[index*2] = digits[value>>4]
		encoded[index*2+1] = digits[value&0x0f]
	}
	return string(encoded)
}

func proxyTransport(rawProxy string) (*http.Transport, error) {
	transport := &http.Transport{}
	raw := strings.TrimSpace(rawProxy)
	if raw == "" {
		return transport, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid proxy")
	}
	if parsed.Scheme != "socks5" {
		transport.Proxy = http.ProxyURL(parsed)
		return transport, nil
	}
	var auth *xproxy.Auth
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		auth = &xproxy.Auth{User: parsed.User.Username(), Password: password}
	}
	dialer, err := xproxy.SOCKS5("tcp", parsed.Host, auth, xproxy.Direct)
	if err != nil {
		return nil, err
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.Dial(network, address)
	}
	return transport, nil
}
