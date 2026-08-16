package domainmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	Provider       = "domain_api"
	DefaultBaseURL = "https://bikaqiuruanjian.com"
)

var (
	ErrInvalidConfig = errors.New("域名邮配置无效")
	ErrRequestFailed = errors.New("域名邮 API 请求失败")
	ErrNotFound      = errors.New("域名邮资源不存在")
)

type Config struct {
	BaseURL string
	APIKey  string
	Timeout time.Duration
}

type GenerateRequest struct {
	Name       string `json:"name"`
	ExpiryTime int64  `json:"expiryTime"`
	Domain     string `json:"domain"`
}

type Mailbox struct {
	ID     string
	Email  string
	Name   string
	Domain string
}

type Message struct {
	ID         string
	From       string
	FromName   string
	Subject    string
	ReceivedAt time.Time
	HTML       string
	Text       string
}

type RemoteConfig struct {
	Domains []string
}

type MailboxPage struct {
	Items      []Mailbox
	NextCursor string
}

type MessagePage struct {
	Items      []Message
	NextCursor string
}

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type Option func(*Client)

func WithHTTPClient(client *http.Client) Option {
	return func(target *Client) {
		if client != nil {
			target.http = client
		}
	}
}

func New(config Config, options ...Option) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || strings.TrimSpace(config.APIKey) == "" {
		return nil, ErrInvalidConfig
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	client := &Client{baseURL: baseURL, apiKey: strings.TrimSpace(config.APIKey), http: &http.Client{Timeout: timeout}}
	for _, option := range options {
		option(client)
	}
	return client, nil
}

func ValidExpiryTime(value int64) bool {
	switch value {
	case 0, 3600000, 86400000, 604800000:
		return true
	default:
		return false
	}
}

func (c *Client) SystemConfig(ctx context.Context) (RemoteConfig, error) {
	body, err := c.request(ctx, http.MethodGet, "/api/config", nil, nil)
	if err != nil {
		return RemoteConfig{}, err
	}
	value, err := decode(body)
	if err != nil {
		return RemoteConfig{}, err
	}
	domains := decodeDomains(value)
	if len(domains) == 0 {
		return RemoteConfig{}, fmt.Errorf("%w: 响应缺少可用域名", ErrRequestFailed)
	}
	return RemoteConfig{Domains: domains}, nil
}

func (c *Client) Generate(ctx context.Context, input GenerateRequest) (Mailbox, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Domain = strings.TrimSpace(input.Domain)
	if input.Name == "" || input.Domain == "" || !ValidExpiryTime(input.ExpiryTime) {
		return Mailbox{}, ErrInvalidConfig
	}
	body, err := c.request(ctx, http.MethodPost, "/api/emails/generate", nil, input)
	if err != nil {
		return Mailbox{}, err
	}
	value, err := decode(body)
	if err != nil {
		return Mailbox{}, err
	}
	mailbox, ok := decodeMailbox(value)
	if ok {
		return mailbox, nil
	}
	return c.findGeneratedMailbox(ctx, input)
}

func (c *Client) findGeneratedMailbox(ctx context.Context, input GenerateRequest) (Mailbox, error) {
	cursor := ""
	seen := map[string]struct{}{}
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := c.ListMailboxes(ctx, cursor)
		if err != nil {
			return Mailbox{}, err
		}
		for _, mailbox := range page.Items {
			if strings.EqualFold(mailbox.Email, input.Name+"@"+input.Domain) || (strings.EqualFold(mailbox.Name, input.Name) && strings.EqualFold(mailbox.Domain, input.Domain)) {
				return mailbox, nil
			}
		}
		if page.NextCursor == "" {
			break
		}
		if _, exists := seen[page.NextCursor]; exists {
			break
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return Mailbox{}, fmt.Errorf("%w: 生成响应缺少 emailId 或邮箱地址", ErrRequestFailed)
}

func (c *Client) ListMailboxes(ctx context.Context, cursor string) (MailboxPage, error) {
	query := url.Values{}
	if strings.TrimSpace(cursor) != "" {
		query.Set("cursor", strings.TrimSpace(cursor))
	}
	body, err := c.request(ctx, http.MethodGet, "/api/emails", query, nil)
	if err != nil {
		return MailboxPage{}, err
	}
	value, err := decode(body)
	if err != nil {
		return MailboxPage{}, err
	}
	return MailboxPage{Items: decodeMailboxes(value), NextCursor: decodeNextCursor(value)}, nil
}

func (c *Client) ListMessages(ctx context.Context, emailID, cursor string) (MessagePage, error) {
	emailID = strings.TrimSpace(emailID)
	if emailID == "" {
		return MessagePage{}, ErrInvalidConfig
	}
	query := url.Values{}
	if strings.TrimSpace(cursor) != "" {
		query.Set("cursor", strings.TrimSpace(cursor))
	}
	body, err := c.request(ctx, http.MethodGet, "/api/emails/"+url.PathEscape(emailID), query, nil)
	if err != nil {
		return MessagePage{}, err
	}
	value, err := decode(body)
	if err != nil {
		return MessagePage{}, err
	}
	return MessagePage{Items: decodeMessages(value), NextCursor: decodeNextCursor(value)}, nil
}

func (c *Client) DeleteMailbox(ctx context.Context, emailID string) error {
	emailID = strings.TrimSpace(emailID)
	if emailID == "" {
		return ErrInvalidConfig
	}
	_, err := c.request(ctx, http.MethodDelete, "/api/emails/"+url.PathEscape(emailID), nil, nil)
	return err
}

func (c *Client) GetMessage(ctx context.Context, emailID, messageID string) (Message, error) {
	emailID = strings.TrimSpace(emailID)
	messageID = strings.TrimSpace(messageID)
	if emailID == "" || messageID == "" {
		return Message{}, ErrInvalidConfig
	}
	path := "/api/emails/" + url.PathEscape(emailID) + "/" + url.PathEscape(messageID)
	body, err := c.request(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return Message{}, err
	}
	value, err := decode(body)
	if err != nil {
		return Message{}, err
	}
	message, ok := decodeMessage(value)
	if !ok {
		return Message{}, fmt.Errorf("%w: 响应缺少 messageId", ErrRequestFailed)
	}
	return message, nil
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, input any) ([]byte, error) {
	var reader io.Reader
	if input != nil {
		body, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(body)
	}
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRequestFailed, transportError(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, ErrRequestFailed
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := redactSensitive(responseError(body), c.apiKey, c.baseURL)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: HTTP %d %s", ErrNotFound, resp.StatusCode, message)
		}
		return nil, fmt.Errorf("%w: HTTP %d %s", ErrRequestFailed, resp.StatusCode, message)
	}
	return body, nil
}

func transportError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "请求超时"
	}
	if errors.Is(err, context.Canceled) {
		return "请求已取消"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return truncate(err.Error(), 200)
}

func redactSensitive(value string, secrets ...string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func responseError(body []byte) string {
	value, err := decode(body)
	if err == nil {
		if object, ok := value.(map[string]any); ok {
			if message := stringField(object, "error", "message", "detail"); message != "" {
				return truncate(message, 200)
			}
		}
	}
	return truncate(strings.TrimSpace(string(body)), 200)
}

func decode(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: JSON 响应格式错误", ErrRequestFailed)
	}
	return value, nil
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func truncate(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
