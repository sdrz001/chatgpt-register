package codexreg

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	helperModeEnv = "CODEXREG_SIDECAR_HELPER"
	helperCaseEnv = "CODEXREG_SIDECAR_CASE"
)

func TestRegisterDefaultsToRod(t *testing.T) {
	original := browserRegister
	defer func() { browserRegister = original }()

	called := false
	browserRegister = func(ctx context.Context, in Input) (string, error) {
		called = true
		if in.Backend != BackendRod {
			t.Fatalf("backend=%q want %q", in.Backend, BackendRod)
		}
		return sidecarTestToken(), nil
	}
	result, err := Register(context.Background(), Input{
		Email:     "rod@example.test",
		Password:  "rod-password",
		FullName:  "Rod User",
		Age:       "25",
		FetchCode: func(context.Context) (string, error) { return "123456", nil },
	})
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	if !called || result.AccessToken != sidecarTestToken() {
		t.Fatalf("Rod route not used: called=%v token=%q", called, result.AccessToken)
	}
}

func TestNormalizeBackend(t *testing.T) {
	for input, want := range map[string]string{
		"":                  BackendRod,
		" ROD ":             BackendRod,
		"CLOAKBROWSER":      BackendCloakBrowser,
		BackendCloakBrowser: BackendCloakBrowser,
	} {
		got, err := normalizeBackend(input)
		if err != nil || got != want {
			t.Fatalf("normalizeBackend(%q)=(%q, %v) want %q", input, got, err, want)
		}
	}
	if _, err := normalizeBackend("other"); err == nil {
		t.Fatal("normalizeBackend(other) returned nil error")
	}
}

func TestRedactSensitiveKeepsAuthHostsAndMasksLongJWT(t *testing.T) {
	jwt := "abc." + strings.Repeat("p", 16) + "." + strings.Repeat("s", 8)
	clean := redactSensitive("host=auth.openai.com host=chatgpt.com token="+jwt, Input{}, "", "")
	if !strings.Contains(clean, "auth.openai.com") || !strings.Contains(clean, "chatgpt.com") {
		t.Fatalf("auth hosts were redacted: %s", clean)
	}
	if strings.Contains(clean, jwt) || (!strings.Contains(clean, "[token]") && !strings.Contains(clean, "[redacted]")) {
		t.Fatalf("JWT was not redacted: %s", clean)
	}
}

func TestRedactSensitiveKeepsNaturalTokenLogsAndMasksCredentials(t *testing.T) {
	status := "session token acquired; accessToken plan=free"
	if clean := redactSensitive(status, Input{}, "", ""); clean != status {
		t.Fatalf("natural status log was redacted: %q", clean)
	}
	clean := redactSensitive("access_token=secret-value; Bearer bearer-value", Input{}, "", "")
	if strings.Contains(clean, "secret-value") || strings.Contains(clean, "bearer-value") {
		t.Fatalf("credential leaked: %s", clean)
	}
	if clean != "access_token=[redacted]; Bearer [redacted]" {
		t.Fatalf("unexpected credential redaction: %q", clean)
	}
}

func TestSidecarSuccessAndRedaction(t *testing.T) {
	var logs []string
	in := sidecarTestInput(t, "success")
	in.Log = func(format string, a ...any) {
		logs = append(logs, fmt.Sprintf(format, a...))
	}
	result, err := Register(context.Background(), in)
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	if result.AccessToken != sidecarTestToken() || result.AccountID != "account-id" || result.UserID != "user-id" {
		t.Fatalf("unexpected result: %+v", result)
	}
	joined := strings.Join(logs, "\n")
	assertNoSidecarSecrets(t, joined)
	if !strings.Contains(joined, "[redacted]") {
		t.Fatalf("redacted log marker missing: %s", joined)
	}
}

func TestSidecarCodeAndScreenshot(t *testing.T) {
	in := sidecarTestInput(t, "code_screenshot")
	fetches := 0
	in.FetchCode = func(context.Context) (string, error) {
		fetches++
		return "654321", nil
	}
	var shot []byte
	in.SaveShot = func(png []byte) { shot = append([]byte(nil), png...) }
	result, err := Register(context.Background(), in)
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	if fetches != 1 {
		t.Fatalf("FetchCode calls=%d want 1", fetches)
	}
	if result.AccessToken != sidecarTestToken() {
		t.Fatalf("access token=%q", result.AccessToken)
	}
	if string(shot) != string(sidecarTestPNG()) {
		t.Fatalf("screenshot=%x want %x", shot, sidecarTestPNG())
	}
}

func TestSidecarFetchCodeError(t *testing.T) {
	in := sidecarTestInput(t, "fetch_error")
	in.FetchCode = func(context.Context) (string, error) {
		return "", errors.New("mail failed for sensitive@example.test code 654321")
	}
	_, err := Register(context.Background(), in)
	if err == nil {
		t.Fatal("Register() returned nil error")
	}
	assertNoSidecarSecrets(t, err.Error())
}

func TestSidecarErrors(t *testing.T) {
	t.Run("sidecar error redacts stderr", func(t *testing.T) {
		_, err := Register(context.Background(), sidecarTestInput(t, "error"))
		if err == nil {
			t.Fatal("Register() returned nil error")
		}
		message := err.Error()
		assertNoSidecarSecrets(t, message)
		if !strings.Contains(message, "sidecar stderr") || !strings.Contains(message, "[redacted]") {
			t.Fatalf("sanitized stderr missing: %s", message)
		}
	})

	t.Run("account taken", func(t *testing.T) {
		_, err := Register(context.Background(), sidecarTestInput(t, "account_taken"))
		if !errors.Is(err, ErrAccountTaken) {
			t.Fatalf("Register() error=%v want ErrAccountTaken", err)
		}
	})
}

func TestSidecarRejectsMalformedProtocol(t *testing.T) {
	for _, mode := range []string{
		"malformed_json",
		"wrong_version",
		"wrong_request",
		"unknown_type",
		"empty_result",
		"invalid_screenshot",
		"oversized_line",
	} {
		t.Run(mode, func(t *testing.T) {
			_, err := Register(context.Background(), sidecarTestInput(t, mode))
			if err == nil {
				t.Fatal("Register() returned nil error")
			}
			assertNoSidecarSecrets(t, err.Error())
		})
	}
}

func TestResolveSidecarCommand(t *testing.T) {
	t.Run("explicit", func(t *testing.T) {
		python, script, err := resolveSidecarCommand(Input{PythonExecutable: "PYTHON", SidecarScript: "SCRIPT"})
		if err != nil || python != "PYTHON" || script != "SCRIPT" {
			t.Fatalf("resolveSidecarCommand()=(%q, %q, %v)", python, script, err)
		}
	})
	t.Run("default virtual environment", func(t *testing.T) {
		for _, root := range sidecarProjectRoots() {
			candidate := root + string(os.PathSeparator) + ".venv" + string(os.PathSeparator) + "Scripts" + string(os.PathSeparator) + "python.exe"
			if _, err := os.Stat(candidate); err == nil {
				python, _, resolveErr := resolveSidecarCommand(Input{SidecarScript: "SCRIPT"})
				if resolveErr != nil || python != candidate {
					t.Fatalf("default python=(%q, %v) want %q", python, resolveErr, candidate)
				}
				return
			}
		}
		t.Skip("project .venv/Scripts/python.exe is absent")
	})
}

func TestSidecarNormalizesInvalidUTF8(t *testing.T) {
	var logs []string
	session := sidecarSession{input: Input{Log: func(format string, a ...any) {
		logs = append(logs, fmt.Sprintf(format, a...))
	}}}
	_, err := session.handle(sidecarMessage{Type: "log", Message: "update \xa1\xfa available"})
	if err != nil {
		t.Fatalf("handle() error: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "update � available") {
		t.Fatalf("normalized log=%q", logs)
	}
}

func TestSidecarOversizedScreenshot(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(make([]byte, sidecarScreenshotLimit+1))
	_, err := decodeSidecarScreenshot(encoded)
	if err == nil || !strings.Contains(err.Error(), "1MiB") {
		t.Fatalf("decodeSidecarScreenshot() error=%v", err)
	}
}

func TestSidecarCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Register(ctx, sidecarTestInput(t, "cancel"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Register() error=%v want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestSidecarHelperProcess(t *testing.T) {
	if os.Getenv(helperModeEnv) != "1" {
		return
	}
	if err := runSidecarHelper(os.Getenv(helperCaseEnv)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func sidecarTestInput(t *testing.T, mode string) Input {
	t.Helper()
	t.Setenv(helperModeEnv, "1")
	t.Setenv(helperCaseEnv, mode)
	t.Setenv("CODEXREG_TEST_LICENSE", "inherited-license")
	return Input{
		Email:            "sensitive@example.test",
		Password:         "secret-password",
		FullName:         "Sidecar User",
		Age:              "25",
		Proxy:            "proxy-user:proxy-password@127.0.0.1:8080",
		Headless:         true,
		Backend:          BackendCloakBrowser,
		PythonExecutable: os.Args[0],
		SidecarScript:    "-test.run=^TestSidecarHelperProcess$",
		FetchCode:        func(context.Context) (string, error) { return "654321", nil },
	}
}

func runSidecarHelper(mode string) error {
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return fmt.Errorf("missing start message: %v", scanner.Err())
	}
	var raw map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
		return err
	}
	if _, exists := raw["license"]; exists {
		return errors.New("start message contains license")
	}
	if os.Getenv("CODEXREG_TEST_LICENSE") != "inherited-license" {
		return errors.New("license environment was not inherited")
	}
	if os.Getenv("PYTHONIOENCODING") != "utf-8" || os.Getenv("PYTHONUTF8") != "1" {
		return errors.New("UTF-8 sidecar environment was not configured")
	}
	var start sidecarStartMessage
	if err := json.Unmarshal(scanner.Bytes(), &start); err != nil {
		return err
	}
	if start.Version != sidecarProtocolVersion || start.Type != "start" || start.RequestID == "" {
		return fmt.Errorf("invalid start envelope: %+v", start)
	}
	if start.Payload.Email != "sensitive@example.test" || start.Payload.Password != "secret-password" || start.Payload.FullName != "Sidecar User" || start.Payload.Age != "25" || start.Payload.Proxy != "http://proxy-user:proxy-password@127.0.0.1:8080" || !start.Payload.Headless {
		return fmt.Errorf("invalid start payload")
	}
	encoder := json.NewEncoder(os.Stdout)
	send := func(message sidecarMessage) error {
		message.Version = sidecarProtocolVersion
		message.RequestID = start.RequestID
		return encoder.Encode(message)
	}
	if mode == "malformed_json" {
		_, err := fmt.Fprintln(os.Stdout, "{")
		return err
	}
	if mode == "wrong_version" {
		return encoder.Encode(sidecarMessage{Version: 2, Type: "ready", RequestID: start.RequestID})
	}
	if mode == "wrong_request" {
		return encoder.Encode(sidecarMessage{Version: 1, Type: "ready", RequestID: "other-request"})
	}
	if mode == "unknown_type" {
		return send(sidecarMessage{Type: "mystery"})
	}
	if mode == "oversized_line" {
		_, err := fmt.Fprintln(os.Stdout, strings.Repeat("x", sidecarScannerLimit+1))
		return err
	}
	if err := send(sidecarMessage{Type: "ready"}); err != nil {
		return err
	}

	switch mode {
	case "success":
		if err := send(sidecarMessage{Type: "log", Message: "email sensitive@example.test password secret-password proxy-password token " + sidecarTestToken() + " code 654321"}); err != nil {
			return err
		}
		return send(sidecarMessage{Type: "result", AccessToken: sidecarTestToken()})
	case "code_screenshot":
		if err := send(sidecarMessage{Type: "code_request"}); err != nil {
			return err
		}
		if !scanner.Scan() {
			return fmt.Errorf("missing code response: %v", scanner.Err())
		}
		var response sidecarMessage
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			return err
		}
		if response.Version != 1 || response.Type != "code_response" || response.RequestID != start.RequestID || response.Code != "654321" {
			return fmt.Errorf("invalid code response: %+v", response)
		}
		if err := send(sidecarMessage{Type: "screenshot", Data: base64.StdEncoding.EncodeToString(sidecarTestPNG())}); err != nil {
			return err
		}
		return send(sidecarMessage{Type: "result", AccessToken: sidecarTestToken()})
	case "fetch_error":
		if err := send(sidecarMessage{Type: "code_request"}); err != nil {
			return err
		}
		time.Sleep(30 * time.Second)
		return nil
	case "error":
		_, _ = fmt.Fprintln(os.Stderr, "stderr sensitive@example.test secret-password proxy-password 654321 "+sidecarTestToken())
		return send(sidecarMessage{Type: "error", Code: "browser_error", Message: "failure for sensitive@example.test secret-password proxy-password 654321 " + sidecarTestToken()})
	case "account_taken":
		return send(sidecarMessage{Type: "error", Code: "account_taken", Message: "already used"})
	case "empty_result":
		return send(sidecarMessage{Type: "result"})
	case "invalid_screenshot":
		return send(sidecarMessage{Type: "screenshot", Data: "not-base64"})
	case "cancel":
		time.Sleep(30 * time.Second)
		return nil
	default:
		return fmt.Errorf("unknown helper case %q", mode)
	}
}

func sidecarTestToken() string {
	payload := `{"https://api.openai.com/auth":{"chatgpt_account_id":"account-id","chatgpt_user_id":"user-id","chatgpt_plan_type":"free"},"https://api.openai.com/profile":{"email":"result@example.test"}}`
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func sidecarTestPNG() []byte {
	return []byte("\x89PNG\r\n\x1a\nsidecar-test")
}

func assertNoSidecarSecrets(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{
		"sensitive@example.test",
		"secret-password",
		"proxy-password",
		"654321",
		sidecarTestToken(),
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("sensitive value leaked: %q in %s", secret, text)
		}
	}
}
