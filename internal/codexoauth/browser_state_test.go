package codexoauth

import (
	"context"
	"strings"
	"testing"
)

func TestPhoneCodeRejected(t *testing.T) {
	for _, body := range []string{"Invalid code", "Your code expired", "验证码无效", "验证码已过期"} {
		if !phoneCodeRejected(body) {
			t.Fatalf("phoneCodeRejected(%q)=false", body)
		}
	}
	for _, body := range []string{"Enter verification code", "This phone number is invalid", "请输入验证码"} {
		if phoneCodeRejected(body) {
			t.Fatalf("phoneCodeRejected(%q)=true", body)
		}
	}
}

func TestPhoneNumberRejected(t *testing.T) {
	for _, body := range []string{
		"This phone number is already associated with another account",
		"This phone number is not supported",
		"The phone number you entered is invalid",
		"Try a different phone number.",
		"此手机号已绑定其他账号",
		"该号码无法使用，请尝试其他号码",
	} {
		if !phoneNumberRejected(body) {
			t.Fatalf("phoneNumberRejected(%q)=false", body)
		}
	}
	for _, body := range []string{
		"Enter your phone number to receive a verification code",
		"We only use your phone number for security",
		"请输入手机号",
		"Invalid code",
	} {
		if phoneNumberRejected(body) {
			t.Fatalf("phoneNumberRejected(%q)=true", body)
		}
	}
}

func TestPhoneSessionManagerReplacesRejectedNumbersWithinLimit(t *testing.T) {
	var allocated int
	var finished []bool
	manager := newPhoneSessionManager(2, func(context.Context) (*PhoneSession, error) {
		allocated++
		number := "+10000000001"
		if allocated == 2 {
			number = "+10000000002"
		}
		return &PhoneSession{
			Number:   number,
			WaitCode: func(context.Context) (string, error) { return "123456", nil },
			Finish: func(_ context.Context, success bool) error {
				finished = append(finished, success)
				return nil
			},
		}, nil
	})
	first, err := manager.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.reject(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := manager.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Number == second.Number || manager.attempts != 2 || allocated != 2 || len(finished) != 1 || finished[0] {
		t.Fatalf("first=%+v second=%+v attempts=%d allocated=%d finished=%v", first, second, manager.attempts, allocated, finished)
	}
	if err := manager.reject(context.Background()); err == nil || !strings.Contains(err.Error(), "2") || allocated != 2 {
		t.Fatalf("limit error=%v allocated=%d", err, allocated)
	}
	if len(finished) != 2 || finished[1] {
		t.Fatalf("finished=%v", finished)
	}
}

func TestClassifyPage(t *testing.T) {
	tests := []struct {
		name    string
		signals pageSignals
		want    browserState
	}{
		{"callback", pageSignals{URL: RedirectURI + "?code=x&state=y"}, stateCallback},
		{"phone input wins", pageSignals{HasEmail: true, HasPhone: true}, statePhone},
		{"phone OTP URL", pageSignals{URL: AuthBase + "/phone-verification", HasOTP: true}, statePhoneOTP},
		{"allocated phone OTP", pageSignals{HasOTP: true, PhoneAllocated: true}, statePhoneOTP},
		{"email OTP", pageSignals{URL: AuthBase + "/email-verification", HasOTP: true}, stateEmailOTP},
		{"password", pageSignals{HasPassword: true}, statePassword},
		{"email", pageSignals{HasEmail: true}, stateEmail},
		{"consent", pageSignals{URL: AuthBase + "/sign-in-with-chatgpt/codex/consent", HasAction: true}, stateConsent},
		{"wait", pageSignals{URL: AuthBase + "/loading"}, stateWait},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyPage(test.signals); got != test.want {
				t.Fatalf("classifyPage(%+v)=%q want %q", test.signals, got, test.want)
			}
		})
	}
}
