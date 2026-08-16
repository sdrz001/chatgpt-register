package domainmail

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientGenerateListAndReadMessages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "secret-key" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/config":
			fmt.Fprint(w, `{"defaultRole":"CIVILIAN","emailDomains":"moemail.app,example.com","adminContact":"admin@example.com","maxEmails":"10"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/emails/generate":
			var input GenerateRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Name != "test" || input.Domain != "moemail.app" || input.ExpiryTime != 3600000 {
				t.Fatalf("input=%+v", input)
			}
			fmt.Fprint(w, `{"id":"box-1","email":"test@moemail.app"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/emails":
			if r.URL.Query().Get("cursor") != "cursor-1" {
				t.Fatalf("cursor=%q", r.URL.Query().Get("cursor"))
			}
			fmt.Fprint(w, `{"emails":[{"id":"box-1","address":"test@moemail.app","createdAt":"2026-08-16T01:00:00Z","expiresAt":"2026-08-17T01:00:00Z","userId":"user-1"}],"nextCursor":"cursor-2","total":1}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/emails/box-1":
			fmt.Fprint(w, `{"messages":[{"id":"msg-1","from_address":"noreply@openai.com","subject":"Your code 123456","received_at":1786842123000}],"nextCursor":"msg-cursor","total":1}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/emails/box-1/msg-1":
			fmt.Fprint(w, `{"message":{"id":"msg-1","from_address":"noreply@openai.com","subject":"Your code 123456","content":"123456","html":"<b>123456</b>","received_at":1786842123000}}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, APIKey: "secret-key"}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	config, err := client.SystemConfig(context.Background())
	if err != nil || len(config.Domains) != 2 || config.Domains[0] != "moemail.app" {
		t.Fatalf("config=%+v error=%v", config, err)
	}
	mailbox, err := client.Generate(context.Background(), GenerateRequest{Name: "test", Domain: "moemail.app", ExpiryTime: 3600000})
	if err != nil || mailbox.ID != "box-1" || mailbox.Email != "test@moemail.app" {
		t.Fatalf("mailbox=%+v error=%v", mailbox, err)
	}
	mailboxes, err := client.ListMailboxes(context.Background(), "cursor-1")
	if err != nil || len(mailboxes.Items) != 1 || mailboxes.NextCursor != "cursor-2" {
		t.Fatalf("mailboxes=%+v error=%v", mailboxes, err)
	}
	messages, err := client.ListMessages(context.Background(), "box-1", "")
	if err != nil || len(messages.Items) != 1 || messages.Items[0].From != "noreply@openai.com" || messages.NextCursor != "msg-cursor" {
		t.Fatalf("messages=%+v error=%v", messages, err)
	}
	message, err := client.GetMessage(context.Background(), "box-1", "msg-1")
	if err != nil || message.ID != "msg-1" || message.Text != "123456" || message.HTML != "<b>123456</b>" || message.ReceivedAt.IsZero() {
		t.Fatalf("message=%+v error=%v", message, err)
	}
}

func TestDecodeDomainsSupportsOfficialAndWrappedFormats(t *testing.T) {
	for name, input := range map[string]string{
		"official": `{"emailDomains":"moemail.app, example.com"}`,
		"wrapped":  `{"data":{"emailDomains":"moemail.app,example.com"}}`,
		"array":    `{"domains":["moemail.app",{"name":"example.com"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			value, err := decode([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			domains := decodeDomains(value)
			if len(domains) != 2 || domains[0] != "moemail.app" || domains[1] != "example.com" {
				t.Fatalf("domains=%v", domains)
			}
		})
	}
}

func TestGenerateFallsBackToMailboxList(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			fmt.Fprint(w, `{"success":true}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"email_id":"box-2","name":"fresh","domain":"moemail.app"}]}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, APIKey: "key"}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := client.Generate(context.Background(), GenerateRequest{Name: "fresh", Domain: "moemail.app", ExpiryTime: 0})
	if err != nil || mailbox.ID != "box-2" || mailbox.Email != "fresh@moemail.app" || requests != 2 {
		t.Fatalf("mailbox=%+v requests=%d error=%v", mailbox, requests, err)
	}
}

func TestDeleteMailboxUsesOfficialEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/emails/remote-box" || r.Header.Get("X-API-Key") != "secret" {
			t.Fatalf("method=%s path=%s key=%q", r.Method, r.URL.Path, r.Header.Get("X-API-Key"))
		}
		fmt.Fprint(w, `{"success":true}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, APIKey: "secret"}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteMailbox(context.Background(), "remote-box"); err != nil {
		t.Fatal(err)
	}
}

func TestClientRedactsAPIKeyFromErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := `{"error":"bad request very-secret ` + r.URL.String() + `"}`
		http.Error(w, message, http.StatusBadRequest)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, APIKey: "very-secret"}, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SystemConfig(context.Background())
	if err == nil || strings.Contains(err.Error(), "very-secret") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("error=%v", err)
	}
}

func TestValidExpiryTime(t *testing.T) {
	for _, value := range []int64{0, 3600000, 86400000, 604800000} {
		if !ValidExpiryTime(value) {
			t.Fatalf("expiry %d rejected", value)
		}
	}
	if ValidExpiryTime(1000) {
		t.Fatal("invalid expiry accepted")
	}
}
