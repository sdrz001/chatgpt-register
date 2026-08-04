package codexoauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	netproxy "golang.org/x/net/proxy"
)

type PhoneSession struct {
	Number         string
	WaitCode       func(context.Context) (string, error)
	Finish         func(context.Context, bool) error
	ReplaceOnError func(error) bool
}

type Input struct {
	Email            string
	Password         string
	Proxy            string
	Headless         bool
	FetchEmailCode   func(context.Context) (string, error)
	AcquirePhone     func(context.Context) (*PhoneSession, error)
	MaxPhoneAttempts int
	Log              func(string, ...any)
	SaveShot         func([]byte)
}

func (in Input) logf(format string, values ...any) {
	if in.Log != nil {
		in.Log(format, values...)
	}
}

var (
	errOAuthCallback      = errors.New("OAuth callback received")
	defaultCallbackBroker = NewCallbackBroker()
	callbackServerOnce    sync.Once
	callbackServerErr     error
)

func Authorize(ctx context.Context, in Input) (Tokens, error) {
	in.Email = strings.TrimSpace(in.Email)
	if in.Email == "" || !strings.Contains(in.Email, "@") {
		return Tokens{}, fmt.Errorf("Codex OAuth 邮箱格式错误")
	}
	if in.Password == "" {
		return Tokens{}, fmt.Errorf("Codex OAuth 密码为空")
	}
	flow, err := NewFlow()
	if err != nil {
		return Tokens{}, fmt.Errorf("创建 PKCE 失败: %w", err)
	}
	if err := ensureCallbackServer(); err != nil {
		return Tokens{}, fmt.Errorf("启动 OAuth 回调监听失败: %w", err)
	}
	waiter, err := defaultCallbackBroker.Register(flow.State)
	if err != nil {
		return Tokens{}, err
	}
	defer defaultCallbackBroker.Cancel(waiter)

	browserCtx, cancelBrowser := context.WithCancelCause(ctx)
	browserDone := make(chan error, 1)
	go func() {
		browserDone <- driveBrowser(browserCtx, in, flow, defaultCallbackBroker)
	}()

	var callback Callback
	select {
	case callback = <-waiter.ch:
		select {
		case <-browserDone:
		case <-time.After(2 * time.Second):
			cancelBrowser(errOAuthCallback)
		}
	case err := <-browserDone:
		if err != nil {
			cancelBrowser(err)
			return Tokens{}, err
		}
		select {
		case callback = <-waiter.ch:
		case <-ctx.Done():
			cancelBrowser(ctx.Err())
			return Tokens{}, ctx.Err()
		}
	case <-ctx.Done():
		cancelBrowser(ctx.Err())
		return Tokens{}, ctx.Err()
	}
	cancelBrowser(errOAuthCallback)
	if callback.Error != "" {
		return Tokens{}, fmt.Errorf("OAuth 回调错误: %s", callback.Error)
	}
	if callback.Code == "" || callback.State != flow.State {
		return Tokens{}, fmt.Errorf("OAuth 回调校验失败")
	}
	client, err := proxyHTTPClient(in.Proxy, 30*time.Second)
	if err != nil {
		return Tokens{}, err
	}
	tokens, err := Exchange(ctx, client, "", flow, callback.Code)
	if err != nil {
		return Tokens{}, err
	}
	if tokens.Email == "" {
		tokens.Email = in.Email
	}
	return tokens, nil
}

func ensureCallbackServer() error {
	callbackServerOnce.Do(func() {
		listener, err := net.Listen("tcp", "127.0.0.1:1455")
		if err != nil {
			callbackServerErr = err
			return
		}
		_, callbackServerErr = defaultCallbackBroker.Start(listener)
	})
	return callbackServerErr
}

func proxyHTTPClient(rawProxy string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{}
	normalized := normalizeProxy(rawProxy)
	if normalized == "" {
		return &http.Client{Transport: transport, Timeout: timeout}, nil
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("代理格式错误")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
	case "socks5", "socks5h":
		var auth *netproxy.Auth
		if parsed.User != nil {
			password, _ := parsed.User.Password()
			auth = &netproxy.Auth{User: parsed.User.Username(), Password: password}
		}
		dialer, err := netproxy.SOCKS5("tcp", parsed.Host, auth, netproxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("创建 SOCKS5 代理失败: %w", err)
		}
		if contextDialer, ok := dialer.(netproxy.ContextDialer); ok {
			transport.DialContext = contextDialer.DialContext
		} else {
			transport.Dial = dialer.Dial
		}
	default:
		return nil, fmt.Errorf("代理协议不受支持: %s", parsed.Scheme)
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func normalizeProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "://") {
		return raw
	}
	parts := strings.Split(raw, ":")
	if len(parts) == 4 {
		return "http://" + url.QueryEscape(parts[2]) + ":" + url.QueryEscape(parts[3]) + "@" + parts[0] + ":" + parts[1]
	}
	return "http://" + raw
}
