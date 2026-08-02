package codexreg

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
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
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", "", fmt.Errorf("invalid JWT format")
	}
	payload := parts[1]
	if rem := len(payload) % 4; rem != 0 {
		payload += strings.Repeat("=", 4-rem)
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return "", "", "", "", fmt.Errorf("base64 decode: %w", err)
	}
	var claims map[string]any
	if err = json.Unmarshal(decoded, &claims); err != nil {
		return "", "", "", "", fmt.Errorf("json unmarshal: %w", err)
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
	if planType == "" {
		planType = "free"
	}
	return
}
