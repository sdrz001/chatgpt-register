package smsactivate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAPIKey = "super-secret-key"

func validConfig() Config {
	return Config{
		Platform: PlatformHeroSMS,
		APIKey:   testAPIKey,
		Country:  16,
		MaxPrice: 0.5,
		Timeout:  time.Second,
	}
}

func newTestClient(t *testing.T, config Config, handler http.HandlerFunc, options ...Option) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	allOptions := []Option{WithEndpoint(server.URL), WithHTTPClient(server.Client())}
	allOptions = append(allOptions, options...)
	client, err := New(config, allOptions...)
	if err != nil {
		server.Close()
		t.Fatalf("New() error = %v", err)
	}
	return client, server
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"platform", func(config *Config) { config.Platform = "other" }},
		{"api key", func(config *Config) { config.APIKey = " " }},
		{"missing country", func(config *Config) { config.Country = 0 }},
		{"negative country", func(config *Config) { config.Country = -1 }},
		{"fixed and random", func(config *Config) { config.RandomCountries = []int{1} }},
		{"invalid random candidate", func(config *Config) { config.Country = 0; config.RandomCountries = []int{1, 0} }},
		{"zero price", func(config *Config) { config.MaxPrice = 0 }},
		{"price over limit", func(config *Config) { config.MaxPrice = 5.01 }},
		{"timeout", func(config *Config) { config.Timeout = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			test.mutate(&config)
			err := config.Validate()
			if !IsCode(err, CodeConfig) {
				t.Fatalf("Validate() error = %v, code = %q", err, ErrorCode(err))
			}
			if strings.Contains(fmt.Sprint(err), testAPIKey) {
				t.Fatal("validation error leaked API key")
			}
		})
	}

	config := validConfig()
	config.Country = 0
	config.RandomCountries = []int{16, 187}
	config.MaxPrice = MaxPriceLimit
	if err := config.Validate(); err != nil {
		t.Fatalf("valid random Config.Validate() error = %v", err)
	}
}

func TestPlatformEndpointsAndOptions(t *testing.T) {
	if got, err := endpointFor(PlatformHeroSMS); err != nil || got != HeroSMSEndpoint {
		t.Fatalf("Hero SMS endpoint = %q, %v", got, err)
	}
	if got, err := endpointFor(PlatformSMSBower); err != nil || got != SMSBowerEndpoint {
		t.Fatalf("SMSBower endpoint = %q, %v", got, err)
	}
	config := validConfig()
	config.Platform = PlatformSMSBower
	client, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if client.endpoint.String() != SMSBowerEndpoint {
		t.Fatalf("endpoint = %q", client.endpoint.String())
	}
	if _, err := New(config, WithEndpoint("file:///tmp/api")); !IsCode(err, CodeConfig) {
		t.Fatalf("invalid endpoint error = %v", err)
	}
	if _, err := New(config, WithHTTPClient(nil)); !IsCode(err, CodeConfig) {
		t.Fatalf("nil HTTP client error = %v", err)
	}
}

func TestClientMethods(t *testing.T) {
	var mu sync.Mutex
	var requests []url.Values
	handler := func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		mu.Lock()
		requests = append(requests, query)
		mu.Unlock()
		if query.Get("api_key") != testAPIKey {
			t.Errorf("api_key = %q", query.Get("api_key"))
		}
		switch query.Get("action") {
		case "getBalance":
			fmt.Fprint(w, "ACCESS_BALANCE:12.75")
		case "getNumber":
			if query.Get("service") != ServiceOpenAI {
				t.Errorf("service = %q, want %q", query.Get("service"), ServiceOpenAI)
			}
			if query.Get("country") != "16" || query.Get("maxPrice") != "0.5" {
				t.Errorf("getNumber query = %v", query)
			}
			fmt.Fprint(w, "ACCESS_NUMBER:activation-1:447700900123")
		case "getStatus":
			fmt.Fprint(w, "STATUS_OK:839201")
		case "setStatus":
			fmt.Fprint(w, "ACCESS_READY")
		default:
			http.Error(w, "bad action", http.StatusBadRequest)
		}
	}
	client, server := newTestClient(t, validConfig(), handler)
	defer server.Close()

	balance, err := client.GetBalance(context.Background())
	if err != nil || balance != 12.75 {
		t.Fatalf("GetBalance() = %v, %v", balance, err)
	}
	number, err := client.GetNumber(context.Background())
	if err != nil {
		t.Fatalf("GetNumber() error = %v", err)
	}
	if number != (Number{ActivationID: "activation-1", Phone: "447700900123", Country: 16}) {
		t.Fatalf("GetNumber() = %+v", number)
	}
	status, err := client.GetStatus(context.Background(), number.ActivationID)
	if err != nil || status != (Status{State: StatusOK, Code: "839201"}) {
		t.Fatalf("GetStatus() = %+v, %v", status, err)
	}
	response, err := client.SetStatus(context.Background(), number.ActivationID, 6)
	if err != nil || response != "ACCESS_READY" {
		t.Fatalf("SetStatus() = %q, %v", response, err)
	}
	mu.Lock()
	requestCount := len(requests)
	mu.Unlock()
	if requestCount != 4 {
		t.Fatalf("request count = %d, want 4", requestCount)
	}
}

func TestGetStatusStates(t *testing.T) {
	responses := []string{
		"STATUS_WAIT_CODE",
		"STATUS_WAIT_RESEND",
		"STATUS_WAIT_RETRY:111111",
		"STATUS_OK:222222",
		"STATUS_CANCEL",
		"UNKNOWN_STATUS",
	}
	want := []Status{
		{State: StatusWait},
		{State: StatusWait},
		{State: StatusRetry, Code: "111111"},
		{State: StatusOK, Code: "222222"},
		{State: StatusCancel},
	}
	index := 0
	client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responses[index])
		index++
	})
	defer server.Close()
	for i, expected := range want {
		got, err := client.GetStatus(context.Background(), "id")
		if err != nil || got != expected {
			t.Fatalf("GetStatus() call %d = %+v, %v", i, got, err)
		}
	}
	if _, err := client.GetStatus(context.Background(), "id"); !IsCode(err, "UNEXPECTED_RESPONSE") {
		t.Fatalf("unknown status error = %v", err)
	}
	if _, err := client.GetStatus(context.Background(), " "); !IsCode(err, CodeConfig) {
		t.Fatalf("empty activation ID error = %v", err)
	}
}

func TestFriendlyErrorsKeepCodeAndRedactKey(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "NO_BALANCE")
		})
		defer server.Close()
		_, err := client.GetBalance(context.Background())
		if !IsCode(err, "NO_BALANCE") || !strings.Contains(err.Error(), "余额不足") {
			t.Fatalf("error = %T %v, code = %q", err, err, ErrorCode(err))
		}
	})

	t.Run("json details", func(t *testing.T) {
		client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"title":"VENDOR_FAILURE","details":"request %s failed"}`, testAPIKey)
		})
		defer server.Close()
		_, err := client.GetBalance(context.Background())
		if !IsCode(err, "VENDOR_FAILURE") || strings.Contains(err.Error(), testAPIKey) || !strings.Contains(err.Error(), "***") {
			t.Fatalf("error = %v, code = %q", err, ErrorCode(err))
		}
	})

	t.Run("transport", func(t *testing.T) {
		config := validConfig()
		client, err := New(config, WithHTTPClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial URL containing %s", testAPIKey)
		})))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		_, err = client.GetBalance(context.Background())
		if !IsCode(err, "TRANSPORT_ERROR") || strings.Contains(err.Error(), testAPIKey) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRandomCandidatesContinueOnlyForExpectedCodes(t *testing.T) {
	t.Run("exhausted", func(t *testing.T) {
		config := validConfig()
		config.Country = 0
		config.RandomCountries = []int{16, 187}
		var calls int
		client, server := newTestClient(t, config, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.URL.Query().Get("service") != ServiceOpenAI || r.URL.Query().Get("maxPrice") != "0.5" {
				t.Errorf("getNumber query = %v", r.URL.Query())
			}
			if calls == 1 {
				fmt.Fprint(w, "WRONG_MAX_PRICE")
				return
			}
			fmt.Fprint(w, "NO_NUMBERS")
		})
		defer server.Close()
		_, err := client.GetNumber(context.Background())
		if calls != 2 || !IsCode(err, "NO_NUMBERS") {
			t.Fatalf("calls = %d, error = %v, code = %q", calls, err, ErrorCode(err))
		}
	})

	t.Run("success after retry", func(t *testing.T) {
		config := validConfig()
		config.Country = 0
		config.RandomCountries = []int{16, 187, 16}
		var calls int
		client, server := newTestClient(t, config, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				fmt.Fprint(w, "NO_NUMBERS")
				return
			}
			fmt.Fprint(w, "ACCESS_NUMBER:id:15551234567")
		})
		defer server.Close()
		number, err := client.GetNumber(context.Background())
		if err != nil || calls != 2 || (number.Country != 16 && number.Country != 187) {
			t.Fatalf("number = %+v, calls = %d, error = %v", number, calls, err)
		}
	})

	t.Run("fatal stops", func(t *testing.T) {
		config := validConfig()
		config.Country = 0
		config.RandomCountries = []int{16, 187}
		var calls int
		client, server := newTestClient(t, config, func(w http.ResponseWriter, _ *http.Request) {
			calls++
			fmt.Fprint(w, "NO_BALANCE")
		})
		defer server.Close()
		_, err := client.GetNumber(context.Background())
		if calls != 1 || !IsCode(err, "NO_BALANCE") {
			t.Fatalf("calls = %d, error = %v", calls, err)
		}
	})
}

func TestContextCancellationAndInjectedHTTPClient(t *testing.T) {
	started := make(chan struct{})
	client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.GetBalance(ctx)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not observe context cancellation")
	}
}

func TestHTTPErrorAndMalformedResponses(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, testAPIKey, http.StatusBadGateway)
		})
		defer server.Close()
		_, err := client.GetBalance(context.Background())
		if !IsCode(err, "HTTP_ERROR") || strings.Contains(err.Error(), testAPIKey) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("balance", func(t *testing.T) {
		client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "ACCESS_BALANCE:not-a-number")
		})
		defer server.Close()
		_, err := client.GetBalance(context.Background())
		if !IsCode(err, "UNEXPECTED_RESPONSE") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("number", func(t *testing.T) {
		client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "ACCESS_NUMBER:missing-phone")
		})
		defer server.Close()
		_, err := client.GetNumber(context.Background())
		if !IsCode(err, "UNEXPECTED_RESPONSE") {
			t.Fatalf("error = %v", err)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

type oversizedClient struct{}

func (oversizedClient) Do(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", maxResponseBytes+1))),
		Header:     make(http.Header),
	}, nil
}

func TestResponseSizeLimit(t *testing.T) {
	client, err := New(validConfig(), WithHTTPClient(oversizedClient{}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = client.GetBalance(context.Background())
	if !IsCode(err, "RESPONSE_TOO_LARGE") {
		t.Fatalf("error = %v", err)
	}
}
