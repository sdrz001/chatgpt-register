package codexoauth

import (
	"context"
	"fmt"
	"strings"
)

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

type phoneSessionManager struct {
	maxAttempts int
	attempts    int
	current     *PhoneSession
	acquireFn   func(context.Context) (*PhoneSession, error)
}

func newPhoneSessionManager(maxAttempts int, acquireFn func(context.Context) (*PhoneSession, error)) *phoneSessionManager {
	if maxAttempts < 1 {
		maxAttempts = 3
	}
	return &phoneSessionManager{maxAttempts: maxAttempts, acquireFn: acquireFn}
}

func (m *phoneSessionManager) acquire(ctx context.Context) (*PhoneSession, error) {
	if m.current != nil {
		return m.current, nil
	}
	if m.acquireFn == nil {
		return nil, fmt.Errorf("Codex OAuth 需要新增手机号，请先配置动态接码")
	}
	if m.attempts >= m.maxAttempts {
		return nil, fmt.Errorf("Codex OAuth 手机号尝试已达到上限 %d 次", m.maxAttempts)
	}
	session, err := m.acquireFn(ctx)
	if err != nil {
		return nil, err
	}
	if session == nil || strings.TrimSpace(session.Number) == "" || session.WaitCode == nil {
		return nil, fmt.Errorf("动态接码返回的号码会话无效")
	}
	m.attempts++
	m.current = session
	return session, nil
}

func (m *phoneSessionManager) reject(ctx context.Context) error {
	if m.current != nil && m.current.Finish != nil {
		if err := m.current.Finish(ctx, false); err != nil {
			return fmt.Errorf("取消不可用接码订单失败: %w", err)
		}
	}
	m.current = nil
	if m.attempts >= m.maxAttempts {
		return fmt.Errorf("Codex OAuth 手机号尝试已达到上限 %d 次", m.maxAttempts)
	}
	return nil
}

func phoneCodeRejected(body string) bool {
	value := strings.ToLower(strings.TrimSpace(body))
	return containsAny(value, "invalid code", "incorrect code", "code expired", "验证码错误", "验证码无效", "验证码已过期")
}

func phoneNumberRejected(body string) bool {
	value := strings.ToLower(strings.TrimSpace(body))
	return containsAny(value,
		"phone number is already associated", "phone number is already linked", "phone number has already been used", "phone number is already in use",
		"phone number is not supported", "phone number isn't supported", "phone number cannot be used", "phone number can't be used",
		"phone number you entered is invalid", "invalid phone number", "unsupported phone number", "try a different phone number",
		"手机号已绑定", "手机号码已绑定", "号码已绑定", "手机号已被使用", "手机号码已被使用",
		"手机号不受支持", "手机号码不受支持", "不支持此手机号", "手机号无效", "手机号码无效",
		"该号码无法使用", "此号码无法使用", "手机号无法使用", "手机号码无法使用",
	)
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
