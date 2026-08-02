package codexoauth

import "testing"

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
