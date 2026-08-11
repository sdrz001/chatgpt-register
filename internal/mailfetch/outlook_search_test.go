package mailfetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestSearchMessagesUsesAllMailboxSearchAndPagination(t *testing.T) {
	page := 0
	client := New(WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "login.microsoftonline.com":
			return jsonResponse(http.StatusOK, `{"access_token":"access","expires_in":3600}`), nil
		case "graph.microsoft.com":
			page++
			if page == 1 {
				if request.URL.Path != "/v1.0/me/messages" {
					t.Fatalf("path=%s", request.URL.Path)
				}
				if request.URL.Query().Get("$search") != `"plus"` || request.URL.Query().Get("$top") != "2" {
					t.Fatalf("query=%v", request.URL.Query())
				}
				return jsonResponse(http.StatusOK, `{"value":[{"id":"older","subject":"Plus receipt","receivedDateTime":"2026-01-01T00:00:00Z","from":{"emailAddress":{"address":"noreply@openai.com","name":"OpenAI"}}}],"@odata.nextLink":"https://graph.microsoft.com/v1.0/me/messages?page=2"}`), nil
			}
			return jsonResponse(http.StatusOK, `{"value":[{"id":"newer","subject":"Plus renewal","receivedDateTime":"2026-02-01T00:00:00Z","from":{"emailAddress":{"address":"noreply@openai.com","name":"OpenAI"}}}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
		}
	})}))

	messages, err := client.SearchMessages(context.Background(), Account{ClientID: "client", RefreshToken: "refresh"}, "plus", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].ID != "newer" || messages[1].ID != "older" || page != 2 {
		t.Fatalf("messages=%+v page=%d", messages, page)
	}
}

func TestListMessagesReturnsFolderErrorsInsteadOfEmptyResult(t *testing.T) {
	client := New(WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "login.microsoftonline.com" {
			return jsonResponse(http.StatusOK, `{"access_token":"access","expires_in":3600}`), nil
		}
		return jsonResponse(http.StatusInternalServerError, `{"error":"failed"}`), nil
	})}))

	messages, err := client.ListMessages(context.Background(), Account{ClientID: "client", RefreshToken: "refresh"}, 20)
	if err == nil || len(messages) != 0 {
		t.Fatalf("messages=%+v error=%v", messages, err)
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
