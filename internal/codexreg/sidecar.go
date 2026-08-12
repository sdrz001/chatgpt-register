package codexreg

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	sidecarProtocolVersion = 1
	sidecarScannerLimit    = 2 << 20
	sidecarScreenshotLimit = 1 << 20
	sidecarStderrLimit     = 16 << 10
	sidecarCancelGrace     = 2 * time.Second
)

var (
	emailPattern            = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_\x60{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+`)
	jwtPattern              = regexp.MustCompile(`\b[A-Za-z0-9_-]{3,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{8,}\b`)
	proxyAuthPattern        = regexp.MustCompile(`(?i)((?:https?|socks5)://)[^/@\s]+@`)
	tokenValuePattern       = regexp.MustCompile(`(?i)((?:access[_-]?token|token)\s*[:=]\s*)[^\s,;]+`)
	bearerValuePattern      = regexp.MustCompile(`(?i)(bearer\s+)[^\s,;]+`)
	verificationCodePattern = regexp.MustCompile(`\b[0-9]{4,8}\b`)
)

type sidecarStartMessage struct {
	Version   int                 `json:"version"`
	Type      string              `json:"type"`
	RequestID string              `json:"request_id"`
	Payload   sidecarStartPayload `json:"payload"`
}

type sidecarStartPayload struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	FullName string `json:"full_name"`
	Age      string `json:"age"`
	Proxy    string `json:"proxy"`
	Headless bool   `json:"headless"`
}

type sidecarMessage struct {
	Version     int    `json:"version"`
	Type        string `json:"type"`
	RequestID   string `json:"request_id"`
	Message     string `json:"message,omitempty"`
	Code        string `json:"code,omitempty"`
	Data        string `json:"data,omitempty"`
	AccessToken string `json:"access_token,omitempty"`
}

type limitedBuffer struct {
	buf       bytes.Buffer
	remaining int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.remaining > 0 {
		keep := min(len(p), b.remaining)
		_, _ = b.buf.Write(p[:keep])
		b.remaining -= keep
	}
	return n, nil
}

type sidecarWriter struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

func (w *sidecarWriter) send(message any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.encoder.Encode(message)
}

type sidecarProcess struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	writer     *sidecarWriter
	stderr     *limitedBuffer
	stderrDone chan struct{}
	done       chan struct{}
	forceStop  context.CancelFunc
}

func startSidecarProcess(python, script string) (*sidecarProcess, error) {
	processCtx, forceStop := context.WithCancel(context.Background())
	cmd := exec.CommandContext(processCtx, python, script)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		forceStop()
		return nil, fmt.Errorf("创建 sidecar stdin 失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		forceStop()
		_ = stdin.Close()
		return nil, fmt.Errorf("创建 sidecar stdout 失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		forceStop()
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("创建 sidecar stderr 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		forceStop()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("启动 CloakBrowser sidecar 失败: %w", err)
	}
	process := &sidecarProcess{
		cmd: cmd, stdin: stdin, stdout: stdout,
		writer:     &sidecarWriter{encoder: json.NewEncoder(stdin)},
		stderr:     &limitedBuffer{remaining: sidecarStderrLimit},
		stderrDone: make(chan struct{}), done: make(chan struct{}), forceStop: forceStop,
	}
	go func() {
		_, _ = io.Copy(process.stderr, stderr)
		close(process.stderrDone)
	}()
	return process, nil
}

func (p *sidecarProcess) watchCancellation(ctx context.Context, requestID string) {
	select {
	case <-ctx.Done():
		_ = p.writer.send(sidecarMessage{Version: sidecarProtocolVersion, Type: "cancel", RequestID: requestID})
		timer := time.NewTimer(sidecarCancelGrace)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
			p.forceStop()
		}
	case <-p.done:
	}
}

func (p *sidecarProcess) wait(force bool) error {
	_ = p.stdin.Close()
	if force {
		p.forceStop()
	}
	err := p.cmd.Wait()
	close(p.done)
	<-p.stderrDone
	p.forceStop()
	return err
}

type sidecarSession struct {
	ctx       context.Context
	input     Input
	process   *sidecarProcess
	requestID string
	token     string
	code      string
}

func (s *sidecarSession) handle(message sidecarMessage) (bool, error) {
	switch message.Type {
	case "ready":
		return false, nil
	case "log":
		text := strings.ToValidUTF8(message.Message, "�")
		s.input.logf("%s", redactSensitive(text, s.input, s.code, s.token))
	case "code_request":
		code, err := s.input.FetchCode(s.ctx)
		if err != nil {
			return false, fmt.Errorf("读取验证码失败: %s", redactSensitive(err.Error(), s.input, s.code, s.token))
		}
		s.code = code
		err = s.process.writer.send(sidecarMessage{Version: sidecarProtocolVersion, Type: "code_response", RequestID: s.requestID, Code: code})
		if err != nil {
			return false, errors.New("发送 sidecar code_response 失败")
		}
	case "screenshot":
		png, err := decodeSidecarScreenshot(message.Data)
		if err != nil {
			return false, err
		}
		if s.input.SaveShot != nil {
			s.input.SaveShot(png)
		}
	case "result":
		s.token = strings.TrimSpace(message.AccessToken)
		if s.token == "" {
			return false, errors.New("sidecar result 缺少 access_token")
		}
		return true, nil
	case "error":
		return true, s.sidecarError(message)
	}
	return false, nil
}

func (s *sidecarSession) sidecarError(message sidecarMessage) error {
	text := redactSensitive(message.Message, s.input, s.code, s.token)
	if message.Code == "account_taken" {
		if text == "" {
			text = ErrAccountTaken.Error()
		}
		return fmt.Errorf("%w: %s", ErrAccountTaken, text)
	}
	if message.Code == "cancelled" && s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if text == "" {
		text = "CloakBrowser sidecar 返回错误"
	}
	if message.Code != "" {
		return fmt.Errorf("%s: %s", message.Code, text)
	}
	return errors.New(text)
}

func (s *sidecarSession) run() (bool, error) {
	scanner := bufio.NewScanner(s.process.stdout)
	scanner.Buffer(make([]byte, 64<<10), sidecarScannerLimit)
	for scanner.Scan() {
		var message sidecarMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return false, errors.New("sidecar 返回了畸形 JSONL 消息")
		}
		if err := validateSidecarMessage(message, s.requestID); err != nil {
			return false, err
		}
		terminal, err := s.handle(message)
		if terminal || err != nil {
			return terminal, err
		}
	}
	if scanner.Err() != nil {
		return false, errors.New("sidecar JSONL 输出超过 2MiB 或读取失败")
	}
	return false, errors.New("sidecar 在返回结果前退出")
}

func registerSidecar(ctx context.Context, in Input) (string, error) {
	python, script, err := resolveSidecarCommand(in)
	if err != nil {
		return "", err
	}
	requestID, err := newRequestID()
	if err != nil {
		return "", fmt.Errorf("生成 sidecar request_id 失败: %w", err)
	}
	process, err := startSidecarProcess(python, script)
	if err != nil {
		return "", err
	}
	go process.watchCancellation(ctx, requestID)
	start := sidecarStartMessage{
		Version: sidecarProtocolVersion, Type: "start", RequestID: requestID,
		Payload: sidecarStartPayload{Email: in.Email, Password: in.Password, FullName: in.FullName, Age: in.Age, Proxy: normalizeProxy(in.Proxy), Headless: in.Headless},
	}
	if err := process.writer.send(start); err != nil {
		_ = process.wait(true)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", sidecarFailure(fmt.Errorf("发送 sidecar start 消息失败: %w", err), process.stderr.buf.String(), in, "", "")
	}
	session := &sidecarSession{ctx: ctx, input: in, process: process, requestID: requestID}
	terminal, protocolErr := session.run()
	waitErr := process.wait(protocolErr != nil || !terminal)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if protocolErr != nil {
		if errors.Is(protocolErr, ErrAccountTaken) {
			return "", protocolErr
		}
		return "", sidecarFailure(protocolErr, process.stderr.buf.String(), in, session.code, session.token)
	}
	if waitErr != nil {
		return "", sidecarFailure(fmt.Errorf("sidecar 进程异常退出: %w", waitErr), process.stderr.buf.String(), in, session.code, session.token)
	}
	return session.token, nil
}

func validateSidecarMessage(message sidecarMessage, requestID string) error {
	if message.Version != sidecarProtocolVersion {
		return errors.New("sidecar 协议版本不匹配")
	}
	if message.RequestID != requestID {
		return errors.New("sidecar request_id 不匹配")
	}
	switch message.Type {
	case "ready", "log", "code_request", "screenshot", "result", "error":
		return nil
	default:
		return errors.New("sidecar 消息类型无效")
	}
}

func decodeSidecarScreenshot(encoded string) ([]byte, error) {
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
	png, err := io.ReadAll(io.LimitReader(decoder, sidecarScreenshotLimit+1))
	if err != nil {
		return nil, errors.New("sidecar screenshot base64 无效")
	}
	if len(png) > sidecarScreenshotLimit {
		return nil, errors.New("sidecar screenshot 超过 1MiB 限制")
	}
	if len(png) < 8 || !bytes.Equal(png[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return nil, errors.New("sidecar screenshot 不是 PNG")
	}
	return png, nil
}

func newRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func resolveSidecarCommand(in Input) (string, string, error) {
	roots := sidecarProjectRoots()
	script := strings.TrimSpace(in.SidecarScript)
	if script == "" {
		for _, root := range roots {
			candidate := filepath.Join(root, "internal", "codexreg", "cloakbrowser_sidecar.py")
			if fileExists(candidate) {
				script = candidate
				break
			}
		}
		if script == "" {
			return "", "", errors.New("未找到 CloakBrowser sidecar 脚本")
		}
	}
	python := strings.TrimSpace(in.PythonExecutable)
	if python == "" {
		for _, root := range roots {
			candidates := []string{filepath.Join(root, ".venv", "Scripts", "python.exe")}
			if runtime.GOOS != "windows" {
				candidates = append(candidates, filepath.Join(root, ".venv", "bin", "python"))
			}
			for _, candidate := range candidates {
				if fileExists(candidate) {
					python = candidate
					break
				}
			}
			if python != "" {
				break
			}
		}
		if python == "" {
			python = "python"
		}
	}
	return python, script, nil
}

func sidecarProjectRoots() []string {
	var roots []string
	addParents := func(path string) {
		for {
			if fileExists(filepath.Join(path, "go.mod")) {
				absolute, err := filepath.Abs(path)
				if err == nil && !containsPath(roots, absolute) {
					roots = append(roots, absolute)
				}
			}
			parent := filepath.Dir(path)
			if parent == path {
				return
			}
			path = parent
		}
	}
	if _, source, _, ok := runtime.Caller(0); ok {
		addParents(filepath.Dir(source))
	}
	if cwd, err := os.Getwd(); err == nil {
		addParents(cwd)
	}
	if executable, err := os.Executable(); err == nil {
		addParents(filepath.Dir(executable))
	}
	return roots
}

func containsPath(paths []string, candidate string) bool {
	for _, path := range paths {
		if path == candidate {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func sidecarFailure(err error, stderr string, in Input, code, token string) error {
	message := redactSensitive(strings.ToValidUTF8(err.Error(), "�"), in, code, token)
	stderr = strings.ToValidUTF8(stderr, "�")
	stderr = strings.TrimSpace(redactSensitive(stderr, in, code, token))
	if stderr != "" {
		message += "; sidecar stderr: " + stderr
	}
	return errors.New(message)
}

func redactSensitive(text string, in Input, code, token string) string {
	for _, secret := range []string{in.Email, in.Password, code, token, in.Proxy} {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	if proxyURL, err := url.Parse(normalizeProxy(in.Proxy)); err == nil && proxyURL.User != nil {
		if password, ok := proxyURL.User.Password(); ok && password != "" {
			text = strings.ReplaceAll(text, password, "[redacted]")
		}
	}
	text = emailPattern.ReplaceAllString(text, "[email]")
	text = jwtPattern.ReplaceAllString(text, "[token]")
	text = proxyAuthPattern.ReplaceAllString(text, `${1}[redacted]@`)
	text = tokenValuePattern.ReplaceAllString(text, `${1}[redacted]`)
	text = bearerValuePattern.ReplaceAllString(text, `${1}[redacted]`)
	text = verificationCodePattern.ReplaceAllString(text, "[code]")
	return text
}
