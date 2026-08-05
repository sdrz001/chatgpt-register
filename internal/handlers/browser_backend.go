package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/browserboot"
	"chatgpt-register/internal/models"
)

const (
	browserBackendRod          = "rod"
	browserBackendCloakBrowser = "cloakbrowser"
	browserHealthCacheTTL      = 10 * time.Second
	cloakBrowserHealthTimeout  = 15 * time.Second
)

type browserBackendStatus struct {
	Backend string `json:"backend"`
	Ready   bool   `json:"ready"`
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

type cloakBrowserHealthMessage struct {
	Version   int    `json:"version"`
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	OK        bool   `json:"ok"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type cachedBrowserBackendStatus struct {
	status    browserBackendStatus
	checkedAt time.Time
}

var (
	checkCloakBrowser  = runCloakBrowserHealthCheck
	browserHealthNow   = time.Now
	rodBrowserSnapshot = func(manager *browserboot.Manager) browserboot.Status {
		return manager.Snapshot()
	}
	rodBrowserReady = func(manager *browserboot.Manager) bool {
		return manager.Ready()
	}
	browserHealthCache = struct {
		sync.Mutex
		entries map[string]cachedBrowserBackendStatus
	}{entries: make(map[string]cachedBrowserBackendStatus)}
)

func (h *Handler) checkBrowserBackend(ctx context.Context) browserBackendStatus {
	backend, pythonExecutable, err := h.loadBrowserBackendSettings()
	if err != nil {
		return browserBackendStatus{
			Backend: browserBackendRod,
			Phase:   "error",
			Message: "读取浏览器配置失败",
			Error:   err.Error(),
		}
	}

	switch backend {
	case browserBackendRod:
		return h.checkRodBrowser()
	case browserBackendCloakBrowser:
		cacheKey := backend + "\x00" + pythonExecutable
		now := browserHealthNow()
		browserHealthCache.Lock()
		cached, ok := browserHealthCache.entries[cacheKey]
		useCached := ok && now.Sub(cached.checkedAt) <= browserHealthCacheTTL
		browserHealthCache.Unlock()
		if useCached {
			return cached.status
		}

		status := checkCloakBrowser(ctx, pythonExecutable)
		status.Backend = browserBackendCloakBrowser
		browserHealthCache.Lock()
		browserHealthCache.entries[cacheKey] = cachedBrowserBackendStatus{status: status, checkedAt: browserHealthNow()}
		browserHealthCache.Unlock()
		return status
	default:
		return browserBackendStatus{
			Backend: backend,
			Phase:   "error",
			Message: "浏览器后端配置无效",
			Error:   fmt.Sprintf("不支持的浏览器后端 %q", backend),
		}
	}
}

func (h *Handler) loadBrowserBackendSettings() (string, string, error) {
	backend := browserBackendRod
	if h.DB == nil {
		return backend, "", nil
	}

	var settings []models.Setting
	if err := h.DB.Where("key IN ?", []string{"browser_backend", "python_executable"}).Find(&settings).Error; err != nil {
		return backend, "", err
	}
	pythonExecutable := ""
	for _, setting := range settings {
		switch setting.Key {
		case "browser_backend":
			if value := strings.ToLower(strings.TrimSpace(setting.Value)); value != "" {
				backend = value
			}
		case "python_executable":
			pythonExecutable = strings.TrimSpace(setting.Value)
		}
	}
	return backend, pythonExecutable, nil
}

func (h *Handler) checkRodBrowser() browserBackendStatus {
	if h.Browser == nil {
		return browserBackendStatus{
			Backend: browserBackendRod,
			Phase:   "error",
			Message: "Rod 浏览器尚未初始化",
			Error:   "缺少 Rod 浏览器管理器",
		}
	}
	snapshot := rodBrowserSnapshot(h.Browser)
	status := browserBackendStatus{
		Backend: browserBackendRod,
		Ready:   rodBrowserReady(h.Browser),
		Phase:   snapshot.Phase,
		Message: snapshot.Message,
		Error:   snapshot.Error,
	}
	if status.Phase == "" {
		if status.Ready {
			status.Phase = "ready"
		} else {
			status.Phase = "checking"
		}
	}
	if status.Message == "" {
		if status.Ready {
			status.Message = "Rod 浏览器已就绪"
		} else {
			status.Message = "Rod 浏览器尚未就绪"
		}
	}
	return status
}

func runCloakBrowserHealthCheck(ctx context.Context, pythonExecutable string) browserBackendStatus {
	status := browserBackendStatus{Backend: browserBackendCloakBrowser, Phase: "error"}
	python, script, err := resolveCloakBrowserHealthCommand(pythonExecutable)
	if err != nil {
		status.Message = "CloakBrowser 配置缺失"
		status.Error = err.Error()
		return status
	}

	healthCtx, cancel := context.WithTimeout(ctx, cloakBrowserHealthTimeout)
	defer cancel()
	cmd := exec.CommandContext(healthCtx, python, script, "--health")
	output, err := cmd.Output()
	if healthCtx.Err() != nil {
		status.Message = "CloakBrowser 健康检查超时"
		status.Error = healthCtx.Err().Error()
		return status
	}
	if err != nil && len(output) == 0 {
		status.Message = "Python 或 CloakBrowser 启动失败"
		status.Error = err.Error()
		return status
	}

	message, parseErr := parseCloakBrowserHealth(output)
	if parseErr != nil {
		status.Message = "CloakBrowser 健康检查响应无效"
		status.Error = parseErr.Error()
		return status
	}
	status.Ready = message.OK
	status.Message = strings.TrimSpace(message.Message)
	if status.Ready {
		status.Phase = "ready"
		if status.Message == "" {
			status.Message = "CloakBrowser 已就绪"
		}
		return status
	}
	if status.Message == "" {
		status.Message = "CloakBrowser 依赖缺失或不可用"
	}
	status.Error = strings.TrimSpace(message.Code)
	if status.Error == "" && err != nil {
		status.Error = err.Error()
	}
	return status
}

func parseCloakBrowserHealth(output []byte) (cloakBrowserHealthMessage, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var message cloakBrowserHealthMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			return cloakBrowserHealthMessage{}, fmt.Errorf("解析 JSONL 失败: %w", err)
		}
		if message.Version != 1 || message.Type != "health" || message.RequestID != "health" {
			return cloakBrowserHealthMessage{}, errors.New("预期 JSONL v1 health 消息")
		}
		return message, nil
	}
	if err := scanner.Err(); err != nil {
		return cloakBrowserHealthMessage{}, err
	}
	return cloakBrowserHealthMessage{}, errors.New("健康检查没有返回消息")
}

func resolveCloakBrowserHealthCommand(configuredPython string) (string, string, error) {
	roots := browserBackendProjectRoots()
	script := ""
	for _, root := range roots {
		candidate := filepath.Join(root, "internal", "codexreg", "cloakbrowser_sidecar.py")
		if browserBackendFileExists(candidate) {
			script = candidate
			break
		}
	}
	if script == "" {
		return "", "", errors.New("未找到 internal/codexreg/cloakbrowser_sidecar.py")
	}

	python := strings.TrimSpace(configuredPython)
	if python == "" {
		for _, root := range roots {
			candidates := []string{filepath.Join(root, ".venv", "Scripts", "python.exe")}
			if runtime.GOOS != "windows" {
				candidates = append(candidates, filepath.Join(root, ".venv", "bin", "python"))
			}
			for _, candidate := range candidates {
				if browserBackendFileExists(candidate) {
					python = candidate
					break
				}
			}
			if python != "" {
				break
			}
		}
	}
	if python == "" {
		return "", "", errors.New("未配置 python_executable，且未找到 .venv/Scripts/python.exe")
	}
	return python, script, nil
}

func browserBackendProjectRoots() []string {
	var roots []string
	addParents := func(path string) {
		for {
			if browserBackendFileExists(filepath.Join(path, "go.mod")) {
				absolute, err := filepath.Abs(path)
				if err == nil {
					duplicate := false
					for _, root := range roots {
						if root == absolute {
							duplicate = true
							break
						}
					}
					if !duplicate {
						roots = append(roots, absolute)
					}
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

func browserBackendFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
