package sub2api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   map[string]any
}

func testConfig(serverURL string) Config {
	return Config{
		URL:         serverURL,
		APIKey:      "synthetic-admin-key",
		GroupIDs:    []int{12, 13},
		Concurrency: 10,
		Priority:    1,
		Timeout:     30,
	}
}

func writeJSON(t *testing.T, writer http.ResponseWriter, status int, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func decodeRequest(t *testing.T, request *http.Request) map[string]any {
	t.Helper()
	if request.Body == nil {
		return nil
	}
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return body
}

func remoteAccount(id int, name, email string, groups any) map[string]any {
	return map[string]any{
		"id":          id,
		"name":        name,
		"platform":    "openai",
		"type":        "oauth",
		"status":      "active",
		"group_ids":   groups,
		"credentials": map[string]any{"email": email},
	}
}

func TestListGroupsFetchesEveryPageAndHeaders(t *testing.T) {
	var requests []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, recordedRequest{
			Method: request.Method,
			Path:   request.URL.Path,
			Query:  request.URL.Query(),
			Header: request.Header.Clone(),
		})
		page := request.URL.Query().Get("page")
		switch page {
		case "1":
			writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"items": []any{
				map[string]any{"id": 2, "name": "Second", "platform": "grok", "status": "active"},
				map[string]any{"id": "bad", "name": "Ignored"},
			}}})
		case "2":
			writeJSON(t, writer, http.StatusCreated, map[string]any{"data": map[string]any{"list": []any{
				map[string]any{"id": 1, "name": "First", "platform": "openai", "status": "active"},
			}}})
		default:
			writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"items": []any{}}})
		}
	}))
	defer server.Close()

	groups, err := ListGroups(context.Background(), testConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("ListGroups() error = %v", err)
	}
	if len(groups) != 2 || groups[0].ID != 1 || groups[1].ID != 2 || groups[1].Platform != "grok" {
		t.Fatalf("ListGroups() = %+v", groups)
	}
	if len(requests) != 3 {
		t.Fatalf("request count = %d", len(requests))
	}
	for index, request := range requests {
		if request.Method != http.MethodGet || request.Path != "/api/v1/admin/groups" {
			t.Fatalf("request = %+v", request)
		}
		if request.Query.Get("page") != fmt.Sprint(index+1) || request.Query.Get("page_size") != "200" || request.Query.Has("platform") {
			t.Fatalf("query = %v", request.Query)
		}
		if request.Header.Get("x-api-key") != "synthetic-admin-key" {
			t.Fatalf("x-api-key = %q", request.Header.Get("x-api-key"))
		}
	}
}

func TestListOpenAIOAuthAccountsPaginatesAndMatchesExactly(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		pages = append(pages, query.Get("page"))
		if query.Get("platform") != "openai" || query.Get("type") != "oauth" || query.Get("search") != "User@Example.Test" || query.Get("page_size") != "100" {
			t.Errorf("query = %v", query)
		}
		var items []any
		switch query.Get("page") {
		case "1":
			items = []any{
				remoteAccount(1, "other", "another@example.test", []any{}),
				map[string]any{"id": 2, "name": "User@Example.Test", "platform": "grok", "type": "oauth"},
			}
		case "2":
			items = []any{remoteAccount(9, "custom label", "user@example.test", []any{})}
		default:
			items = []any{}
		}
		writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"items": items}})
	}))
	defer server.Close()
	client, err := NewClient(testConfig(server.URL), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	matches, err := client.ListOpenAIOAuthAccounts(context.Background(), "User@Example.Test")
	if err != nil {
		t.Fatalf("ListOpenAIOAuthAccounts() error = %v", err)
	}
	if len(matches) != 1 || fmt.Sprint(matches[0]["id"]) != "9" {
		t.Fatalf("matches = %+v", matches)
	}
	if !reflect.DeepEqual(pages, []string{"1", "2", "3"}) {
		t.Fatalf("pages = %v", pages)
	}
}

func TestListOpenAIOAuthAccountsStopsRepeatedPage(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"items": []any{
			remoteAccount(1, "other", "other@example.test", []any{}),
		}}})
	}))
	defer server.Close()
	client, err := NewClient(testConfig(server.URL), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	matches, err := client.ListOpenAIOAuthAccounts(context.Background(), "user@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 || calls.Load() != 2 {
		t.Fatalf("matches/calls = %v/%d", matches, calls.Load())
	}
}

func TestImportNewAccountPostsLocatesBindsAndVerifies(t *testing.T) {
	var mu sync.Mutex
	var requests []recordedRequest
	var listCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		entry := recordedRequest{Method: request.Method, Path: request.URL.Path, Query: request.URL.Query(), Header: request.Header.Clone()}
		if request.Method == http.MethodPost || request.Method == http.MethodPut {
			entry.Body = decodeRequest(t, request)
		}
		mu.Lock()
		requests = append(requests, entry)
		mu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/admin/accounts":
			listCalls++
			items := []any{}
			if listCalls == 2 {
				items = []any{remoteAccount(101, "user@example.test", "USER@EXAMPLE.TEST", []any{12, 13})}
			}
			writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"items": items}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/admin/accounts/data":
			writeJSON(t, writer, http.StatusCreated, map[string]any{"data": map[string]any{"account_created": 1, "account_failed": 0}})
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/admin/accounts/101":
			writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/admin/accounts/101":
			writeJSON(t, writer, http.StatusOK, map[string]any{"data": remoteAccount(101, "user@example.test", "user@example.test", []any{12, 13})})
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.String())
			writeJSON(t, writer, http.StatusNotFound, map[string]any{"error": "unexpected"})
		}
	}))
	defer server.Close()

	result, err := ImportOpenAIOAuthAccount(context.Background(), oauthSource(), testConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("ImportOpenAIOAuthAccount() error = %v", err)
	}
	if !result.OK || result.Action != "imported" || result.AccountCreated != 1 || result.AccountID != 101 || !reflect.DeepEqual(result.GroupIDs, []int{12, 13}) {
		t.Fatalf("result = %+v", result)
	}
	var post, put *recordedRequest
	for index := range requests {
		if requests[index].Method == http.MethodPost {
			post = &requests[index]
		}
		if requests[index].Method == http.MethodPut {
			put = &requests[index]
		}
	}
	if post == nil || put == nil {
		t.Fatalf("requests = %+v", requests)
	}
	if post.Header.Get("x-api-key") != "synthetic-admin-key" || post.Body["skip_default_group_bind"] != false {
		t.Fatalf("post = %+v", post)
	}
	data := post.Body["data"].(map[string]any)
	if proxies := data["proxies"].([]any); len(proxies) != 0 {
		t.Fatalf("proxies = %v", proxies)
	}
	accounts := data["accounts"].([]any)
	if len(accounts) != 1 || accounts[0].(map[string]any)["platform"] != "openai" {
		t.Fatalf("accounts = %v", accounts)
	}
	if !reflect.DeepEqual(intSlice(put.Body["group_ids"]), []int{12, 13}) {
		t.Fatalf("group PUT = %v", put.Body)
	}
}

func TestImportExistingAccountUsesPUTAndGroupObjects(t *testing.T) {
	var requests []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		entry := recordedRequest{Method: request.Method, Path: request.URL.Path}
		if request.Method == http.MethodPut {
			entry.Body = decodeRequest(t, request)
		}
		requests = append(requests, entry)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/admin/accounts":
			writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"items": []any{
				remoteAccount(102, "Custom label", "USER@example.test", []any{12, 13}),
			}}})
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/admin/accounts/102":
			writeJSON(t, writer, http.StatusCreated, map[string]any{"data": map[string]any{}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/admin/accounts/102":
			detail := remoteAccount(102, "user@example.test", "user@example.test", nil)
			delete(detail, "group_ids")
			detail["groups"] = []any{map[string]any{"id": 12}, map[string]any{"id": "13"}}
			writeJSON(t, writer, http.StatusOK, map[string]any{"data": detail})
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	result, err := ImportOpenAIOAuthAccount(context.Background(), oauthSource(), testConfig(server.URL), server.Client())
	if err != nil {
		t.Fatalf("ImportOpenAIOAuthAccount() error = %v", err)
	}
	if result.Action != "updated" || result.AccountCreated != 0 || result.AccountID != 102 || !reflect.DeepEqual(result.GroupIDs, []int{12, 13}) {
		t.Fatalf("result = %+v", result)
	}
	methods := make([]string, 0, len(requests))
	for _, request := range requests {
		methods = append(methods, request.Method)
	}
	if strings.Contains(strings.Join(methods, ","), http.MethodPost) {
		t.Fatalf("methods = %v", methods)
	}
	var update map[string]any
	for _, request := range requests {
		if request.Method == http.MethodPut {
			update = request.Body
		}
	}
	if update["name"] != "user@example.test" || update["type"] != "oauth" || update["status"] != "active" {
		t.Fatalf("update = %v", update)
	}
	credentials := update["credentials"].(map[string]any)
	if credentials["refresh_token"] != "synthetic-refresh-token" || !reflect.DeepEqual(intSlice(update["group_ids"]), []int{12, 13}) {
		t.Fatalf("update = %v", update)
	}
}

func TestImportDuplicateStopsBeforeWrite(t *testing.T) {
	var methods []string
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		methods = append(methods, request.Method)
		calls++
		items := []any{}
		if calls == 1 {
			items = []any{
				remoteAccount(1, "one", "user@example.test", []any{}),
				remoteAccount(2, "two", "USER@EXAMPLE.TEST", []any{}),
			}
		}
		writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"items": items}})
	}))
	defer server.Close()
	_, err := ImportOpenAIOAuthAccount(context.Background(), oauthSource(), testConfig(server.URL), server.Client())
	if err == nil || !strings.Contains(err.Error(), "多个同名") {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(methods, []string{http.MethodGet, http.MethodGet}) {
		t.Fatalf("methods = %v", methods)
	}
}

func TestImportVerificationRejectsPlatformEmailAndGroups(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"platform", func(account map[string]any) { account["platform"] = "grok" }, "类型校验"},
		{"type", func(account map[string]any) { account["type"] = "token" }, "类型校验"},
		{"email", func(account map[string]any) { account["credentials"] = map[string]any{"email": "other@example.test"} }, "邮箱校验"},
		{"groups", func(account map[string]any) { account["group_ids"] = []any{12} }, "分类校验"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var listCalls int
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/api/v1/admin/accounts":
					listCalls++
					items := []any{}
					if listCalls == 1 {
						items = []any{remoteAccount(8, "label", "user@example.test", []any{12, 13})}
					}
					writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"items": items}})
				case request.Method == http.MethodPut:
					writeJSON(t, writer, http.StatusOK, map[string]any{})
				case request.Method == http.MethodGet && request.URL.Path == "/api/v1/admin/accounts/8":
					account := remoteAccount(8, "user@example.test", "user@example.test", []any{12, 13})
					test.mutate(account)
					writeJSON(t, writer, http.StatusOK, map[string]any{"data": account})
				default:
					t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
				}
			}))
			defer server.Close()
			_, err := ImportOpenAIOAuthAccount(context.Background(), oauthSource(), testConfig(server.URL), server.Client())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestImportErrorRedactsAllSecrets(t *testing.T) {
	source := oauthSource()
	secrets := []string{"synthetic-admin-key", strings.TrimSpace(source.Email), source.AccessToken, source.RefreshToken, source.IDToken}
	tests := []struct {
		name    string
		handler http.Handler
	}{
		{
			name: "HTTP response",
			handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				message := strings.Join(secrets, " ")
				writeJSON(t, writer, http.StatusBadGateway, map[string]any{"message": message})
			}),
		},
		{
			name: "import result",
			handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"items": []any{}}})
					return
				}
				writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{
					"account_created": 0,
					"account_failed":  1,
					"errors":          []any{strings.Join(secrets, " ")},
				}})
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			_, err := ImportOpenAIOAuthAccount(context.Background(), source, testConfig(server.URL), server.Client())
			if err == nil {
				t.Fatal("expected error")
			}
			for _, secret := range secrets {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked %q: %s", secret, err)
				}
			}
		})
	}
}

func TestListErrorRedactsAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, http.StatusUnauthorized, map[string]any{"error": "synthetic-admin-key rejected"})
	}))
	defer server.Close()
	_, err := ListGroups(context.Background(), testConfig(server.URL), server.Client())
	if err == nil || strings.Contains(err.Error(), "synthetic-admin-key") {
		t.Fatalf("error = %v", err)
	}
}

func TestRedactOverlappingSecrets(t *testing.T) {
	got := Redact("user@example.test example.test", "example.test", "user@example.test")
	if strings.Contains(got, "user@") || strings.Contains(got, "example.test") {
		t.Fatalf("Redact() = %q", got)
	}
}

func TestImportSameEmailIsSerialized(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/admin/accounts" {
			current := active.Add(1)
			for {
				maximum := maxActive.Load()
				if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
					break
				}
			}
			time.Sleep(60 * time.Millisecond)
			active.Add(-1)
			calls.Add(1)
			writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"items": []any{}}})
			return
		}
		writeJSON(t, writer, http.StatusOK, map[string]any{"data": map[string]any{"account_created": 0, "account_failed": 1}})
	}))
	defer server.Close()

	var wait sync.WaitGroup
	start := make(chan struct{})
	errors := make(chan error, 2)
	for _, email := range []string{"User@Example.Test", "user@example.test"} {
		wait.Add(1)
		go func(email string) {
			defer wait.Done()
			<-start
			source := oauthSource()
			source.Email = email
			_, err := ImportOpenAIOAuthAccount(context.Background(), source, testConfig(server.URL), server.Client())
			errors <- err
		}(email)
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err == nil {
			t.Fatal("expected synthetic import error")
		}
	}
	if calls.Load() != 2 || maxActive.Load() != 1 {
		t.Fatalf("list calls/max active = %d/%d", calls.Load(), maxActive.Load())
	}
}

func TestInjectedHTTPClientAndTimeoutContext(t *testing.T) {
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) > 31*time.Second || time.Until(deadline) < 28*time.Second {
			t.Errorf("request deadline = %v, ok = %v", deadline, ok)
		}
		return nil, fmt.Errorf("synthetic-admin-key rt.secret user@example.test")
	})
	client := &http.Client{Transport: transport}
	_, err := ListGroups(context.Background(), testConfig("https://sub2api.example.test"), client)
	if err == nil || strings.Contains(err.Error(), "synthetic-admin-key") || strings.Contains(err.Error(), "rt.secret") {
		t.Fatalf("error = %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func intSlice(value any) []int {
	items, _ := value.([]any)
	result := make([]int, 0, len(items))
	for _, item := range items {
		parsed, _ := anyInt(item)
		result = append(result, parsed)
	}
	sort.Ints(result)
	return result
}
