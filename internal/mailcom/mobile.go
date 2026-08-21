package mailcom

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
	mobileClientID     = "mailcom_mailapp_android"
	mobileRedirectURI  = "com.mail.androidmail.redirect://authorization_code_grant"
	mobileOAuthBasic   = "Basic bWFpbGNvbV9tYWlsYXBwX2FuZHJvaWQ6a2luMmxTU2tVUXRRQ0NsWG9YZklOaEp1bUc2SmQwM0taNVdMN05KOQ=="
	mobileScope        = "mailbox_user_full_access mailbox_user_status_access hsp_user_full_access onlinestorage_user_meta_read onlinestorage_user_meta_write foo bar"
	mobileUserAgent    = "mailcom.android.androidmail/9.8.0 Dalvik/2.1.0 (Linux; U; Android 13; SM-S908E Build/TQ2B.230505.005.A1)"
	mobileWebUserAgent = "Mozilla/5.0 (Linux; Android 13; SM-S908E Build/TQ2B.230505.005.A1; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/101.0.4951.61 Mobile Safari/537.36 [APPNME/mailcom.android.androidmail;APPVS/9.8.0;APPTNME/andall]"
	mobileMessagesMIME = "application/vnd.ui.trinity.messages+json"
	mobileBodyTextMIME = "text/plain"
)

type mobileTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (c *Client) listMobileMessagesLocked(ctx context.Context, s *session, account Account, limit int) ([]Message, error) {
	token, err := c.mobileTokenLocked(ctx, s, account)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("absoluteURI", "false")
	query.Set("orderBy", "INTERNALDATE desc")
	query.Set("amount", strconv.Itoa(limit))
	query.Set("tagsShowAll", "true")
	endpoint := strings.TrimRight(c.endpoints.MobileMailboxURL, "/") + "/Folder/INBOX/Mail?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create mobile message request: %v", ErrRequest, err)
	}
	setMobileHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", mobileMessagesMIME)
	response, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: list mobile messages: %v", ErrRequest, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, ErrSession
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return nil, ErrRateLimited
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: list mobile messages status=%d", ErrRequest, response.StatusCode)
	}
	var payload struct {
		Mail []struct {
			MailURI    string         `json:"mailURI"`
			Attribute  map[string]any `json:"attribute"`
			MailHeader map[string]any `json:"mailHeader"`
		} `json:"mail"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("%w: decode mobile message list: %v", ErrMessage, err)
	}
	messages := make([]Message, 0, len(payload.Mail))
	for _, item := range payload.Mail {
		id := stringValue(item.Attribute, "mailIdentifier")
		if id == "" {
			id = mobileResourceID(item.MailURI)
		}
		if id == "" {
			continue
		}
		messages = append(messages, Message{
			ID:         id,
			Subject:    stringValue(item.MailHeader, "subject"),
			Sender:     stringValue(item.MailHeader, "from"),
			Recipients: stringValues(item.MailHeader, "to"),
			ReceivedAt: messageTime(item.MailHeader, item.Attribute),
		})
	}
	return messages, nil
}

func (c *Client) getMobileMessageLocked(ctx context.Context, s *session, account Account, messageID string) (Message, error) {
	token, err := c.mobileTokenLocked(ctx, s, account)
	if err != nil {
		return Message{}, err
	}
	endpoint := strings.TrimRight(c.endpoints.MobileMailboxURL, "/") + "/Mail/" + url.PathEscape(mobileResourceID(messageID)) + "/Body?absoluteURI=false"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Message{}, fmt.Errorf("%w: create mobile body request: %v", ErrRequest, err)
	}
	setMobileHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", mobileBodyTextMIME)
	response, err := s.client.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("%w: get mobile message body: %v", ErrRequest, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return Message{}, ErrSession
	}
	if response.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("%w: get mobile message body status=%d", ErrRequest, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return Message{}, fmt.Errorf("%w: read mobile message body: %v", ErrRequest, err)
	}
	return Message{ID: mobileResourceID(messageID), Body: string(body)}, nil
}

func (c *Client) mobileTokenLocked(ctx context.Context, s *session, account Account) (string, error) {
	if s.mobileAccessToken != "" && s.mobileExpiresAt.After(time.Now().Add(time.Minute)) {
		return s.mobileAccessToken, nil
	}
	if s.mobileRefreshToken != "" {
		if err := c.refreshMobileTokenLocked(ctx, s); err == nil {
			return s.mobileAccessToken, nil
		}
		s.mobileAccessToken = ""
		s.mobileRefreshToken = ""
		s.mobileExpiresAt = time.Time{}
	}
	return c.loginMobileLocked(ctx, s, account)
}

func (c *Client) loginMobileLocked(ctx context.Context, s *session, account Account) (string, error) {
	verifier, err := randomBase64URL(48)
	if err != nil {
		return "", fmt.Errorf("%w: create PKCE verifier", ErrRequest)
	}
	challengeDigest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeDigest[:])
	state, err := randomBase64URL(48)
	if err != nil {
		return "", fmt.Errorf("%w: create OAuth state", ErrRequest)
	}
	authorizeURL, err := url.Parse(strings.TrimRight(c.endpoints.MobileOAuthURL, "/") + "/authorize")
	if err != nil {
		return "", fmt.Errorf("%w: parse mobile OAuth URL", ErrInvalidConfig)
	}
	query := authorizeURL.Query()
	query.Set("client_id", mobileClientID)
	query.Set("redirect_uri", mobileRedirectURI)
	query.Set("response_type", "code")
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("login_hint", strings.TrimSpace(account.Email))
	query.Set("code_challenge_method", "S256")
	authorizeURL.RawQuery = query.Encode()

	authorize, err := c.mobileWebRequest(ctx, s, http.MethodGet, authorizeURL.String(), "", "", nil)
	if err != nil {
		return "", err
	}
	loginPageURL, err := mobileRedirect(authorize, authorizeURL.String(), "authorize")
	authorize.Body.Close()
	if err != nil {
		return "", err
	}
	loginPage, err := c.mobileWebRequest(ctx, s, http.MethodGet, loginPageURL, "", "", nil)
	if err != nil {
		return "", err
	}
	loginPage.Body.Close()
	loginPageParsed, parseErr := url.Parse(loginPageURL)
	if parseErr != nil || loginPageParsed.Scheme == "" || loginPageParsed.Host == "" {
		return "", fmt.Errorf("%w: mobile login page URL", ErrSession)
	}
	authcodeContext := loginPageParsed.Query().Get("authcode-context")
	if authcodeContext == "" {
		return "", fmt.Errorf("%w: mobile auth context missing", ErrSession)
	}
	authOrigin := loginPageParsed.Scheme + "://" + loginPageParsed.Host
	failedURL := authOrigin + "/loginapp/oauth2?status=login_failed&login_hint=" + url.QueryEscape(strings.TrimSpace(account.Email)) + "&authcode-context=" + url.QueryEscape(authcodeContext)
	form := url.Values{}
	form.Set("password", account.Password)
	form.Set("service", "oauth2")
	form.Set("successURL", strings.TrimRight(c.endpoints.MobileOAuthURL, "/")+"/authcode?authcode-context="+url.QueryEscape(authcodeContext))
	form.Set("loginFailedURL", failedURL)
	form.Set("loginErrorURL", authOrigin+"/login/error")
	form.Set("statistics", "")
	form.Set("username", strings.TrimSpace(account.Email))
	login, err := c.mobileWebRequest(ctx, s, http.MethodPost, c.endpoints.LoginURL, "application/x-www-form-urlencoded", loginPageURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	authcodeURL, err := mobileRedirect(login, c.endpoints.LoginURL, "login")
	login.Body.Close()
	if err != nil {
		return "", err
	}
	authcode, err := c.mobileWebRequest(ctx, s, http.MethodGet, authcodeURL, "", "", nil)
	if err != nil {
		return "", err
	}
	appURL, err := mobileRedirect(authcode, authcodeURL, "authcode")
	authcode.Body.Close()
	if err != nil {
		return "", err
	}
	callback, err := url.Parse(appURL)
	if err != nil || callback.Query().Get("code") == "" || callback.Query().Get("state") != state {
		return "", fmt.Errorf("%w: mobile authorization response invalid", ErrSession)
	}
	token, err := c.requestMobileToken(ctx, s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {callback.Query().Get("code")},
		"redirect_uri":  {mobileRedirectURI},
		"client_id":     {mobileClientID},
		"code_verifier": {verifier},
	})
	if err != nil {
		return "", err
	}
	if token.AccessToken == "" || token.RefreshToken == "" {
		return "", ErrSession
	}
	c.storeMobileToken(s, token, "")
	if err := c.refreshMobileTokenLocked(ctx, s); err != nil {
		return "", err
	}
	return s.mobileAccessToken, nil
}

func (c *Client) refreshMobileTokenLocked(ctx context.Context, s *session) error {
	refreshToken := s.mobileRefreshToken
	token, err := c.requestMobileToken(ctx, s, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {mobileScope},
	})
	if err != nil {
		return err
	}
	if token.AccessToken == "" {
		return ErrSession
	}
	c.storeMobileToken(s, token, refreshToken)
	return nil
}

func (c *Client) requestMobileToken(ctx context.Context, s *session, form url.Values) (mobileTokenResponse, error) {
	endpoint := strings.TrimRight(c.endpoints.MobileOAuthURL, "/") + "/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return mobileTokenResponse{}, fmt.Errorf("%w: create mobile token request", ErrRequest)
	}
	setMobileHeaders(req)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", mobileOAuthBasic)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	response, err := s.client.Do(req)
	if err != nil {
		return mobileTokenResponse{}, fmt.Errorf("%w: mobile token request: %v", ErrRequest, err)
	}
	defer response.Body.Close()
	var token mobileTokenResponse
	if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
		return mobileTokenResponse{}, fmt.Errorf("%w: decode mobile token", ErrRequest)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return mobileTokenResponse{}, ErrSession
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return mobileTokenResponse{}, ErrRateLimited
	}
	if response.StatusCode != http.StatusOK {
		return mobileTokenResponse{}, fmt.Errorf("%w: mobile token status=%d", ErrRequest, response.StatusCode)
	}
	return token, nil
}

func (c *Client) mobileWebRequest(ctx context.Context, s *session, method, endpoint, contentType, referer string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("%w: create mobile web request", ErrRequest)
	}
	req.Header.Set("User-Agent", mobileWebUserAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Origin", "https://auth.mail.com")
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if cookie := mobileCookieHeader(s.mobileCookies); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	response, err := s.mobileClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: mobile web request: %v", ErrRequest, err)
	}
	absorbMobileCookies(s.mobileCookies, response.Header.Values("Set-Cookie"))
	if response.StatusCode == http.StatusTooManyRequests {
		response.Body.Close()
		return nil, ErrRateLimited
	}
	if response.StatusCode == http.StatusForbidden {
		response.Body.Close()
		return nil, ErrBlocked
	}
	return response, nil
}

func mobileCookieHeader(cookies map[string]string) string {
	values := make([]string, 0, len(cookies))
	for name, value := range cookies {
		values = append(values, name+"="+value)
	}
	return strings.Join(values, "; ")
}

func absorbMobileCookies(cookies map[string]string, headers []string) {
	for _, header := range headers {
		pair := strings.SplitN(header, ";", 2)[0]
		name, value, ok := strings.Cut(pair, "=")
		name = strings.TrimSpace(name)
		if ok && name != "" {
			cookies[name] = strings.TrimSpace(value)
		}
	}
}

func mobileRedirect(response *http.Response, baseURL, stage string) (string, error) {
	location := response.Header.Get("Location")
	if response.StatusCode < http.StatusMultipleChoices || response.StatusCode >= http.StatusBadRequest || location == "" {
		return "", fmt.Errorf("%w: mobile %s redirect invalid status=%d location=%s", ErrCredentials, stage, response.StatusCode, sanitizedRedirectLocation(location))
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("%w: mobile %s redirect URL", ErrRequest, stage)
	}
	resolved, err := base.Parse(location)
	if err != nil {
		return "", fmt.Errorf("%w: mobile %s location", ErrRequest, stage)
	}
	return resolved.String(), nil
}

func sanitizedRedirectLocation(location string) string {
	if strings.TrimSpace(location) == "" {
		return "empty"
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return "invalid"
	}
	if parsed.IsAbs() {
		return parsed.Scheme + "://" + parsed.Host + parsed.EscapedPath()
	}
	return parsed.EscapedPath()
}

func setMobileHeaders(req *http.Request) {
	req.Header.Set("Accept-Charset", "utf-8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", mobileUserAgent)
	req.Header.Set("X-Ui-App", "mailcom.android.androidmail/9.8.0")
}

func (c *Client) storeMobileToken(s *session, token mobileTokenResponse, retainedRefreshToken string) {
	s.mobileAccessToken = strings.TrimSpace(token.AccessToken)
	s.mobileRefreshToken = strings.TrimSpace(token.RefreshToken)
	if s.mobileRefreshToken == "" {
		s.mobileRefreshToken = retainedRefreshToken
	}
	expiresIn := token.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 1800
	}
	s.mobileExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
}

func randomBase64URL(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func mobileResourceID(value string) string {
	value = strings.TrimSpace(value)
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		value = value[slash+1:]
	}
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}
