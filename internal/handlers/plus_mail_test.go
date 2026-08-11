package handlers

import (
	"strings"
	"testing"

	"chatgpt-register/internal/mailfetch"
)

func TestLooksLikePlusActivationMailTemplates(t *testing.T) {
	tests := []struct {
		name    string
		message mailfetch.Message
		want    bool
	}{
		{
			name: "icloud split receipt fields",
			message: mailfetch.Message{
				From:    "noreply@openai.com",
				Subject: "Your receipt",
				HTML:    `<div class="fr">OpenAI</div><div class="pn">Plus</div><div class="label">Amount</div><div>$20.00</div>`,
			},
			want: true,
		},
		{
			name: "icloud receipt not hidden by distant cancellation",
			message: mailfetch.Message{
				Subject: "邮箱历史邮件",
				Text: "OpenAI ChatGPT Plus subscription canceled " + strings.Repeat("旧邮件内容 ", 200) +
					"OpenAI Plus receipt Amount $20.00",
			},
			want: true,
		},
		{
			name: "renewal",
			message: mailfetch.Message{
				From:    "noreply@tm.openai.com",
				Subject: "Your ChatGPT Plus renewal",
				Text:    "Your membership has renewed. Total $20.00.",
			},
			want: true,
		},
		{
			name: "apple receipt",
			message: mailfetch.Message{
				From:     "no_reply@apple.com",
				FromName: "Apple",
				Subject:  "Your receipt from Apple",
				Text:     "ChatGPT Plus monthly subscription Total $19.99",
			},
			want: true,
		},
		{
			name: "chinese split purchase",
			message: mailfetch.Message{
				FromName: "OpenAI",
				Subject:  "付款收据",
				HTML:     `<div>ChatGPT</div><div>Plus</div><div>购买日期</div><div>金额 US$20.00</div>`,
			},
			want: true,
		},
		{
			name: "canceled",
			message: mailfetch.Message{
				From:    "noreply@openai.com",
				Subject: "ChatGPT Plus subscription canceled",
				Text:    "Your subscription has been canceled.",
			},
			want: false,
		},
		{
			name: "refund",
			message: mailfetch.Message{
				From:    "noreply@openai.com",
				Subject: "ChatGPT Plus receipt",
				Text:    "Refund issued for your payment.",
			},
			want: false,
		},
		{
			name: "marketing",
			message: mailfetch.Message{
				From:    "news@openai.com",
				Subject: "Discover ChatGPT Plus",
				Text:    "Learn about more features and faster responses.",
			},
			want: false,
		},
		{
			name: "unrelated plus",
			message: mailfetch.Message{
				From:    "store@example.com",
				Subject: "Plus membership receipt",
				Text:    "Amount $20.00",
			},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := looksLikePlusActivationMail(test.message); got != test.want {
				t.Fatalf("looksLikePlusActivationMail()=%v want %v message=%+v", got, test.want, test.message)
			}
		})
	}
}
