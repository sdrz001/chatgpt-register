package mailcom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	webOAuthClientID    = "mailcom_mailcheck_chrome"
	webOAuthRedirectURI = "https://lpebgcnlaohcgdfhbffjajlnpifdkllg.chromiumapp.org/"
	webOAuthBasic       = "Basic bWFpbGNvbV9tYWlsY2hlY2tfY2hyb21lOnRJWkNZWjFZOFFhNUt0MjJMVXJXSDJTc29td1VhV1F5dGszWWdNem4="
	settingsOAuthBasic  = "Basic bWFpbGNvbV9tYWlsc2V0X3Jvb3RfbGl2ZToqKioqKioq"
	settingsPartnerData = "eyJ1c2VjYXNlIjoiaW5ib3hfdW5yZWFkIiwiYXJncyI6W10sImlkIjoyLCJjYWxsZXJfYXBwIjoidG9vbGJhciIsImNhbGxlcl92ZXJzaW9uIjoiQ2hyb21lLzguMC41LjAifQ=="
	webOAuthScope       = "mailbox_user_status_access mailbox_user_full_access login"
)

var settingsInputRE = regexp.MustCompile(`(?i)<input\b[^>]*>`)
var settingsNameRE = regexp.MustCompile(`(?i)\bname\s*=\s*["']([^"']*)["']`)
var settingsValueRE = regexp.MustCompile(`(?i)\bvalue\s*=\s*["']([^"']*)["']`)

func (c *Client) getAutomaticSettingsTokenLocked(ctx context.Context, s *session, account Account, cacheKey string) (string, error) {
	state, err := randomHex(12)
	if err != nil {
		return "", fmt.Errorf("%w: create settings OAuth state", ErrRequest)
	}
	authorizeURL, err := url.Parse(strings.TrimRight(c.endpoints.MobileOAuthURL, "/") + "/authorize")
	if err != nil {
		return "", fmt.Errorf("%w: settings OAuth URL", ErrInvalidConfig)
	}
	query := authorizeURL.Query()
	query.Set("client_id", webOAuthClientID)
	query.Set("redirect_uri", webOAuthRedirectURI)
	query.Set("scope", webOAuthScope)
	query.Set("response_type", "code")
	query.Set("hl", "en-US")
	query.Set("state", state)
	query.Set("login_hint", strings.TrimSpace(account.Email))
	authorizeURL.RawQuery = query.Encode()

	authorize, err := c.settingsWebRequest(ctx, s, http.MethodGet, authorizeURL.String(), nil, nil)
	if err != nil {
		return "", err
	}
	loginPageURL, err := mobileRedirect(authorize, authorizeURL.String(), "settings authorize")
	authorize.Body.Close()
	if err != nil {
		return "", err
	}
	loginPage, err := c.settingsWebRequest(ctx, s, http.MethodGet, loginPageURL, nil, nil)
	if err != nil {
		return "", err
	}
	loginHTML, readErr := io.ReadAll(io.LimitReader(loginPage.Body, 2<<20))
	loginPage.Body.Close()
	if readErr != nil {
		return "", fmt.Errorf("%w: read settings login page", ErrRequest)
	}
	form := settingsLoginForm(string(loginHTML))
	form.Set("username", strings.TrimSpace(account.Email))
	form.Set("password", account.Password)
	if form.Get("service") == "" {
		form.Set("service", "oauth2")
	}
	loginPageParsed, err := url.Parse(loginPageURL)
	if err != nil {
		return "", fmt.Errorf("%w: settings login page URL", ErrSession)
	}
	authcodeContext := loginPageParsed.Query().Get("authcode-context")
	if form.Get("successURL") == "" {
		if authcodeContext == "" {
			return "", fmt.Errorf("%w: settings auth context missing", ErrSession)
		}
		form.Set("successURL", strings.TrimRight(c.endpoints.MobileOAuthURL, "/")+"/authcode?authcode-context="+url.QueryEscape(authcodeContext))
		form.Set("loginFailedURL", loginPageParsed.Scheme+"://"+loginPageParsed.Host+"/oauth2/?status=login-failed&login_hint="+url.QueryEscape(strings.TrimSpace(account.Email))+"&authcode-context="+url.QueryEscape(authcodeContext))
		form.Set("loginErrorURL", loginPageParsed.Scheme+"://"+loginPageParsed.Host+"/loginapplication/error/loginerror")
	}
	loginHeaders := http.Header{}
	loginHeaders.Set("Content-Type", "application/x-www-form-urlencoded")
	loginHeaders.Set("Origin", loginPageParsed.Scheme+"://"+loginPageParsed.Host)
	loginHeaders.Set("Referer", loginPageURL)
	login, err := c.settingsWebRequest(ctx, s, http.MethodPost, c.endpoints.LoginURL, loginHeaders, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	authcodeURL, err := mobileRedirect(login, c.endpoints.LoginURL, "settings login")
	login.Body.Close()
	if err != nil {
		return "", err
	}
	authcode, err := c.settingsWebRequest(ctx, s, http.MethodGet, authcodeURL, nil, nil)
	if err != nil {
		return "", err
	}
	callbackURL, err := mobileRedirect(authcode, authcodeURL, "settings authcode")
	authcode.Body.Close()
	if err != nil {
		return "", err
	}
	callback, err := url.Parse(callbackURL)
	if err != nil || callback.Query().Get("code") == "" || callback.Query().Get("state") != state {
		return "", fmt.Errorf("%w: settings authorization response invalid", ErrSession)
	}
	webToken, err := c.requestWebOAuthToken(ctx, s, callback.Query().Get("code"))
	if err != nil {
		return "", err
	}
	navigatorURL, err := c.openNavigatorSession(ctx, s, webToken)
	if err != nil {
		return "", err
	}
	sid, err := c.exchangeNavigatorSID(ctx, s, navigatorURL)
	if err != nil {
		return "", err
	}
	settingsToken, err := c.requestSettingsBridgeToken(ctx, s, sid)
	if err != nil {
		return "", err
	}
	expiresAt := time.Now().Add(30 * time.Minute)
	if expiry, ok := tokenExpiry(settingsToken); ok {
		expiresAt = expiry
	}
	s.sid = sid
	s.tokens[cacheKey] = cachedToken{value: settingsToken, expiresAt: expiresAt}
	return settingsToken, nil
}

func (c *Client) requestWebOAuthToken(ctx context.Context, s *session, code string) (string, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", webOAuthClientID)
	form.Set("redirect_uri", webOAuthRedirectURI)
	form.Set("grant_type", "authorization_code")
	headers := http.Header{}
	headers.Set("Authorization", webOAuthBasic)
	headers.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	headers.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.settingsWebRequest(ctx, s, http.MethodPost, strings.TrimRight(c.endpoints.MobileOAuthURL, "/")+"/token", headers, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || strings.TrimSpace(payload.AccessToken) == "" {
		return "", fmt.Errorf("%w: web OAuth access token missing", ErrSession)
	}
	return payload.AccessToken, nil
}

func (c *Client) openNavigatorSession(ctx context.Context, s *session, accessToken string) (string, error) {
	loginURL, err := url.Parse(c.endpoints.LoginURL)
	if err != nil {
		return "", fmt.Errorf("%w: login URL", ErrInvalidConfig)
	}
	loginURL.Path = "/oauth2login"
	loginURL.RawQuery = ""
	form := url.Values{}
	form.Set("service", "mailint")
	form.Set("origin", "toolbar")
	form.Set("access_token", accessToken)
	form.Set("successURL", "https://navigator-lxa.mail.com/login")
	form.Set("loginFailedURL", "http://www.mail.com/?status=nologin")
	form.Set("loginErrorURL", "http://www.mail.com/?status=nologin")
	form.Set("statistics", "")
	form.Set("partnerdata", settingsPartnerData)
	headers := http.Header{}
	headers.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.settingsWebRequest(ctx, s, http.MethodPost, loginURL.String(), headers, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	return mobileRedirect(response, loginURL.String(), "navigator login")
}

func (c *Client) exchangeNavigatorSID(ctx context.Context, s *session, navigatorURL string) (string, error) {
	navigator, err := c.settingsWebRequest(ctx, s, http.MethodGet, navigatorURL, nil, nil)
	if err != nil {
		return "", err
	}
	navigator.Body.Close()
	handoff, err := url.Parse(navigatorURL)
	if err != nil {
		return "", fmt.Errorf("%w: navigator URL", ErrSession)
	}
	handoff.Path = "/halogin"
	query := handoff.Query()
	query.Set("tz", "5.5")
	handoff.RawQuery = query.Encode()
	halo, err := c.settingsWebRequest(ctx, s, http.MethodGet, handoff.String(), nil, nil)
	if err != nil {
		return "", err
	}
	rootURL, err := mobileRedirect(halo, handoff.String(), "navigator handoff")
	halo.Body.Close()
	if err != nil {
		return "", err
	}
	root, err := url.Parse(rootURL)
	if err != nil || root.Query().Get("sid") == "" {
		return "", fmt.Errorf("%w: navigator sid missing", ErrSession)
	}
	sid := root.Query().Get("sid")
	landed, err := c.settingsWebRequest(ctx, s, http.MethodGet, rootURL, nil, nil)
	if err != nil {
		return "", err
	}
	landed.Body.Close()
	return sid, nil
}

func (c *Client) requestSettingsBridgeToken(ctx context.Context, s *session, sid string) (string, error) {
	bridgeURL, err := url.Parse(c.endpoints.OAuthURL)
	if err != nil {
		return "", fmt.Errorf("%w: settings bridge URL", ErrInvalidConfig)
	}
	query := bridgeURL.Query()
	query.Set("sid", sid)
	bridgeURL.RawQuery = query.Encode()
	form := url.Values{}
	form.Set("grant_type", "urn:mam:oauth:grant-type:spa")
	form.Set("scope", settingsScope)
	headers := http.Header{}
	headers.Set("Authorization", settingsOAuthBasic)
	headers.Set("Accept", "*/*")
	headers.Set("Content-Type", "application/x-www-form-urlencoded")
	headers.Set("Origin", settingsOrigin)
	headers.Set("Referer", settingsOrigin+"/")
	response, err := c.settingsWebRequest(ctx, s, http.MethodPost, bridgeURL.String(), headers, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || strings.TrimSpace(payload.AccessToken) == "" {
		return "", fmt.Errorf("%w: settings OAuth access token missing", ErrSession)
	}
	return payload.AccessToken, nil
}

func (c *Client) settingsWebRequest(ctx context.Context, s *session, method, endpoint string, headers http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("%w: create settings web request", ErrRequest)
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/149.0.0.0 Safari/537.36")
	}
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if cookie := mobileCookieHeader(s.mobileCookies); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	response, err := s.mobileClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: settings web request: %v", ErrRequest, err)
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
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusBadRequest {
		response.Body.Close()
		return nil, fmt.Errorf("%w: settings web status=%d", ErrRequest, response.StatusCode)
	}
	return response, nil
}

func settingsLoginForm(source string) url.Values {
	values := url.Values{}
	for _, input := range settingsInputRE.FindAllString(source, -1) {
		nameMatch := settingsNameRE.FindStringSubmatch(input)
		if len(nameMatch) != 2 || nameMatch[1] == "" {
			continue
		}
		value := ""
		if valueMatch := settingsValueRE.FindStringSubmatch(input); len(valueMatch) == 2 {
			value = html.UnescapeString(valueMatch[1])
		}
		values.Set(nameMatch[1], value)
	}
	return values
}

func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
