package codexoauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCallbackBrokerRoutesConcurrentStates(t *testing.T) {
	broker := NewCallbackBroker()
	first, err := broker.Register("state-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := broker.Register("state-two")
	if err != nil {
		t.Fatal(err)
	}
	if !broker.DispatchURL(RedirectURI+"?code=code-two&state=state-two") || !broker.DispatchURL(RedirectURI+"?code=code-one&state=state-one") {
		t.Fatal("DispatchURL returned false")
	}
	for waiter, want := range map[*callbackWaiter]string{first: "code-one", second: "code-two"} {
		callback, err := waiter.Wait(context.Background())
		if err != nil || callback.Code != want {
			t.Fatalf("callback=%+v error=%v", callback, err)
		}
	}
	if broker.DispatchURL(RedirectURI + "?code=duplicate&state=state-one") {
		t.Fatal("duplicate callback accepted")
	}
}

func TestCallbackBrokerRejectsUnknownAndWrongURL(t *testing.T) {
	broker := NewCallbackBroker()
	for _, rawURL := range []string{
		"http://example.test/auth/callback?code=x&state=y",
		"http://localhost:1455/other?code=x&state=y",
		RedirectURI + "?code=x",
		RedirectURI + "?code=x&state=unknown",
	} {
		if broker.DispatchURL(rawURL) {
			t.Fatalf("accepted %q", rawURL)
		}
	}
}

func TestCallbackWaitErrorTimeoutAndCancel(t *testing.T) {
	broker := NewCallbackBroker()
	waiter, err := broker.Register("error-state")
	if err != nil {
		t.Fatal(err)
	}
	if !broker.DispatchURL(RedirectURI + "?error=access_denied&state=error-state") {
		t.Fatal("error callback not dispatched")
	}
	if _, err := waiter.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("error=%v", err)
	}

	waiter, err = broker.Register("timeout-state")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := waiter.Wait(ctx); err == nil {
		t.Fatal("Wait returned nil timeout")
	}
	broker.Cancel(waiter)
	if broker.DispatchURL(RedirectURI + "?code=late&state=timeout-state") {
		t.Fatal("late callback accepted")
	}
}

func TestCallbackBrokerHTTPHandler(t *testing.T) {
	broker := NewCallbackBroker()
	waiter, err := broker.Register("http-state")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/auth/callback?code=http-code&state=http-state", nil)
	response := httptest.NewRecorder()
	broker.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "授权已完成") {
		t.Fatalf("response=%d %q", response.Code, response.Body.String())
	}
	callback, err := waiter.Wait(context.Background())
	if err != nil || callback.Code != "http-code" {
		t.Fatalf("callback=%+v error=%v", callback, err)
	}
}
