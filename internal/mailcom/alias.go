package mailcom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
)

// ListAliases 读取 mail.com 账号下当前处于 ACTIVE 状态的邮箱地址。
func (c *Client) ListAliases(ctx context.Context, account Account) ([]string, error) {
	if err := validateAccount(account); err != nil {
		return nil, err
	}
	s := c.getSession(account.Email)
	aliases, err := c.withSettingsRetry(s, func() (any, error) {
		return c.listAliasesLocked(ctx, s, account)
	})
	if err != nil {
		return nil, err
	}
	return aliases.([]string), nil
}

// AddAlias 在 mail.com 账号下真实创建一个新的收件地址。
// mail.com 不支持 plus 别名裂变，子号地址必须先在网页端注册才会投递邮件。
func (c *Client) AddAlias(ctx context.Context, account Account, address string) error {
	if err := validateAccount(account); err != nil {
		return err
	}
	address = strings.ToLower(strings.TrimSpace(address))
	if !strings.Contains(address, "@") {
		return fmt.Errorf("%w: %s", ErrAliasInvalid, address)
	}
	s := c.getSession(account.Email)
	_, err := c.withSettingsRetry(s, func() (any, error) {
		return nil, c.addAliasLocked(ctx, s, account, address)
	})
	return err
}

// withSettingsRetry 在会话失效时重新登录并重试一次。
func (c *Client) withSettingsRetry(s *session, call func() (any, error)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := call()
	if errors.Is(err, ErrSession) {
		s.reset()
		return call()
	}
	return result, err
}

func (c *Client) listAliasesLocked(ctx context.Context, s *session, account Account) ([]string, error) {
	token, err := c.getScopedTokenLocked(ctx, s, account, settingsClientID, settingsScope)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("absoluteURI", "false")
	query.Set("q.state.in", "ACTIVE")
	query.Set("q.type.in", "MANAGED,DOMAIN_HOSTING")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoints.SettingsAddressesURL+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create alias list request: %v", ErrRequest, err)
	}
	setSettingsHeaders(req, token, addressListContentType, addressListContentType)
	response, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: list aliases: %v", ErrRequest, err)
	}
	defer response.Body.Close()
	if err := settingsStatusError(response.StatusCode, http.StatusOK, "list aliases"); err != nil {
		return nil, err
	}
	var payload struct {
		Addresses []struct {
			Address string `json:"address"`
		} `json:"mailaddresslist"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: decode alias list: %v", ErrSettings, err)
	}
	aliases := make([]string, 0, len(payload.Addresses))
	for _, entry := range payload.Addresses {
		if address := strings.ToLower(strings.TrimSpace(entry.Address)); address != "" {
			aliases = append(aliases, address)
		}
	}
	return aliases, nil
}

func (c *Client) addAliasLocked(ctx context.Context, s *session, account Account, address string) error {
	token, err := c.getScopedTokenLocked(ctx, s, account, settingsClientID, settingsScope)
	if err != nil {
		return err
	}
	if err := c.validateAliasLocked(ctx, s, token, address); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"address":                address,
		"deletable":              true,
		"pgpEnabled":             false,
		"defaultSenderAddress":   false,
		"defaultReceiverAddress": false,
		"state":                  "ACTIVE",
	})
	if err != nil {
		return fmt.Errorf("%w: encode alias payload: %v", ErrSettings, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoints.SettingsAddressesURL+"?absoluteURI=false", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: create alias request: %v", ErrRequest, err)
	}
	setSettingsHeaders(req, token, addressCreateContentType, addressCreateContentType)
	response, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: add alias: %v", ErrRequest, err)
	}
	defer response.Body.Close()
	return settingsStatusError(response.StatusCode, http.StatusCreated, "add alias")
}

func (c *Client) validateAliasLocked(ctx context.Context, s *session, token, address string) error {
	body, err := json.Marshal([]string{address})
	if err != nil {
		return fmt.Errorf("%w: encode alias validation: %v", ErrSettings, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoints.SettingsValidationURL+"?absoluteURI=false", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: create alias validation request: %v", ErrRequest, err)
	}
	setSettingsHeaders(req, token, validationRequestContentType, validationAcceptContentType)
	response, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: validate alias: %v", ErrRequest, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return ErrSession
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s status=%d", ErrAliasInvalid, address, response.StatusCode)
	}
	return nil
}

// settingsStatusError 把设置接口状态码映射成语义化错误。
// 403/409/422/429 表示别名配额或频率受限，调用方应停止继续申请而不是重试。
func settingsStatusError(status, expected int, action string) error {
	if status == expected {
		return nil
	}
	switch status {
	case http.StatusUnauthorized:
		return ErrSession
	case http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s status=%d", ErrAliasLimit, action, status)
	default:
		return fmt.Errorf("%w: %s status=%d", ErrSettings, action, status)
	}
}

func setSettingsHeaders(req *http.Request, token, contentType, accept string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", accept)
	req.Header.Set("Origin", settingsOrigin)
	req.Header.Set("Referer", settingsOrigin+"/")
	req.Header.Set("x-ui-app", settingsUIApp)
}

// AliasAddress 基于母号本地部分生成候选子号地址。
// mail.com 不支持 plus 语法，因此拼接为独立的 local part。
func AliasAddress(base, suffix string) string {
	base = strings.TrimSpace(base)
	at := strings.LastIndex(base, "@")
	suffix = strings.TrimSpace(suffix)
	if at <= 0 || suffix == "" {
		return ""
	}
	return strings.ToLower(base[:at]+suffix) + strings.ToLower(base[at:])
}

func RandomAliasSuffix() string {
	const digits = "0123456789"
	const letters = "abcdefghijklmnopqrstuvwxyz"
	result := make([]byte, 6)
	for i := range result {
		if i < 3 {
			result[i] = letters[rand.IntN(len(letters))]
			continue
		}
		result[i] = digits[rand.IntN(len(digits))]
	}
	return string(result)
}
