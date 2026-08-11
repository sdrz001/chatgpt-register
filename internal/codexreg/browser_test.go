package codexreg

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRegistrationLauncherDisablesImages(t *testing.T) {
	launcher := registrationLauncher(true)
	if value := launcher.Get("blink-settings"); value != "imagesEnabled=false" {
		t.Fatalf("blink-settings=%q", value)
	}
}

func TestClassifyRegistrationPage(t *testing.T) {
	tests := []struct {
		name    string
		signals registrationPageSignals
		want    registrationPageState
	}{
		{name: "disabled", signals: registrationPageSignals{HasReady: true, Body: "Your account has been deleted or deactivated"}, want: registrationStateDisabled},
		{name: "retry before ready URL", signals: registrationPageSignals{URL: "https://chatgpt.com/", HasRetry: true}, want: registrationStateRetry},
		{name: "profile birthdate", signals: registrationPageSignals{HasName: true, ProfileField: "birthdate", HasEmail: true}, want: registrationStateProfile},
		{name: "password", signals: registrationPageSignals{HasPassword: true, HasEmail: true}, want: registrationStatePassword},
		{name: "invalid code", signals: registrationPageSignals{HasCode: true, HasEmail: true, Body: "Invalid code"}, want: registrationStateCodeRejected},
		{name: "invalid Japanese code", signals: registrationPageSignals{HasCode: true, Body: "認証コードが正しくありません"}, want: registrationStateCodeRejected},
		{name: "code", signals: registrationPageSignals{HasCode: true, HasEmail: true}, want: registrationStateCode},
		{name: "email before composer", signals: registrationPageSignals{HasEmail: true, HasReady: true}, want: registrationStateEmail},
		{name: "email before ready URL", signals: registrationPageSignals{URL: "https://chatgpt.com/", HasEmail: true}, want: registrationStateEmail},
		{name: "composer", signals: registrationPageSignals{HasReady: true}, want: registrationStateReady},
		{name: "ready URL", signals: registrationPageSignals{URL: "https://chatgpt.com/"}, want: registrationStateReady},
		{name: "auth URL", signals: registrationPageSignals{URL: "https://chatgpt.com/auth/login"}, want: registrationStateWait},
		{name: "wait", signals: registrationPageSignals{}, want: registrationStateWait},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyRegistrationPage(test.signals); got != test.want {
				t.Fatalf("classifyRegistrationPage(%+v)=%q want %q", test.signals, got, test.want)
			}
		})
	}
}

func TestRegistrationBirthdate(t *testing.T) {
	now := time.Date(2026, time.August, 4, 0, 0, 0, 0, time.UTC)
	if got := registrationBirthdate("2000-02-03", now); got != "2000-02-03" {
		t.Fatalf("explicit birthdate=%q", got)
	}
	if got := registrationBirthdate("25", now); got != "2001-08-04" {
		t.Fatalf("age birthdate=%q", got)
	}
	if got := registrationBirthdate("invalid", now); got != "1996-08-04" {
		t.Fatalf("fallback birthdate=%q", got)
	}
}

func TestWaitRegistrationSuccessHold(t *testing.T) {
	start := time.Now()
	waitRegistrationSuccessHold(context.Background(), 30*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("hold returned too early: %s", elapsed)
	}
}

func TestWaitRegistrationSuccessHoldStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	waitRegistrationSuccessHold(ctx, time.Second)
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Fatalf("canceled hold took %s", elapsed)
	}
}

func TestRegistrationPageDiagnosticRedactsSensitiveValues(t *testing.T) {
	diagnostic := registrationPageDiagnostic(registrationPageSignals{
		URL:   "https://chatgpt.com/auth/login?email=person@example.test&code=123456",
		Title: "Verify person@example.test",
		Body:  "Use code 123456 or call +12025550123",
	})
	for _, secret := range []string{"person@example.test", "123456", "+12025550123", "?email="} {
		if strings.Contains(diagnostic, secret) {
			t.Fatalf("diagnostic leaked %q: %s", secret, diagnostic)
		}
	}
	for _, expected := range []string{"https://chatgpt.com/auth/login", "[email]", "[code]", "[number]"} {
		if !strings.Contains(diagnostic, expected) {
			t.Fatalf("diagnostic missing %q: %s", expected, diagnostic)
		}
	}
}
