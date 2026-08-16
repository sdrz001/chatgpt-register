package mailfetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDomainMailListAndGetMessage(t *testing.T) {
	listCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "domain-secret" {
			t.Fatalf("api key=%q", r.Header.Get("X-API-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/emails/remote-box":
			listCalls++
			if listCalls == 1 {
				fmt.Fprint(w, `{"messages":[{"id":"older","from_address":"noreply@openai.com","subject":"Verify email","received_at":1786842000000}],"nextCursor":"next"}`)
				return
			}
			if r.URL.Query().Get("cursor") != "next" {
				t.Fatalf("cursor=%q", r.URL.Query().Get("cursor"))
			}
			fmt.Fprint(w, `{"messages":[{"id":"newer","from_address":"noreply@openai.com","subject":"Verify email","received_at":1786842123000}]}`)
		case "/api/emails/remote-box/newer":
			fmt.Fprint(w, `{"message":{"id":"newer","from_address":"noreply@openai.com","subject":"Verify email","content":"","html":"<p>Your code is <strong>222222</strong></p>","received_at":1786842123000}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(WithHTTPClient(server.Client()))
	account := Account{
		Provider: "domain_api", RemoteMailboxID: "remote-box",
		DomainMailBaseURL: server.URL, DomainMailAPIKey: "domain-secret",
	}
	messages, err := client.ListMessages(context.Background(), account, 1)
	if err != nil || len(messages) != 1 || messages[0].ID != "newer" || listCalls != 2 {
		t.Fatalf("messages=%+v calls=%d error=%v", messages, listCalls, err)
	}
	message, err := client.GetMessage(context.Background(), account, "newer")
	if err != nil || message.ID != "newer" || message.Text != "Your code is 222222" || message.HTML != "<p>Your code is <strong>222222</strong></p>" {
		t.Fatalf("message=%+v error=%v", message, err)
	}
}

func TestDomainMailVerifyRequiresRemoteMailboxID(t *testing.T) {
	client := New()
	err := client.Verify(context.Background(), Account{
		Provider: "domain_api", DomainMailBaseURL: "https://mail.example.test", DomainMailAPIKey: "key",
	})
	if err == nil {
		t.Fatal("verify returned nil error")
	}
}
