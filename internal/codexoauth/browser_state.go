package codexoauth

import "strings"

type browserState string

const (
	stateWait     browserState = "wait"
	stateCallback browserState = "callback"
	stateEmail    browserState = "email"
	statePassword browserState = "password"
	statePhone    browserState = "phone"
	stateEmailOTP browserState = "email_otp"
	statePhoneOTP browserState = "phone_otp"
	stateConsent  browserState = "consent"
)

type pageSignals struct {
	URL            string
	Body           string
	HasEmail       bool
	HasPassword    bool
	HasPhone       bool
	HasOTP         bool
	HasAction      bool
	PhoneAllocated bool
}

func classifyPage(signals pageSignals) browserState {
	urlText := strings.ToLower(signals.URL)
	body := strings.ToLower(signals.Body)
	if strings.HasPrefix(urlText, strings.ToLower(RedirectURI)) {
		return stateCallback
	}
	if signals.HasPhone {
		return statePhone
	}
	if signals.HasOTP {
		if signals.PhoneAllocated || containsAny(urlText+" "+body, "phone", "sms", "text message", "手机", "短信") {
			return statePhoneOTP
		}
		return stateEmailOTP
	}
	if signals.HasPassword {
		return statePassword
	}
	if signals.HasEmail {
		return stateEmail
	}
	if signals.HasAction && containsAny(urlText+" "+body, "consent", "organization", "workspace", "authorize", "授权", "同意", "continue") {
		return stateConsent
	}
	return stateWait
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
