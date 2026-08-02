package codexoauth

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

type Callback struct {
	Code  string
	State string
	Error string
}

type callbackWaiter struct {
	state string
	ch    chan Callback
	once  sync.Once
}

func (w *callbackWaiter) Wait(ctx context.Context) (Callback, error) {
	select {
	case callback := <-w.ch:
		if callback.Error != "" {
			return Callback{}, fmt.Errorf("OAuth 回调错误: %s", callback.Error)
		}
		if callback.Code == "" {
			return Callback{}, fmt.Errorf("OAuth 回调缺少 code")
		}
		return callback, nil
	case <-ctx.Done():
		return Callback{}, ctx.Err()
	}
}

type CallbackBroker struct {
	mu      sync.Mutex
	waiters map[string]*callbackWaiter
}

func NewCallbackBroker() *CallbackBroker {
	return &CallbackBroker{waiters: make(map[string]*callbackWaiter)}
}

func (b *CallbackBroker) Register(state string) (*callbackWaiter, error) {
	state = strings.TrimSpace(state)
	if state == "" {
		return nil, fmt.Errorf("OAuth state 为空")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.waiters[state]; exists {
		return nil, fmt.Errorf("OAuth state 已注册")
	}
	waiter := &callbackWaiter{state: state, ch: make(chan Callback, 1)}
	b.waiters[state] = waiter
	return waiter, nil
}

func (b *CallbackBroker) Cancel(waiter *callbackWaiter) {
	if waiter == nil {
		return
	}
	b.mu.Lock()
	if b.waiters[waiter.state] == waiter {
		delete(b.waiters, waiter.state)
	}
	b.mu.Unlock()
}

func (b *CallbackBroker) DispatchURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() != "localhost" || parsed.Port() != "1455" || parsed.Path != "/auth/callback" {
		return false
	}
	state := strings.TrimSpace(parsed.Query().Get("state"))
	if state == "" {
		return false
	}
	b.mu.Lock()
	waiter := b.waiters[state]
	if waiter != nil {
		delete(b.waiters, state)
	}
	b.mu.Unlock()
	if waiter == nil {
		return false
	}
	callback := Callback{Code: strings.TrimSpace(parsed.Query().Get("code")), State: state, Error: strings.TrimSpace(parsed.Query().Get("error"))}
	waiter.once.Do(func() { waiter.ch <- callback })
	return true
}

func (b *CallbackBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/auth/callback" || !b.DispatchURL(RedirectURI+"?"+r.URL.RawQuery) {
		http.Error(w, "invalid or expired OAuth callback", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>Codex OAuth</title><p>%s</p>", html.EscapeString("Codex OAuth 授权已完成，可以关闭此页面。"))
}

func (b *CallbackBroker) Start(listener net.Listener) (*http.Server, error) {
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:1455")
		if err != nil {
			return nil, err
		}
	}
	server := &http.Server{Handler: b}
	go func() { _ = server.Serve(listener) }()
	return server, nil
}
