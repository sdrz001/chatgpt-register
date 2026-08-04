package smsactivate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 1 << 20

type Client struct {
	config     Config
	httpClient HTTPClient
	endpoint   *url.URL
}

type Number struct {
	ActivationID string
	Phone        string
	Country      int
}

type StatusState string

const (
	StatusWait   StatusState = "wait"
	StatusRetry  StatusState = "retry"
	StatusOK     StatusState = "ok"
	StatusCancel StatusState = "cancel"
)

type Status struct {
	State StatusState
	Code  string
}

func New(config Config, options ...Option) (*Client, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.RandomCountries = normalizedCountries(config.RandomCountries)
	if err := config.Validate(); err != nil {
		return nil, err
	}
	endpoint, _ := endpointFor(config.Platform)
	settings := clientOptions{
		httpClient: &http.Client{Timeout: config.Timeout},
		endpoint:   endpoint,
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(&settings); err != nil {
			return nil, err
		}
	}
	parsedEndpoint, err := parseEndpoint(settings.endpoint)
	if err != nil {
		return nil, err
	}
	return &Client{config: config, httpClient: settings.httpClient, endpoint: parsedEndpoint}, nil
}

func (c *Client) GetBalance(ctx context.Context) (float64, error) {
	text, err := c.call(ctx, "getBalance", nil)
	if err != nil {
		return 0, err
	}
	value, ok := strings.CutPrefix(text, "ACCESS_BALANCE:")
	if !ok {
		return 0, responseError("getBalance", c.redact(text))
	}
	balance, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, responseError("getBalance", c.redact(text))
	}
	return balance, nil
}

func (c *Client) GetNumber(ctx context.Context) (Number, error) {
	if c.config.Country > 0 {
		return c.getNumber(ctx, c.config.Country)
	}
	countries := append([]int(nil), c.config.RandomCountries...)
	rand.Shuffle(len(countries), func(i, j int) {
		countries[i], countries[j] = countries[j], countries[i]
	})
	for _, country := range countries {
		number, err := c.getNumber(ctx, country)
		if err == nil {
			return number, nil
		}
		if !IsCode(err, "NO_NUMBERS") && !IsCode(err, "WRONG_MAX_PRICE") {
			return Number{}, err
		}
	}
	return Number{}, &Error{
		Code:    "NO_NUMBERS",
		Message: fmt.Sprintf("随机候选国家均没有不高于 $%s 的 OpenAI 号码", formatPrice(c.config.MaxPrice)),
	}
}

func (c *Client) getNumber(ctx context.Context, country int) (Number, error) {
	text, err := c.call(ctx, "getNumber", url.Values{
		"service":  {ServiceOpenAI},
		"country":  {strconv.Itoa(country)},
		"maxPrice": {formatPrice(c.config.MaxPrice)},
	})
	if err != nil {
		return Number{}, err
	}
	parts := strings.Split(text, ":")
	if len(parts) < 3 || parts[0] != "ACCESS_NUMBER" || strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[2]) == "" {
		return Number{}, responseError("getNumber", c.redact(text))
	}
	return Number{
		ActivationID: strings.TrimSpace(parts[1]),
		Phone:        strings.TrimSpace(parts[2]),
		Country:      country,
	}, nil
}

func (c *Client) GetStatus(ctx context.Context, activationID string) (Status, error) {
	activationID = strings.TrimSpace(activationID)
	if activationID == "" {
		return Status{}, configError("激活 ID 为空")
	}
	text, err := c.call(ctx, "getStatus", url.Values{"id": {activationID}})
	if err != nil {
		return Status{}, err
	}
	switch {
	case text == "STATUS_WAIT_CODE", text == "STATUS_WAIT_RESEND":
		return Status{State: StatusWait}, nil
	case text == "STATUS_CANCEL":
		return Status{State: StatusCancel}, nil
	case text == "STATUS_WAIT_RETRY" || strings.HasPrefix(text, "STATUS_WAIT_RETRY:"):
		return Status{State: StatusRetry, Code: statusCode(text)}, nil
	case text == "STATUS_OK" || strings.HasPrefix(text, "STATUS_OK:"):
		return Status{State: StatusOK, Code: statusCode(text)}, nil
	default:
		return Status{}, responseError("getStatus", c.redact(text))
	}
}

func (c *Client) SetStatus(ctx context.Context, activationID string, status int) (string, error) {
	activationID = strings.TrimSpace(activationID)
	if activationID == "" {
		return "", configError("激活 ID 为空")
	}
	return c.call(ctx, "setStatus", url.Values{
		"id":     {activationID},
		"status": {strconv.Itoa(status)},
	})
}

func (c *Client) Allocate(ctx context.Context) (*Activation, error) {
	number, err := c.GetNumber(ctx)
	if err != nil {
		return nil, err
	}
	return newActivation(c, number), nil
}

func (c *Client) CancelActivation(ctx context.Context, activationID string, createdAt time.Time) (string, error) {
	activation := &Activation{client: c, ActivationID: strings.TrimSpace(activationID), createdAt: createdAt}
	result, err := activation.Close(ctx, false)
	if IsCode(err, "NO_ACTIVATION") || IsCode(err, "WRONG_ACTIVATION_ID") || IsCode(err, "NO_ACTIVATIONS") {
		return "ALREADY_CLOSED", nil
	}
	return result, err
}

func (c *Client) call(ctx context.Context, action string, params url.Values) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	query := c.endpoint.Query()
	query.Set("api_key", c.config.APIKey)
	query.Set("action", action)
	for key, values := range params {
		for _, value := range values {
			query.Add(key, value)
		}
	}
	requestURL := *c.endpoint
	requestURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return "", c.wrapError("创建请求失败", err)
	}
	req.Header.Set("User-Agent", "chatgpt-register/smsactivate")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", c.wrapError("连接接码平台失败", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", c.wrapError("读取接码平台响应失败", err)
	}
	if len(body) > maxResponseBytes {
		return "", &Error{Code: "RESPONSE_TOO_LARGE", Message: "接码平台响应过大"}
	}
	text := strings.TrimSpace(string(body))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", &Error{Code: "HTTP_ERROR", Message: fmt.Sprintf("接码平台请求失败（HTTP %d）", resp.StatusCode)}
	}
	if err := c.apiResponseError(text); err != nil {
		return "", err
	}
	return text, nil
}

func (c *Client) apiResponseError(text string) error {
	var payload struct {
		Title   string `json:"title"`
		Details string `json:"details"`
	}
	if json.Unmarshal([]byte(text), &payload) == nil && strings.TrimSpace(payload.Title) != "" {
		return codedError(payload.Title, c.redact(payload.Details))
	}
	code := strings.TrimSpace(strings.SplitN(text, ":", 2)[0])
	if _, ok := errorMessages[code]; ok {
		return codedError(code, "")
	}
	return nil
}

func (c *Client) wrapError(message string, err error) error {
	return &Error{Code: "TRANSPORT_ERROR", Message: message + "：" + c.redact(err.Error()), cause: err}
}

func (c *Client) redact(value string) string {
	if c.config.APIKey == "" {
		return value
	}
	return strings.ReplaceAll(value, c.config.APIKey, "***")
}

func statusCode(text string) string {
	_, code, ok := strings.Cut(text, ":")
	if !ok {
		return ""
	}
	return strings.TrimSpace(code)
}

func formatPrice(price float64) string {
	return strconv.FormatFloat(price, 'f', -1, 64)
}
