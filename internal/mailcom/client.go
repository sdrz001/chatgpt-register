package mailcom

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Provider = "mailcom"

const (
	defaultLoginPageURL     = "https://www.mail.com/"
	defaultLoginURL         = "https://login.mail.com/login"
	defaultOAuthURL         = "https://oauthbridge.navigator-lxa.mail.com/navigator/oauth2/token"
	defaultMailListURL      = "https://maillist.mail.com/Mailbox/Mail"
	defaultMailBodyURL      = "https://webmail-cats-live.mail.com/mailbox/primary/mailbody/{mail_id}/Body"
	defaultOAuthConfigURL   = "https://mailset-root.mail.com/"
	defaultMobileOAuthURL   = "https://oauth2.mail.com"
	defaultMobileMailboxURL = "https://hsp2.mail.com/service/msgsrv/Mailbox/primaryMailbox"
	mailScope               = "mail_mailbox_r"
	mailClientID            = "mailcom_webmailermaillist_passport_live"

	defaultSettingsAddressesURL  = "https://settings-cats.mail.com/mailaccount/primary/emailAddresses"
	defaultSettingsValidationURL = "https://settings-cats.mail.com/mailaccount/emailAddressValidations"
	settingsScope                = "mail_mailbox_w webmailer_setting_r webmailer_setting_w mail_confix_w"
	settingsClientID             = "mailcom_mailset_root_live"
	settingsOrigin               = "https://mailset-root.mail.com"
	settingsUIApp                = "mailcom.mailset-compose/1.0.5-build.335"
	addressListContentType       = "application/vnd.ui.trinity.mailaddress.list-v5+json"
	addressCreateContentType     = "application/vnd.ui.trinity.minimalmailaddress-v3+json"
	validationRequestContentType = "application/vnd.ui.trinity.email-address-validation-request+json"
	validationAcceptContentType  = "application/vnd.ui.trinity.email-address-validation-response+json"
)

var (
	ErrInvalidConfig = errors.New("mail.com config is invalid")
	ErrCredentials   = errors.New("mail.com credentials are invalid")
	ErrBlocked       = errors.New("mail.com rejected the current network")
	ErrRateLimited   = errors.New("mail.com request rate limited")
	ErrSession       = errors.New("mail.com session expired")
	ErrRequest       = errors.New("mail.com request failed")
	ErrMessage       = errors.New("mail.com message response is invalid")
	ErrAliasInvalid  = errors.New("mail.com rejected the alias address")
	ErrAliasLimit    = errors.New("mail.com alias quota exhausted")
	ErrSettings      = errors.New("mail.com settings request failed")
	contextCodeRE    = regexp.MustCompile(`(?i)(?:verification|verify|security|login|one[- ]time|otp|code|验证码|校验码|动态码|一次性)[^0-9]{0,50}([0-9]{4,8})`)
	reverseCodeRE    = regexp.MustCompile(`(?i)([0-9]{4,8})[^0-9]{0,30}(?:verification|verify|security|login|one[- ]time|otp|code|验证码|校验码|动态码|一次性)`)
	genericCodeRE    = regexp.MustCompile(`[0-9]{4,8}`)
	statisticsRE     = regexp.MustCompile(`(?i)name=["']statistics["'][^>]*value=["']([^"']*)`)
)

type Endpoints struct {
	LoginPageURL          string
	LoginURL              string
	OAuthURL              string
	MailListURL           string
	MailBodyURL           string
	SettingsAddressesURL  string
	SettingsValidationURL string
	OAuthConfigURL        string
	MobileOAuthURL        string
	MobileMailboxURL      string
}

type Config struct {
	OAuthPublicSecret string
	Timeout           time.Duration
	Endpoints         Endpoints
}

type Account struct {
	Email    string
	Password string
}

type Message struct {
	ID         string
	Subject    string
	Sender     string
	Recipients []string
	ReceivedAt time.Time
	Body       string
}

type Client struct {
	http      *http.Client
	config    Config
	endpoints Endpoints
	mu        sync.Mutex
	sessions  map[string]*session
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
	config.OAuthPublicSecret = strings.TrimSpace(config.OAuthPublicSecret)
	if config.Timeout <= 0 {
		config.Timeout = 25 * time.Second
	}
	client := &Client{
		http:     &http.Client{Timeout: config.Timeout},
		config:   config,
		sessions: make(map[string]*session),
	}
	for _, option := range options {
		option(client)
	}
	client.endpoints = config.Endpoints
	if client.endpoints.LoginPageURL == "" {
		client.endpoints.LoginPageURL = defaultLoginPageURL
	}
	if client.endpoints.LoginURL == "" {
		client.endpoints.LoginURL = defaultLoginURL
	}
	if client.endpoints.OAuthURL == "" {
		client.endpoints.OAuthURL = defaultOAuthURL
	}
	if client.endpoints.MailListURL == "" {
		client.endpoints.MailListURL = defaultMailListURL
	}
	if client.endpoints.SettingsAddressesURL == "" {
		client.endpoints.SettingsAddressesURL = defaultSettingsAddressesURL
	}
	if client.endpoints.SettingsValidationURL == "" {
		client.endpoints.SettingsValidationURL = defaultSettingsValidationURL
	}
	if client.endpoints.MailBodyURL == "" {
		client.endpoints.MailBodyURL = defaultMailBodyURL
	}
	if client.endpoints.OAuthConfigURL == "" {
		client.endpoints.OAuthConfigURL = defaultOAuthConfigURL
	}
	if client.endpoints.MobileOAuthURL == "" {
		client.endpoints.MobileOAuthURL = defaultMobileOAuthURL
	}
	if client.endpoints.MobileMailboxURL == "" {
		client.endpoints.MobileMailboxURL = defaultMobileMailboxURL
	}
	return client, nil
}

func (c *Client) Verify(ctx context.Context, account Account) error {
	_, err := c.ListMessages(ctx, account, 1)
	return err
}

func (c *Client) ListMessages(ctx context.Context, account Account, limit int) ([]Message, error) {
	if limit < 1 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	if err := validateAccount(account); err != nil {
		return nil, err
	}
	s := c.getSession(account.Email)
	messages, err := c.listMessages(ctx, s, account, limit)
	if errors.Is(err, ErrSession) {
		s.mu.Lock()
		s.reset()
		err = nil
		messages, err = c.listMessagesLocked(ctx, s, account, limit)
		s.mu.Unlock()
	}
	return messages, err
}

func (c *Client) GetMessage(ctx context.Context, account Account, messageID string) (Message, error) {
	if err := validateAccount(account); err != nil {
		return Message{}, err
	}
	if strings.TrimSpace(messageID) == "" {
		return Message{}, fmt.Errorf("%w: message id is empty", ErrMessage)
	}
	s := c.getSession(account.Email)
	message, err := c.getMessage(ctx, s, account, messageID)
	if errors.Is(err, ErrSession) {
		s.mu.Lock()
		s.reset()
		err = nil
		message, err = c.getMessageLocked(ctx, s, account, messageID)
		s.mu.Unlock()
	}
	return message, err
}

func ExtractCode(subject, body string) string {
	text := html.UnescapeString(subject + "\n" + body)
	for _, pattern := range []*regexp.Regexp{contextCodeRE, reverseCodeRE} {
		match := pattern.FindStringSubmatch(text)
		if len(match) == 2 {
			return match[1]
		}
	}
	matches := genericCodeRE.FindAllStringIndex(text, -1)
	seen := make(map[string]struct{}, len(matches))
	unique := make([]string, 0, len(matches))
	for _, indexes := range matches {
		start, end := indexes[0], indexes[1]
		if start > 0 && text[start-1] >= '0' && text[start-1] <= '9' {
			continue
		}
		if end < len(text) && text[end] >= '0' && text[end] <= '9' {
			continue
		}
		candidate := text[start:end]
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		unique = append(unique, candidate)
	}
	if len(unique) == 1 {
		return unique[0]
	}
	return ""
}

func validateAccount(account Account) error {
	if !strings.Contains(strings.TrimSpace(account.Email), "@") || strings.TrimSpace(account.Password) == "" {
		return ErrCredentials
	}
	return nil
}

type session struct {
	mu                 sync.Mutex
	client             *http.Client
	mobileClient       *http.Client
	mobileCookies      map[string]string
	sid                string
	authID             string
	tokens             map[string]cachedToken
	mobileAccessToken  string
	mobileRefreshToken string
	mobileExpiresAt    time.Time
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

func (c *Client) getSession(email string) *session {
	key := strings.ToLower(strings.TrimSpace(email))
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.sessions[key]; ok {
		return existing
	}
	jar, _ := cookiejar.New(nil)
	base := *c.http
	base.Jar = jar
	base.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	mobile := *c.http
	mobile.Jar = nil
	mobile.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	created := &session{client: &base, mobileClient: &mobile, mobileCookies: make(map[string]string), tokens: make(map[string]cachedToken)}
	c.sessions[key] = created
	return created
}

func (s *session) reset() {
	s.sid = ""
	s.authID = ""
	s.tokens = make(map[string]cachedToken)
	s.mobileAccessToken = ""
	s.mobileRefreshToken = ""
	s.mobileExpiresAt = time.Time{}
	s.mobileCookies = make(map[string]string)
}

func (c *Client) listMessages(ctx context.Context, s *session, account Account, limit int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return c.listMessagesLocked(ctx, s, account, limit)
}

func (c *Client) listMessagesLocked(ctx context.Context, s *session, account Account, limit int) ([]Message, error) {
	if strings.TrimSpace(c.config.OAuthPublicSecret) == "" {
		return c.listMobileMessagesLocked(ctx, s, account, limit)
	}
	token, err := c.getTokenLocked(ctx, s, account)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("folderTypeOrId", "INBOX")
	query.Set("offset", "0")
	query.Set("amount", strconv.Itoa(limit))
	query.Set("orderBy", "INTERNALDATE DESC")
	query.Set("no_cache", s.authID)
	query.Set("condition", "mail.header:subject,to,from,cc:"+strings.TrimSpace(account.Email))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoints.MailListURL+"?"+query.Encode(), strings.NewReader(""))
	if err != nil {
		return nil, fmt.Errorf("%w: create message request: %v", ErrRequest, err)
	}
	setMailHeaders(req, token)
	response, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: list messages: %v", ErrRequest, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, ErrSession
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: list messages status=%d", ErrRequest, response.StatusCode)
	}
	var payload struct {
		Elements []struct {
			RawData struct {
				Attribute  map[string]any `json:"attribute"`
				MailHeader map[string]any `json:"mailHeader"`
			} `json:"rawData"`
		} `json:"mailListElements"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: decode message list: %v", ErrMessage, err)
	}
	messages := make([]Message, 0, len(payload.Elements))
	for _, element := range payload.Elements {
		id := stringValue(element.RawData.Attribute, "mailIdentifier")
		if id == "" {
			continue
		}
		recipients := stringValues(element.RawData.MailHeader, "to")
		messages = append(messages, Message{
			ID:         id,
			Subject:    stringValue(element.RawData.MailHeader, "subject"),
			Sender:     stringValue(element.RawData.MailHeader, "from"),
			Recipients: recipients,
			ReceivedAt: messageTime(element.RawData.MailHeader, element.RawData.Attribute),
		})
	}
	return messages, nil
}

func (c *Client) getMessage(ctx context.Context, s *session, account Account, messageID string) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return c.getMessageLocked(ctx, s, account, messageID)
}

func (c *Client) getMessageLocked(ctx context.Context, s *session, account Account, messageID string) (Message, error) {
	if strings.TrimSpace(c.config.OAuthPublicSecret) == "" {
		return c.getMobileMessageLocked(ctx, s, account, messageID)
	}
	token, err := c.getTokenLocked(ctx, s, account)
	if err != nil {
		return Message{}, err
	}
	bodyURL := strings.ReplaceAll(c.endpoints.MailBodyURL, "{mail_id}", url.PathEscape(messageID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bodyURL+"?absoluteURI=false&no_cache="+url.QueryEscape(s.authID), nil)
	if err != nil {
		return Message{}, fmt.Errorf("%w: create body request: %v", ErrRequest, err)
	}
	setMailHeaders(req, token)
	req.Header.Set("Accept", "text/plain")
	response, err := s.client.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("%w: get message body: %v", ErrRequest, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return Message{}, ErrSession
	}
	if response.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("%w: get message body status=%d", ErrRequest, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return Message{}, fmt.Errorf("%w: read message body: %v", ErrRequest, err)
	}
	return Message{ID: messageID, Body: string(body)}, nil
}

func (c *Client) getTokenLocked(ctx context.Context, s *session, account Account) (string, error) {
	return c.getScopedTokenLocked(ctx, s, account, mailClientID, mailScope)
}

func (c *Client) getScopedTokenLocked(ctx context.Context, s *session, account Account, clientID, scope string) (string, error) {
	return c.getScopedTokenWithSecretRefreshLocked(ctx, s, account, clientID, scope, true)
}

func (c *Client) getScopedTokenWithSecretRefreshLocked(ctx context.Context, s *session, account Account, clientID, scope string, refreshSecret bool) (string, error) {
	cacheKey := clientID + "|" + scope
	if cached, ok := s.tokens[cacheKey]; ok && cached.expiresAt.After(time.Now().Add(time.Minute)) {
		return cached.value, nil
	}
	if clientID == settingsClientID && strings.TrimSpace(c.config.OAuthPublicSecret) == "" {
		return c.getAutomaticSettingsTokenLocked(ctx, s, account, cacheKey)
	}
	if s.sid == "" {
		if err := c.loginLocked(ctx, s, account); err != nil {
			return "", err
		}
	}
	form := url.Values{}
	form.Set("grant_type", "urn:mam:oauth:grant-type:spa")
	form.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoints.OAuthURL+"?sid="+url.QueryEscape(s.sid), strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: create OAuth request: %v", ErrRequest, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if clientID == settingsClientID {
		req.Header.Set("Origin", settingsOrigin)
		req.Header.Set("Referer", settingsOrigin+"/")
		req.Header.Set("x-ui-app", settingsUIApp)
	} else {
		req.Header.Set("Origin", "https://webmailer.mail.com")
		req.Header.Set("Referer", "https://webmailer.mail.com/")
		req.Header.Set("x-ui-app", "mailcom.webmailer.mail-list/6.6.3")
	}
	secret, err := c.oauthPublicSecret(ctx, false)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(clientID+":"+secret)))
	response, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: OAuth request: %v", ErrRequest, err)
	}
	if (response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && refreshSecret && strings.TrimSpace(c.config.OAuthPublicSecret) == "" {
		response.Body.Close()
		invalidateOAuthPublicSecret(c.endpoints.OAuthConfigURL)
		return c.getScopedTokenWithSecretRefreshLocked(ctx, s, account, clientID, scope, false)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return "", ErrSession
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return "", ErrRateLimited
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: OAuth status=%d", ErrRequest, response.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || strings.TrimSpace(payload.AccessToken) == "" {
		return "", fmt.Errorf("%w: OAuth access token missing", ErrRequest)
	}
	expiresAt := time.Now().Add(30 * time.Minute)
	if exp, ok := tokenExpiry(payload.AccessToken); ok {
		expiresAt = exp
	}
	if tokenPayload, ok := decodeToken(payload.AccessToken); ok {
		if authID := stringValue(tokenPayload, "auth_id"); authID != "" {
			s.authID = authID
		}
	}
	s.tokens[cacheKey] = cachedToken{value: payload.AccessToken, expiresAt: expiresAt}
	return payload.AccessToken, nil
}

func (c *Client) loginLocked(ctx context.Context, s *session, account Account) error {
	statistics := ""
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoints.LoginPageURL, nil); err == nil {
		if response, requestErr := s.client.Do(req); requestErr == nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				if match := statisticsRE.FindSubmatch(body); len(match) == 2 {
					statistics = string(match[1])
				}
			}
		}
	}
	form := url.Values{}
	form.Set("username", strings.TrimSpace(account.Email))
	form.Set("password", account.Password)
	form.Set("service", "mailint")
	form.Set("uasServiceID", "mc_starter_mailcom")
	form.Set("successURL", "https://$(clientName)-$(dataCenter).mail.com/login")
	form.Set("loginFailedURL", "https://www.mail.com/logout?ls=wd")
	form.Set("loginErrorURL", "https://www.mail.com/logout?ls=te")
	form.Set("edition", "US")
	form.Set("lang", "en")
	form.Set("usertype", "standard")
	form.Set("ibaInfo", "abd=false")
	form.Set("statistics", statistics)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoints.LoginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: create login request: %v", ErrRequest, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", strings.TrimRight(c.endpoints.LoginPageURL, "/"))
	req.Header.Set("Referer", c.endpoints.LoginPageURL)
	response, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: login request: %v", ErrRequest, err)
	}
	defer response.Body.Close()
	location := response.Header.Get("Location")
	if response.StatusCode == http.StatusTooManyRequests {
		return ErrRateLimited
	}
	if response.StatusCode == http.StatusForbidden {
		return ErrBlocked
	}
	if (response.StatusCode == http.StatusFound || response.StatusCode == http.StatusSeeOther) && !strings.Contains(location, "ott=") {
		if strings.Contains(location, "logout?ls=wd") {
			return ErrCredentials
		}
		return fmt.Errorf("%w: login redirect missing ott", ErrCredentials)
	}
	if response.StatusCode != http.StatusSeeOther || !strings.Contains(location, "ott=") {
		return fmt.Errorf("%w: login status=%d", ErrRequest, response.StatusCode)
	}
	loginBase, err := url.Parse(c.endpoints.LoginURL)
	if err != nil {
		return fmt.Errorf("%w: parse login URL", ErrRequest)
	}
	callback, err := loginBase.Parse(location)
	if err != nil {
		return fmt.Errorf("%w: parse login redirect", ErrRequest)
	}
	callback.Path = "/halogin"
	query := callback.Query()
	query.Set("tz", strconv.Itoa(timezoneHours()))
	callback.RawQuery = query.Encode()
	handoff, err := http.NewRequestWithContext(ctx, http.MethodGet, callback.String(), nil)
	if err != nil {
		return fmt.Errorf("%w: create session request", ErrRequest)
	}
	exchanged, err := s.client.Do(handoff)
	if err != nil {
		return fmt.Errorf("%w: session exchange: %v", ErrRequest, err)
	}
	defer exchanged.Body.Close()
	sid := ""
	if exchangedLocation := exchanged.Header.Get("Location"); exchangedLocation != "" {
		if parsed, parseErr := url.Parse(exchangedLocation); parseErr == nil {
			sid = parsed.Query().Get("sid")
		}
	}
	if (exchanged.StatusCode != http.StatusFound && exchanged.StatusCode != http.StatusSeeOther) || sid == "" {
		return ErrSession
	}
	s.sid = sid
	s.tokens = make(map[string]cachedToken)
	return nil
}

func setMailHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.1and1.mms.unified-maillist-v1+json; charset=utf-8")
	req.Header.Set("Content-Type", "application/vnd.1and1.mms.inboxadrequest-v1+json; charset=utf-8")
	req.Header.Set("Origin", "https://webmailer.mail.com")
	req.Header.Set("Referer", "https://webmailer.mail.com/")
	req.Header.Set("x-ui-app", "mailcom.webmailer.mail-list/6.6.3")
}

func decodeToken(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return nil, false
	}
	var result map[string]any
	if json.Unmarshal(payload, &result) != nil {
		return nil, false
	}
	return result, true
}

func tokenExpiry(token string) (time.Time, bool) {
	payload, ok := decodeToken(token)
	if !ok {
		return time.Time{}, false
	}
	value, ok := payload["exp"].(float64)
	if !ok || value <= 0 {
		return time.Time{}, false
	}
	if value > 10_000_000_000 {
		value /= 1000
	}
	return time.Unix(int64(value), 0), true
}

func timezoneHours() int {
	_, offset := time.Now().Zone()
	return offset / 3600
}

func stringValue(object map[string]any, key string) string {
	value, ok := object[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

func stringValues(object map[string]any, key string) []string {
	value, ok := object[key]
	if !ok {
		return nil
	}
	if text, ok := value.(string); ok {
		return []string{text}
	}
	array, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(array))
	for _, item := range array {
		result = append(result, fmt.Sprint(item))
	}
	return result
}

func messageTime(header, attribute map[string]any) time.Time {
	for _, object := range []map[string]any{header, attribute} {
		for _, key := range []string{"date", "internalDate"} {
			value, ok := object[key]
			if !ok {
				continue
			}
			switch typed := value.(type) {
			case float64:
				return time.UnixMilli(int64(typed))
			case json.Number:
				if parsed, err := typed.Int64(); err == nil {
					return time.UnixMilli(parsed)
				}
			case string:
				if parsed, err := strconv.ParseInt(typed, 10, 64); err == nil {
					return time.UnixMilli(parsed)
				}
				for _, layout := range []string{time.RFC3339, time.RFC3339Nano, http.TimeFormat} {
					if parsed, err := time.Parse(layout, typed); err == nil {
						return parsed
					}
				}
			}
		}
	}
	return time.Time{}
}
