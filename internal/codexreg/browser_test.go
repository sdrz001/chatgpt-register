package codexreg

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestClassifyRegistrationTrialResponse(t *testing.T) {
	eligible := `{"accounts":{"account":{"eligible_promo_campaigns":{"plus":{"metadata":{"plan_name":"plus","discount":{"percentage":100},"duration":{"num_periods":1,"period":"month"}}}}}}}`
	if status := classifyRegistrationTrialResponse([]byte(eligible)); status != registrationTrialEligible {
		t.Fatalf("eligible status=%q", status)
	}
	ineligible := `{"accounts":{"account":{"eligible_promo_campaigns":{}}}}`
	if status := classifyRegistrationTrialResponse([]byte(ineligible)); status != registrationTrialIneligible {
		t.Fatalf("ineligible status=%q", status)
	}
	pending := `{"accounts":{"account":{"account_plan":{"plan_type":"free"}}}}`
	if status := classifyRegistrationTrialResponse([]byte(pending)); status != registrationTrialPending {
		t.Fatalf("pending status=%q", status)
	}
}

func TestRegistrationTrialStatusSettled(t *testing.T) {
	if registrationTrialStatusSettled(registrationTrialIneligible, time.Second, 1) {
		t.Fatal("early ineligible response settled")
	}
	if registrationTrialStatusSettled(registrationTrialIneligible, 12*time.Second, 2) {
		t.Fatal("insufficient ineligible confirmations settled")
	}
	if !registrationTrialStatusSettled(registrationTrialIneligible, 12*time.Second, 3) {
		t.Fatal("stable ineligible response did not settle")
	}
	if !registrationTrialStatusSettled(registrationTrialEligible, 0, 0) {
		t.Fatal("eligible response did not settle immediately")
	}
}

func TestRegistrationLauncherLoadsAllResources(t *testing.T) {
	launcher := registrationLauncher(true)
	if value := launcher.Get("blink-settings"); value != "" {
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
		{name: "profile segmented birthdate", signals: registrationPageSignals{HasName: true, ProfileField: "birthdate_segments", ProfileValue: "2026-08-12"}, want: registrationStateProfile},
		{name: "password", signals: registrationPageSignals{HasPassword: true, HasEmail: true}, want: registrationStatePassword},
		{name: "password choice", signals: registrationPageSignals{HasCode: true, HasPasswordSignup: true}, want: registrationStatePasswordChoice},
		{name: "invalid code before password choice", signals: registrationPageSignals{HasCode: true, HasPasswordSignup: true, CodeInvalid: true}, want: registrationStateCodeRejected},
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

func TestRegistrationProfileValueSupportsAgeAndBirthdateTemplates(t *testing.T) {
	now := time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		meta  registrationInputMeta
		field string
		value string
	}{
		{name: "legacy age", meta: registrationInputMeta{Name: "age", Type: "text", Value: "30"}, field: "age", value: "35"},
		{name: "native date", meta: registrationInputMeta{Type: "date", AriaLabel: "Birth date"}, field: "birthdate", value: "1991-08-12"},
		{name: "localized date", meta: registrationInputMeta{Type: "text", Placeholder: "出生日期", Value: "2026/08/12"}, field: "birthdate", value: "1991/08/12"},
		{name: "US date", meta: registrationInputMeta{Type: "text", Placeholder: "MM/DD/YYYY", Value: "08/12/2026"}, field: "birthdate", value: "08/12/1991"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			field, value := registrationProfileValue("35", test.meta, now)
			if field != test.field || value != test.value {
				t.Fatalf("field/value=%q/%q want %q/%q", field, value, test.field, test.value)
			}
		})
	}
}

func TestNormalizeProxySupportedShapes(t *testing.T) {
	for input, want := range map[string]string{
		"proxy.test:8080":                    "http://proxy.test:8080",
		"user:pass@proxy.test:8080":          "http://user:pass@proxy.test:8080",
		"user@name:p/a ss@proxy.test:8080":   "http://user%40name:p%2Fa%20ss@proxy.test:8080",
		"proxy.test:8080:user:pass":          "http://user:pass@proxy.test:8080",
		"http://user:pass@proxy.test:8080":   "http://user:pass@proxy.test:8080",
		"socks5://user:pass@proxy.test:1080": "socks5://user:pass@proxy.test:1080",
	} {
		if got := normalizeProxy(input); got != want {
			t.Fatalf("normalizeProxy(%q)=%q want %q", input, got, want)
		}
	}
}

func TestRegistrationBirthdateSegments(t *testing.T) {
	values := registrationBirthdateSegments("1991-08-12")
	for kind, want := range map[string]string{"year": "1991", "month": "08", "day": "12"} {
		if values[kind] != want {
			t.Fatalf("segment %s=%q want %q", kind, values[kind], want)
		}
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
		Body:  "Use code 123456, birthday 1991 / 02 / 03, or call +12025550123",
	})
	for _, secret := range []string{"person@example.test", "123456", "1991 / 02 / 03", "+12025550123", "?email="} {
		if strings.Contains(diagnostic, secret) {
			t.Fatalf("diagnostic leaked %q: %s", secret, diagnostic)
		}
	}
	for _, expected := range []string{"https://chatgpt.com/auth/login", "[email]", "[code]", "[date]", "[number]"} {
		if !strings.Contains(diagnostic, expected) {
			t.Fatalf("diagnostic missing %q: %s", expected, diagnostic)
		}
	}
}
