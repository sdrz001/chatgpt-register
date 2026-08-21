package mailfetch

import (
	"testing"
	"time"

	"chatgpt-register/internal/mailcom"
)

func TestMailComClientAllowsAutomaticSecretDiscovery(t *testing.T) {
	client := New()
	account := Account{Email: "user@musician.org", Password: "mail-password", Provider: mailcom.Provider}
	created, err := client.mailComClient(account)
	if err != nil {
		t.Fatal(err)
	}
	if created == nil {
		t.Fatal("expected mail.com protocol client")
	}
	if isDomainMail(account) {
		t.Fatal("mail.com provider routed to domain mail")
	}
}

func TestMailComClientCachePerCredential(t *testing.T) {
	client := New()
	account := Account{
		Email: "User@Mail.com", Password: "mail-password",
		Provider: mailcom.Provider, MailComOAuthPublicSecret: "secret",
	}

	first, err := client.mailComClient(account)
	if err != nil {
		t.Fatal(err)
	}
	same, err := client.mailComClient(account)
	if err != nil {
		t.Fatal(err)
	}
	if first != same {
		t.Fatal("expected cached mail.com client reuse")
	}

	rotated := account
	rotated.Password = "rotated-password"
	changed, err := client.mailComClient(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("expected new client after password change")
	}
}

func TestMailComMessagesMapToMailfetchMessages(t *testing.T) {
	received := time.Date(2026, 8, 18, 10, 30, 0, 0, time.UTC)
	messages := mailComMessages([]mailcom.Message{{
		ID: "message-1", Subject: "Your verification code", Sender: "OpenAI <noreply@openai.com>",
		ReceivedAt: received, Body: "code 123456",
	}})
	if len(messages) != 1 {
		t.Fatalf("messages=%+v", messages)
	}
	message := messages[0]
	if message.ID != "message-1" || message.Subject != "Your verification code" {
		t.Fatalf("message=%+v", message)
	}
	if message.From != "OpenAI <noreply@openai.com>" || !message.ReceivedAt.Equal(received) {
		t.Fatalf("message=%+v", message)
	}
	if message.Text != "code 123456" || message.HTML != "" {
		t.Fatalf("message=%+v", message)
	}
}
