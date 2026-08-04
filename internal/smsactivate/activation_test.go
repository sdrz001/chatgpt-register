package smsactivate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestActivationPollAndSuccessfulIdempotentClose(t *testing.T) {
	var mu sync.Mutex
	var setStatuses []string
	getStatusCalls := 0
	client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		mu.Lock()
		defer mu.Unlock()
		switch query.Get("action") {
		case "getNumber":
			fmt.Fprint(w, "ACCESS_NUMBER:act-1:447700900123")
		case "setStatus":
			setStatuses = append(setStatuses, query.Get("status"))
			fmt.Fprintf(w, "ACCESS_STATUS_%s", query.Get("status"))
		case "getStatus":
			getStatusCalls++
			switch getStatusCalls {
			case 1:
				fmt.Fprint(w, "STATUS_WAIT_CODE")
			case 2:
				fmt.Fprint(w, "STATUS_OK:111111")
			default:
				fmt.Fprint(w, "STATUS_WAIT_RETRY:222222")
			}
		}
	})
	defer server.Close()

	activation, err := client.Allocate(context.Background())
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}
	if activation.ActivationID != "act-1" || activation.Number != "447700900123" || activation.E164 != "+447700900123" || activation.Country != 16 {
		t.Fatalf("activation = %+v", activation)
	}
	code, err := activation.PollCode(context.Background(), time.Millisecond)
	if err != nil || code != "111111" {
		t.Fatalf("first PollCode() = %q, %v", code, err)
	}
	code, err = activation.PollCode(context.Background(), time.Millisecond)
	if err != nil || code != "222222" {
		t.Fatalf("second PollCode() = %q, %v", code, err)
	}
	result, err := activation.Close(context.Background(), true)
	if err != nil || result != "ACCESS_STATUS_6" {
		t.Fatalf("Close(true) = %q, %v", result, err)
	}
	secondResult, secondErr := activation.Close(context.Background(), false)
	if secondErr != nil || secondResult != result {
		t.Fatalf("second Close() = %q, %v", secondResult, secondErr)
	}
	mu.Lock()
	gotStatuses := strings.Join(setStatuses, ",")
	mu.Unlock()
	if gotStatuses != "1,3,6" {
		t.Fatalf("setStatus sequence = %q, want 1,3,6", gotStatuses)
	}
	if _, err := activation.PollCode(context.Background(), time.Millisecond); !IsCode(err, "ACTIVATION_CLOSED") {
		t.Fatalf("PollCode() after close error = %v", err)
	}
}

func TestActivationFailedCloseUsesStatusEight(t *testing.T) {
	var statuses []string
	client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "getNumber":
			fmt.Fprint(w, "ACCESS_NUMBER:act-2:+15551234567")
		case "setStatus":
			statuses = append(statuses, r.URL.Query().Get("status"))
			fmt.Fprint(w, "ACCESS_CANCEL")
		}
	})
	defer server.Close()
	activation, err := client.Allocate(context.Background())
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}
	if activation.E164 != "+15551234567" {
		t.Fatalf("E164 = %q", activation.E164)
	}
	first, err := activation.Close(context.Background(), true)
	if err != nil || first != "ACCESS_CANCEL" {
		t.Fatalf("Close(true) = %q, %v", first, err)
	}
	second, err := activation.Close(context.Background(), true)
	if err != nil || second != first {
		t.Fatalf("second Close(true) = %q, %v", second, err)
	}
	if strings.Join(statuses, ",") != "8" {
		t.Fatalf("statuses = %v, want [8]", statuses)
	}
}

func TestActivationCloseRetriesOriginalStatusAfterError(t *testing.T) {
	calls := 0
	var statuses []string
	client, err := New(validConfig(), WithHTTPClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		statuses = append(statuses, req.URL.Query().Get("status"))
		if calls == 1 {
			return nil, fmt.Errorf("close failed")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ACCESS_READY")), Header: http.Header{}}, nil
	})))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	activation := newActivation(client, Number{ActivationID: "id", Phone: "123", Country: 16})
	activation.codeReceived = true
	_, firstErr := activation.Close(context.Background(), true)
	result, secondErr := activation.Close(context.Background(), false)
	if calls != 2 || strings.Join(statuses, ",") != "6,6" || firstErr == nil || secondErr != nil || result != "ACCESS_READY" {
		t.Fatalf("calls/statuses = %d/%v, first error = %v, second result/error = %q/%v", calls, statuses, firstErr, result, secondErr)
	}
}

func TestActivationCloseRetriesAfterEarlyCancelWindow(t *testing.T) {
	calls := 0
	client, err := New(validConfig(), WithHTTPClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := "EARLY_CANCEL_DENIED"
		if calls == 2 {
			body = "ACCESS_CANCEL"
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	activation := newActivation(client, Number{ActivationID: "id", Phone: "123", Country: 16})
	activation.createdAt = time.Now().Add(-cancelRetryAge)
	result, err := activation.Close(context.Background(), false)
	if err != nil || result != "ACCESS_CANCEL" || calls != 2 {
		t.Fatalf("Close(false)=%q/%v calls=%d", result, err, calls)
	}
}

func TestActivationConcurrentCloseSharesRequest(t *testing.T) {
	var calls int
	started := make(chan struct{})
	release := make(chan struct{})
	client, err := New(validConfig(), WithHTTPClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		close(started)
		<-release
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ACCESS_CANCEL")), Header: http.Header{}}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	activation := newActivation(client, Number{ActivationID: "id", Phone: "123", Country: 16})
	results := make(chan error, 2)
	go func() { _, err := activation.Close(context.Background(), false); results <- err }()
	<-started
	go func() { _, err := activation.Close(context.Background(), false); results <- err }()
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestActivationPollContextTimeoutAndThenRetry(t *testing.T) {
	var mu sync.Mutex
	var statuses []string
	client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "setStatus":
			mu.Lock()
			statuses = append(statuses, r.URL.Query().Get("status"))
			mu.Unlock()
			fmt.Fprint(w, "ACCESS_READY")
		case "getStatus":
			fmt.Fprint(w, "STATUS_WAIT_CODE")
		}
	})
	defer server.Close()
	activation := newActivation(client, Number{ActivationID: "id", Phone: "123", Country: 16})
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		_, err := activation.PollCode(ctx, time.Second)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("PollCode() call %d error = %v", i, err)
		}
	}
	mu.Lock()
	gotStatuses := strings.Join(statuses, ",")
	mu.Unlock()
	if gotStatuses != "1,3" {
		t.Fatalf("setStatus sequence = %q, want 1,3", gotStatuses)
	}
}

func TestActivationPollCancelledAndValidation(t *testing.T) {
	var statuses []string
	client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "setStatus":
			statuses = append(statuses, r.URL.Query().Get("status"))
			fmt.Fprint(w, "ACCESS_READY")
		case "getStatus":
			fmt.Fprint(w, "STATUS_CANCEL")
		}
	})
	defer server.Close()
	activation := newActivation(client, Number{ActivationID: "id", Phone: "123", Country: 16})
	if _, err := activation.PollCode(context.Background(), 0); !IsCode(err, CodeConfig) {
		t.Fatalf("zero interval error = %v", err)
	}
	if _, err := activation.PollCode(context.Background(), time.Millisecond); !IsCode(err, "STATUS_CANCEL") {
		t.Fatalf("cancel status error = %v", err)
	}
	result, err := activation.Close(context.Background(), false)
	if err != nil || result != "STATUS_CANCEL" || strings.Join(statuses, ",") != "1" {
		t.Fatalf("Close(false)=%q/%v statuses=%v", result, err, statuses)
	}
}
