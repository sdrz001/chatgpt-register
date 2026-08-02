package sub2api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const DefaultSource = "chatgpt-register-codex-oauth"

type OAuthSource struct {
	Email            string `json:"email"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	ExpiresIn        int    `json:"expires_in"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	ChatGPTUserID    string `json:"chatgpt_user_id"`
	PlanType         string `json:"plan_type"`
	Source           string `json:"source,omitempty"`
}

func (source *OAuthSource) UnmarshalJSON(data []byte) error {
	type alias OAuthSource
	var decoded struct {
		alias
		ExpiresIn json.RawMessage `json:"expires_in"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*source = OAuthSource(decoded.alias)
	if len(decoded.ExpiresIn) == 0 || string(decoded.ExpiresIn) == "null" {
		return nil
	}
	var number int
	if err := json.Unmarshal(decoded.ExpiresIn, &number); err == nil {
		source.ExpiresIn = number
		return nil
	}
	var text string
	if err := json.Unmarshal(decoded.ExpiresIn, &text); err == nil {
		parsed, parseErr := strconv.Atoi(strings.TrimSpace(text))
		if parseErr == nil {
			source.ExpiresIn = parsed
			return nil
		}
	}
	source.ExpiresIn = 0
	return nil
}

type Credentials struct {
	AccessToken      string `json:"access_token"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	ChatGPTUserID    string `json:"chatgpt_user_id"`
	Email            string `json:"email"`
	ExpiresAt        string `json:"expires_at"`
	ExpiresIn        int    `json:"expires_in"`
	PlanType         string `json:"plan_type"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
}

type Extra struct {
	Email        string `json:"email"`
	EmailKey     string `json:"email_key"`
	Name         string `json:"name"`
	AuthProvider string `json:"auth_provider"`
	Source       string `json:"source"`
	LastRefresh  string `json:"last_refresh"`
}

type Account struct {
	Name        string      `json:"name"`
	Platform    string      `json:"platform"`
	Type        string      `json:"type"`
	Concurrency int         `json:"concurrency"`
	Priority    int         `json:"priority"`
	Credentials Credentials `json:"credentials"`
	Extra       Extra       `json:"extra"`
}

type Group struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Status   string `json:"status"`
}

type ImportResult struct {
	OK             bool   `json:"ok"`
	Action         string `json:"action"`
	AccountCreated int    `json:"account_created"`
	AccountID      int    `json:"account_id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	GroupIDs       []int  `json:"group_ids"`
}

var emailKeyPattern = regexp.MustCompile(`[^a-z0-9]+`)

func BuildOpenAIOAuthAccount(source OAuthSource, options ...any) (Account, error) {
	concurrency, priority, now, err := parseBuildOptions(options)
	if err != nil {
		return Account{}, err
	}
	email := strings.TrimSpace(source.Email)
	accessToken := strings.TrimSpace(source.AccessToken)
	refreshToken := strings.TrimSpace(source.RefreshToken)
	idToken := strings.TrimSpace(source.IDToken)
	if email == "" {
		return Account{}, newError("OpenAI OAuth 账号缺少邮箱")
	}
	if accessToken == "" {
		return Account{}, newError("OpenAI OAuth 账号缺少 Access Token")
	}
	if refreshToken == "" {
		return Account{}, newError("OpenAI OAuth 账号缺少 Refresh Token")
	}
	concurrency, err = boundedInt(concurrency, 1, 100, "Sub2API 并发数")
	if err != nil {
		return Account{}, err
	}
	priority, err = boundedInt(priority, 1, 100, "Sub2API 优先级")
	if err != nil {
		return Account{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	expiresIn := source.ExpiresIn
	tokenExp := jwtExpiry(accessToken)
	if expiresIn <= 0 && tokenExp.After(now) {
		expiresIn = int(tokenExp.Sub(now) / time.Second)
	}
	expiresAt := tokenExp
	if !tokenExp.After(now) {
		seconds := expiresIn
		if seconds < 1 {
			seconds = 1
		}
		expiresAt = now.Add(time.Duration(seconds) * time.Second)
	}
	if expiresIn < 0 {
		expiresIn = 0
	}
	planType := strings.TrimSpace(source.PlanType)
	if planType == "" {
		planType = "plus"
	}
	sourceName := strings.TrimSpace(source.Source)
	if sourceName == "" {
		sourceName = DefaultSource
	}
	emailKey := strings.Trim(emailKeyPattern.ReplaceAllString(strings.ToLower(email), "_"), "_")
	return Account{
		Name:        email,
		Platform:    "openai",
		Type:        "oauth",
		Concurrency: concurrency,
		Priority:    priority,
		Credentials: Credentials{
			AccessToken:      accessToken,
			ChatGPTAccountID: strings.TrimSpace(source.ChatGPTAccountID),
			ChatGPTUserID:    strings.TrimSpace(source.ChatGPTUserID),
			Email:            email,
			ExpiresAt:        utcMillis(expiresAt),
			ExpiresIn:        expiresIn,
			PlanType:         planType,
			RefreshToken:     refreshToken,
			IDToken:          idToken,
		},
		Extra: Extra{
			Email:        email,
			EmailKey:     emailKey,
			Name:         email,
			AuthProvider: "oauth",
			Source:       sourceName,
			LastRefresh:  utcMillis(now),
		},
	}, nil
}

func parseBuildOptions(options []any) (int, int, time.Time, error) {
	concurrency := DefaultConcurrency
	priority := DefaultPriority
	var now time.Time
	if len(options) > 3 {
		return 0, 0, time.Time{}, newError("OpenAI OAuth 账号构建参数无效")
	}
	for index, option := range options {
		switch index {
		case 0:
			value, ok := option.(int)
			if !ok {
				return 0, 0, time.Time{}, newError("OpenAI OAuth 账号构建参数无效")
			}
			concurrency = value
		case 1:
			value, ok := option.(int)
			if !ok {
				return 0, 0, time.Time{}, newError("OpenAI OAuth 账号构建参数无效")
			}
			priority = value
		case 2:
			value, ok := option.(time.Time)
			if !ok {
				return 0, 0, time.Time{}, newError("OpenAI OAuth 账号构建参数无效")
			}
			now = value
		}
	}
	return concurrency, priority, now, nil
}

func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil {
		return time.Time{}
	}
	exp, err := claims.Exp.Int64()
	if err != nil || exp <= 0 {
		return time.Time{}
	}
	return time.Unix(exp, 0).UTC()
}

func utcMillis(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}
