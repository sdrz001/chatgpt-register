package sub2api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type Client struct {
	config Config
	http   HTTPClient
	now    func() time.Time
}

var importLocks [64]sync.Mutex

func NewClient(config Config, httpClients ...HTTPClient) (*Client, error) {
	normalized, err := config.Validated()
	if err != nil {
		return nil, err
	}
	if len(httpClients) > 1 {
		return nil, newError("Sub2API HTTP Client 参数无效")
	}
	var httpClient HTTPClient
	if len(httpClients) == 1 {
		httpClient = httpClients[0]
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: time.Duration(normalized.Timeout) * time.Second}
	}
	return &Client{config: normalized, http: httpClient, now: time.Now}, nil
}

func ListGroups(ctx context.Context, config Config, httpClients ...HTTPClient) ([]Group, error) {
	client, err := NewClient(config, httpClients...)
	if err != nil {
		return nil, err
	}
	groups, err := client.ListGroups(ctx)
	if err != nil {
		return nil, wrapRedacted(err, client.config.APIKey)
	}
	return groups, nil
}

func ImportOpenAIOAuthAccount(ctx context.Context, source OAuthSource, config Config, httpClients ...HTTPClient) (ImportResult, error) {
	client, err := NewClient(config, httpClients...)
	if err != nil {
		return ImportResult{}, wrapRedacted(err, config.APIKey, source.Email, source.AccessToken, source.RefreshToken, source.IDToken)
	}
	account, err := BuildOpenAIOAuthAccount(source, client.config.Concurrency, client.config.Priority, client.now())
	if err != nil {
		return ImportResult{}, wrapRedacted(err, client.config.APIKey, source.Email, source.AccessToken, source.RefreshToken, source.IDToken)
	}
	secrets := append([]string{client.config.APIKey, account.Name}, sensitiveValues(account)...)
	lock := lockForEmail(account.Name)
	lock.Lock()
	defer lock.Unlock()
	result, err := client.ImportOpenAIOAuthAccount(ctx, account)
	if err != nil {
		return ImportResult{}, wrapRedacted(err, secrets...)
	}
	return result, nil
}

func lockForEmail(email string) *sync.Mutex {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return &importLocks[int(digest[0])%len(importLocks)]
}

func (c *Client) ListGroups(ctx context.Context) ([]Group, error) {
	groups := make(map[int]Group)
	for page := 1; ; page++ {
		query := url.Values{
			"page":      {strconv.Itoa(page)},
			"page_size": {"200"},
		}
		body, err := c.request(ctx, http.MethodGet, "/api/v1/admin/groups", query, nil, http.StatusOK, http.StatusCreated)
		if err != nil {
			return nil, err
		}
		items := accountItems(body)
		added := 0
		for _, item := range items {
			id, ok := positiveInt(item["id"])
			name := strings.TrimSpace(stringValue(item["name"]))
			if !ok || name == "" {
				continue
			}
			if _, exists := groups[id]; !exists {
				added++
			}
			groups[id] = Group{
				ID:       id,
				Name:     name,
				Platform: strings.TrimSpace(stringValue(item["platform"])),
				Status:   strings.TrimSpace(stringValue(item["status"])),
			}
		}
		if len(items) == 0 || added == 0 {
			break
		}
	}
	ids := make([]int, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	result := make([]Group, 0, len(ids))
	for _, id := range ids {
		result = append(result, groups[id])
	}
	return result, nil
}

func (c *Client) ListOpenAIOAuthAccounts(ctx context.Context, email string) ([]map[string]any, error) {
	target := strings.TrimSpace(email)
	if target == "" {
		return []map[string]any{}, nil
	}
	matches := make(map[string]map[string]any)
	seenPages := make(map[string]struct{})
	for page := 1; ; page++ {
		query := url.Values{
			"page":       {strconv.Itoa(page)},
			"page_size":  {"100"},
			"platform":   {"openai"},
			"type":       {"oauth"},
			"search":     {email},
			"sort_by":    {"created_at"},
			"sort_order": {"desc"},
		}
		body, err := c.requestWithSecrets(ctx, http.MethodGet, "/api/v1/admin/accounts", query, nil, []string{email}, http.StatusOK)
		if err != nil {
			return nil, err
		}
		items := accountItems(body)
		if len(items) == 0 {
			break
		}
		pageJSON, _ := json.Marshal(items)
		pageDigest := sha256.Sum256(pageJSON)
		pageKey := hex.EncodeToString(pageDigest[:])
		if _, exists := seenPages[pageKey]; exists {
			break
		}
		seenPages[pageKey] = struct{}{}
		for _, item := range items {
			if !strings.EqualFold(strings.TrimSpace(accountEmail(item)), target) ||
				!strings.EqualFold(strings.TrimSpace(stringValue(item["platform"])), "openai") ||
				!strings.EqualFold(strings.TrimSpace(stringValue(item["type"])), "oauth") {
				continue
			}
			identity := stringValue(item["id"])
			if identity == "" {
				identity = pageKey
			}
			matches[identity] = item
		}
	}
	keys := make([]string, 0, len(matches))
	for key := range matches {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		result = append(result, matches[key])
	}
	return result, nil
}

func (c *Client) GetAccount(ctx context.Context, accountID int) (map[string]any, error) {
	body, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/api/v1/admin/accounts/%d", accountID), nil, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, newError("Sub2API 账号响应格式无效")
	}
	if object, ok := decoded.(map[string]any); ok {
		if data, exists := object["data"]; exists {
			decoded = data
		}
	}
	account, ok := decoded.(map[string]any)
	if !ok {
		return nil, newError("Sub2API 账号响应格式无效")
	}
	return account, nil
}

func (c *Client) ImportOpenAIOAuthAccount(ctx context.Context, account Account) (ImportResult, error) {
	secrets := append([]string{c.config.APIKey, account.Name}, sensitiveValues(account)...)
	result, err := c.importOpenAIOAuthAccount(ctx, account)
	if err != nil {
		return ImportResult{}, wrapRedacted(err, secrets...)
	}
	return result, nil
}

func (c *Client) importOpenAIOAuthAccount(ctx context.Context, account Account) (ImportResult, error) {
	email := strings.TrimSpace(account.Name)
	existing, err := c.ListOpenAIOAuthAccounts(ctx, email)
	if err != nil {
		return ImportResult{}, err
	}
	if len(existing) > 1 {
		return ImportResult{}, newError("Sub2API 已存在多个同名 OpenAI OAuth 账号")
	}
	action := "updated"
	created := 0
	accountID := 0
	if len(existing) == 1 {
		var ok bool
		accountID, ok = positiveInt(existing[0]["id"])
		if !ok {
			return ImportResult{}, newError("Sub2API 已有账号缺少有效 ID")
		}
		payload := map[string]any{
			"name":        account.Name,
			"type":        "oauth",
			"credentials": account.Credentials,
			"extra":       account.Extra,
			"concurrency": account.Concurrency,
			"priority":    account.Priority,
			"status":      "active",
		}
		if len(c.config.GroupIDs) > 0 {
			payload["group_ids"] = c.config.GroupIDs
		}
		if _, err := c.request(ctx, http.MethodPut, fmt.Sprintf("/api/v1/admin/accounts/%d", accountID), nil, payload, http.StatusOK, http.StatusCreated); err != nil {
			return ImportResult{}, err
		}
	} else {
		payload := map[string]any{
			"data": map[string]any{
				"exported_at": utcMillis(c.now()),
				"proxies":     []any{},
				"accounts":    []Account{account},
			},
			"skip_default_group_bind": false,
		}
		body, err := c.request(ctx, http.MethodPost, "/api/v1/admin/accounts/data", nil, payload, http.StatusOK, http.StatusCreated)
		if err != nil {
			return ImportResult{}, err
		}
		result := dataObject(body)
		created, _ = anyInt(result["account_created"])
		failed, _ := anyInt(result["account_failed"])
		if created != 1 || failed != 0 {
			detail := ""
			if errorsValue, exists := result["errors"]; exists {
				if errorsList, ok := errorsValue.([]any); ok {
					detail = fmt.Sprint(errorsList)
				}
			}
			message := fmt.Sprintf("Sub2API 数据导入结果异常：created=%d/1, failed=%d", created, failed)
			if detail != "" {
				message += ", errors=" + detail
			}
			return ImportResult{}, newError(message)
		}
		after, err := c.ListOpenAIOAuthAccounts(ctx, email)
		if err != nil {
			return ImportResult{}, err
		}
		if len(after) != 1 {
			return ImportResult{}, newError("Sub2API 已创建账号，但新账号定位结果不唯一")
		}
		var ok bool
		accountID, ok = positiveInt(after[0]["id"])
		if !ok {
			return ImportResult{}, newError("Sub2API 新账号缺少有效 ID")
		}
		action = "imported"
		if len(c.config.GroupIDs) > 0 {
			if _, err := c.request(ctx, http.MethodPut, fmt.Sprintf("/api/v1/admin/accounts/%d", accountID), nil, map[string]any{"group_ids": c.config.GroupIDs}, http.StatusOK, http.StatusCreated); err != nil {
				return ImportResult{}, err
			}
		}
	}
	verified, err := c.GetAccount(ctx, accountID)
	if err != nil {
		return ImportResult{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(stringValue(verified["platform"])), "openai") ||
		!strings.EqualFold(strings.TrimSpace(stringValue(verified["type"])), "oauth") {
		return ImportResult{}, newError("Sub2API 账号类型校验失败")
	}
	if !strings.EqualFold(strings.TrimSpace(accountEmail(verified)), email) {
		return ImportResult{}, newError("Sub2API 账号邮箱校验失败")
	}
	verifiedGroups, err := accountGroupIDs(verified)
	if err != nil {
		return ImportResult{}, err
	}
	if len(c.config.GroupIDs) > 0 && !containsAll(verifiedGroups, c.config.GroupIDs) {
		return ImportResult{}, newError("Sub2API 账号分类校验失败")
	}
	name := strings.TrimSpace(stringValue(verified["name"]))
	if name == "" {
		name = email
	}
	return ImportResult{
		OK:             true,
		Action:         action,
		AccountCreated: created,
		AccountID:      accountID,
		Name:           name,
		Status:         stringValue(verified["status"]),
		GroupIDs:       verifiedGroups,
	}, nil
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, payload any, expected ...int) (json.RawMessage, error) {
	return c.requestWithSecrets(ctx, method, path, query, payload, nil, expected...)
}

func (c *Client) requestWithSecrets(ctx context.Context, method, path string, query url.Values, payload any, extraSecrets []string, expected ...int) (json.RawMessage, error) {
	endpoint := c.config.URL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var body io.Reader
	secrets := append([]string{c.config.APIKey}, extraSecrets...)
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, newError("Sub2API 请求数据编码失败")
		}
		body = bytes.NewReader(encoded)
		secrets = append(secrets, sensitiveValues(payload)...)
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(c.config.Timeout)*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, endpoint, body)
	if err != nil {
		return nil, newError("Sub2API 请求创建失败")
	}
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", c.config.APIKey)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, newError("Sub2API 请求失败：" + Redact(err.Error(), secrets...))
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		return nil, newError("Sub2API 响应读取失败：" + Redact(readErr.Error(), secrets...))
	}
	for _, status := range expected {
		if response.StatusCode == status {
			if len(responseBody) == 0 {
				return json.RawMessage(`{}`), nil
			}
			return responseBody, nil
		}
	}
	detail := Redact(errorDetail(responseBody), secrets...)
	if strings.TrimSpace(detail) == "" {
		detail = "请求失败"
	}
	return nil, newError(fmt.Sprintf("Sub2API HTTP %d: %s", response.StatusCode, detail))
}
