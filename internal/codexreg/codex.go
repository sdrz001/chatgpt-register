package codexreg

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// buildAuthFromToken 拿到 accessToken 后解码 JWT 账号信息，组装 auth.json。
// Agent Identity 注册接口已失效（agent_registry_not_enabled），拿到 AT 即视为注册成功。
func buildAuthFromToken(in Input, accessToken string) (map[string]any, string, string, string, error) {
	in.logf("📋 解码 JWT 获取账号信息...")
	accountID, userID, email, planType, err := decodeJWTClaims(accessToken)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("JWT 解码失败: %w", err)
	}
	if email == "" {
		email = in.Email
	}
	in.logf("✅ 已获取 accessToken，account_id=%s plan=%s", accountID, planType)

	auth := map[string]any{
		"auth_mode":       "access_token",
		"access_token":    accessToken,
		"account_id":      accountID,
		"chatgpt_user_id": userID,
		"email":           email,
		"plan_type":       planType,
	}
	return auth, accountID, userID, planType, nil
}

// decodeJWTClaims 解码 JWT payload（不验证签名），提取账号信息。
func decodeJWTClaims(token string) (accountID, userID, email, planType string, err error) {
	claims, err := AccessTokenClaims(token)
	if err != nil {
		return "", "", "", "", err
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
	if auth != nil {
		accountID, _ = auth["chatgpt_account_id"].(string)
		userID, _ = auth["chatgpt_user_id"].(string)
		planType, _ = auth["chatgpt_plan_type"].(string)
	}
	if profile != nil {
		email, _ = profile["email"].(string)
	}
	planType = NormalizePlanType(planType)
	if planType == "" {
		planType = "free"
	}
	return
}

func AccessTokenClaims(token string) (map[string]any, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}
	return claims, nil
}

func AccessTokenDetails(token string) (planType string, expiresAt *time.Time, err error) {
	claims, err := AccessTokenClaims(token)
	if err != nil {
		return "", nil, err
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		planType, _ = auth["chatgpt_plan_type"].(string)
	}
	if exp, ok := claims["exp"].(float64); ok && exp > 0 {
		value := time.Unix(int64(exp), 0).UTC()
		expiresAt = &value
	}
	return NormalizePlanType(planType), expiresAt, nil
}

func NormalizePlanType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(value, "enterprise"):
		return "enterprise"
	case strings.Contains(value, "business"):
		return "business"
	case strings.Contains(value, "team"):
		return "team"
	case strings.Contains(value, "edu"):
		return "edu"
	case strings.Contains(value, "plus"):
		return "plus"
	case value == "pro" || strings.HasPrefix(value, "pro_") || strings.HasSuffix(value, "_pro"):
		return "pro"
	case value == "go" || strings.Contains(value, "chatgptgo"):
		return "go"
	case strings.Contains(value, "free"):
		return "free"
	default:
		return value
	}
}
