package codexreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)

// ErrAccountTaken 注册时提示"账号不存在或已被删除/停用"，视为该地址已被注册，不应重试。
var ErrAccountTaken = errors.New("账号不存在或已被删除/停用")

const registrationSuccessHold = 5 * time.Second

// registerBrowser 启动浏览器完成 ChatGPT 账号注册并返回 accessToken。
// in.Proxy 为空则直连；非空时 Chrome 走该代理，并按出口 IP 做 GeoIP 对齐。
func registerBrowser(ctx context.Context, in Input) (token string, err error) {
	in.logf("🚀 启动浏览器自动化注册流程...")

	// 1. 启动 Chrome，禁用自动化特征
	l := launcher.New().
		Headless(in.Headless).
		NoSandbox(true).
		Set("disable-dev-shm-usage").
		Append("--disable-blink-features", "AutomationControlled").
		Append("--disable-infobars", "").
		Append("--no-first-run", "").
		Append("--no-default-browser-check", "").
		Append("--window-size", "1280,800")

	// 1.1 挂代理（账号密码交给 HandleAuth）
	var proxyUser, proxyPass string
	if strings.TrimSpace(in.Proxy) != "" {
		server, user, pass, perr := parseProxy(in.Proxy)
		if perr != nil {
			return "", fmt.Errorf("解析代理失败: %w", perr)
		}
		l = l.Set("proxy-server", server)
		proxyUser, proxyPass = user, pass
		in.logf("🌐 使用代理: %s", server)
	}

	controlURL, err := l.Launch()
	if err != nil {
		return "", fmt.Errorf("启动 Chrome 失败: %w", err)
	}
	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		return "", fmt.Errorf("连接 Chrome 失败: %w", err)
	}
	defer browser.MustClose()

	// 失败现场截图：无论是返回错误还是 MustXxx panic，都在关浏览器前把当前页面截图交给 SaveShot。
	var page *rod.Page
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("注册流程异常: %v", r)
		}
		if err == nil || page == nil {
			return
		}
		if signals, inspectErr := inspectRegistrationPage(page); inspectErr == nil {
			in.logf("失败页面诊断: %s", registrationPageDiagnostic(signals))
		}
		if in.SaveShot == nil {
			return
		}
		func() {
			defer func() {
				if r2 := recover(); r2 != nil {
					in.logf("📸 截图失败(panic): %v", r2)
				}
			}()
			shotPage := page.CancelTimeout().Timeout(15 * time.Second)
			data, serr := shotPage.Screenshot(false, nil)
			if serr != nil {
				in.logf("📸 截图失败: %v", serr)
				return
			}
			if len(data) == 0 {
				in.logf("📸 截图失败: 空数据")
				return
			}
			in.SaveShot(data)
			in.logf("📸 已保存失败现场截图")
		}()
	}()

	// 1.2 代理需要账号密码认证时，后台处理 Chrome 弹出的认证请求。
	// 注意：必须用非 Must 版本并 recover——MustHandleAuth 在独立 goroutine 里 panic
	// 会绕过调用方的 recover 直接把整个进程带崩。
	if proxyUser != "" || proxyPass != "" {
		go func() {
			defer func() { _ = recover() }()
			wait := browser.HandleAuth(proxyUser, proxyPass)
			_ = wait()
		}()
	}

	// 2. GeoIP：先经代理出口用 HTTP 请求查询地理位置，以便创建页面时一次性注入一致指纹
	geo := lookupGeoIPViaRequest(in)
	acceptLang := "en-US,en;q=0.9"
	if geo != nil {
		_, acceptLang = localeForCountry(geo.CountryCode)
	}

	// 2.1 stealth 隐身插件 + 真实 User-Agent（创建即注入与地理位置一致的指纹）
	page = stealth.MustPage(browser)
	page.MustSetUserAgent(&proto.NetworkSetUserAgentOverride{
		UserAgent:      userAgent,
		AcceptLanguage: acceptLang,
		Platform:       "Win32",
	})

	// 2.3 对齐时区/坐标/locale（UA/语言已在上面按地理信息注入）
	if geo != nil {
		applyGeo(page, geo, in)
	}

	page = page.Timeout(120 * time.Second)

	// 3. 打开 ChatGPT 注册页
	in.logf("🌐 正在打开 ChatGPT 注册页...")
	page.MustNavigate("https://chatgpt.com/auth/login")
	page.MustWaitLoad()
	page.MustElement("#email").MustWaitVisible()
	in.logf("✅ 注册页已加载")

	// 4. 输入邮箱并提交（用 JS 点击，避免元素被遮挡/未进入可点击态时 MustClick 失败）
	if err := submitRegistrationEmail(page, in.Email); err != nil {
		return "", fmt.Errorf("提交注册邮箱失败: %w", err)
	}
	in.logf("📧 已提交邮箱，等待下一步...")

	// 4.1 提交邮箱后可能出现"Create a password"创建密码页（在验证码之前）。
	// 用状态机识别：密码页则填入密码并 Continue；否则直接进入验证码环节。
	codeReady := false
	emailAttempts := 1
	emailSubmittedAt := time.Now()
	passwordDone := false
	passwordSubmittedAt := time.Time{}
	initialDeadline := time.Now().Add(2 * time.Minute)
	lastInitialDiagnostic := time.Time{}
	for time.Now().Before(initialDeadline) && !codeReady {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		signals, inspectErr := inspectRegistrationPage(page)
		if inspectErr != nil {
			return "", fmt.Errorf("读取验证码前页面状态失败: %w", inspectErr)
		}
		switch classifyRegistrationPage(signals) {
		case registrationStateCode, registrationStateCodeRejected:
			codeReady = true
		case registrationStateEmail:
			if time.Since(emailSubmittedAt) >= 3*time.Second {
				if emailAttempts >= 3 {
					if time.Since(emailSubmittedAt) >= 30*time.Second {
						return "", fmt.Errorf("邮箱连续提交 %d 次后页面未跳转: %s", emailAttempts, registrationPageDiagnostic(signals))
					}
					break
				}
				if submitErr := submitRegistrationEmail(page, in.Email); submitErr != nil {
					return "", fmt.Errorf("再次提交注册邮箱失败: %w", submitErr)
				}
				emailAttempts++
				emailSubmittedAt = time.Now()
				in.logf("📧 页面再次要求输入邮箱，已重新提交（%d/3）", emailAttempts)
			}
		case registrationStatePassword:
			if !passwordDone {
				in.logf("🔒 创建密码页已出现，自动设置密码")
				if submitErr := submitRegistrationPassword(page, in.Password); submitErr != nil {
					return "", fmt.Errorf("提交注册密码失败: %w", submitErr)
				}
				passwordDone = true
				passwordSubmittedAt = time.Now()
			} else if time.Since(passwordSubmittedAt) >= 30*time.Second {
				return "", fmt.Errorf("密码提交后页面未跳转: %s", registrationPageDiagnostic(signals))
			}
		case registrationStateDisabled:
			return "", ErrAccountTaken
		case registrationStateRetry:
			if retryErr := clickRegistrationRetry(page); retryErr != nil {
				return "", fmt.Errorf("重试注册页面失败: %w", retryErr)
			}
			in.logf("⚠ 注册页面返回临时错误，已点击重试")
		default:
			if lastInitialDiagnostic.IsZero() || time.Since(lastInitialDiagnostic) >= 15*time.Second {
				in.logf("等待验证码页面: %s", registrationPageDiagnostic(signals))
				lastInitialDiagnostic = time.Now()
			}
		}
		if !codeReady {
			if waitErr := waitRegistrationPoll(ctx); waitErr != nil {
				return "", waitErr
			}
		}
	}
	if !codeReady {
		return "", fmt.Errorf("等待验证码输入框超时")
	}
	in.logf("📨 验证码输入框已出现，正在从邮箱读取验证码...")

	// 5. 自动读取验证码（由 producer 通过邮箱轮询提供）
	code, err := in.FetchCode(ctx)
	if err != nil {
		return "", fmt.Errorf("获取邮箱验证码失败: %w", err)
	}
	// FetchCode 轮询邮件可能耗时较久，会耗尽之前设置的页面超时预算；
	// 提交验证码前刷新一次超时，避免后续操作报 context canceled。
	if err := submitRegistrationCode(page, code); err != nil {
		return "", fmt.Errorf("提交邮箱验证码失败: %w", err)
	}
	in.logf("🔑 已提交验证码")

	// 6. 提交验证码后的页面状态机：验证码错误重发 / 密码 / 资料 / 临时错误 / 主界面。
	ready := false
	profileDone := false
	profileSubmittedAt := time.Time{}
	codeAttempts := 1
	codeSubmittedAt := time.Now()
	postCodeDeadline := time.Now().Add(3 * time.Minute)
	lastPostCodeDiagnostic := time.Time{}
	for time.Now().Before(postCodeDeadline) && !ready {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		signals, inspectErr := inspectRegistrationPage(page)
		if inspectErr != nil {
			return "", fmt.Errorf("读取验证码后页面状态失败: %w", inspectErr)
		}
		switch classifyRegistrationPage(signals) {
		case registrationStateReady:
			ready = true
		case registrationStateDisabled:
			return "", ErrAccountTaken
		case registrationStateRetry:
			if retryErr := clickRegistrationRetry(page); retryErr != nil {
				return "", fmt.Errorf("重试注册页面失败: %w", retryErr)
			}
			in.logf("⚠ 注册页面返回临时错误，已点击重试")
		case registrationStatePassword:
			if !passwordDone {
				in.logf("🔒 验证码后出现创建密码页，自动设置密码")
				if submitErr := submitRegistrationPassword(page, in.Password); submitErr != nil {
					return "", fmt.Errorf("提交注册密码失败: %w", submitErr)
				}
				passwordDone = true
				passwordSubmittedAt = time.Now()
			} else if time.Since(passwordSubmittedAt) >= 30*time.Second {
				return "", fmt.Errorf("密码提交后页面未跳转: %s", registrationPageDiagnostic(signals))
			}
		case registrationStateProfile:
			if !profileDone {
				in.logf("📝 账户完善页面已出现")
				profileField, submitErr := submitRegistrationProfile(page, in.FullName, in.Age)
				if submitErr != nil {
					return "", fmt.Errorf("提交账户资料失败: %w", submitErr)
				}
				profileDone = true
				profileSubmittedAt = time.Now()
				in.logf("👤 已提交资料 (name/%s)", profileField)
			} else if time.Since(profileSubmittedAt) >= 30*time.Second {
				return "", fmt.Errorf("资料提交后页面未跳转: %s", registrationPageDiagnostic(signals))
			}
		case registrationStateCodeRejected:
			if codeAttempts >= 3 {
				return "", fmt.Errorf("邮箱验证码连续 %d 次未通过: %s", codeAttempts, registrationPageDiagnostic(signals))
			}
			clicked, resendErr := clickRegistrationResend(page)
			if resendErr != nil {
				return "", fmt.Errorf("请求重新发送邮箱验证码失败: %w", resendErr)
			}
			if clicked {
				in.logf("⚠ 邮箱验证码无效或已过期，已请求新验证码")
			} else {
				in.logf("⚠ 邮箱验证码无效或已过期，正在等待新验证码")
			}
			newCode, fetchErr := in.FetchCode(ctx)
			if fetchErr != nil {
				return "", fmt.Errorf("获取新邮箱验证码失败: %w", fetchErr)
			}
			if submitErr := submitRegistrationCode(page, newCode); submitErr != nil {
				return "", fmt.Errorf("提交新邮箱验证码失败: %w", submitErr)
			}
			codeAttempts++
			codeSubmittedAt = time.Now()
			postCodeDeadline = time.Now().Add(3 * time.Minute)
			in.logf("已提交新验证码（%d/3）", codeAttempts)
		case registrationStateCode:
			if time.Since(codeSubmittedAt) >= 30*time.Second && (lastPostCodeDiagnostic.IsZero() || time.Since(lastPostCodeDiagnostic) >= 15*time.Second) {
				in.logf("验证码提交后页面仍未跳转: %s", registrationPageDiagnostic(signals))
				lastPostCodeDiagnostic = time.Now()
			}
		default:
			if lastPostCodeDiagnostic.IsZero() || time.Since(lastPostCodeDiagnostic) >= 15*time.Second {
				in.logf("等待注册完成: %s", registrationPageDiagnostic(signals))
				lastPostCodeDiagnostic = time.Now()
			}
		}
		if !ready {
			if waitErr := waitRegistrationPoll(ctx); waitErr != nil {
				return "", waitErr
			}
		}
	}
	if !ready {
		signals, _ := inspectRegistrationPage(page)
		return "", fmt.Errorf("等待 ChatGPT 主界面超时: %s", registrationPageDiagnostic(signals))
	}
	in.logf("✅ ChatGPT 主界面已就绪，提取 accessToken...")

	// 7. 导航到 /api/auth/session 读取 accessToken（重置超时，避免沿用已耗尽的预算）
	page = page.CancelTimeout().Timeout(60 * time.Second)
	page.MustNavigate("https://chatgpt.com/api/auth/session")
	page.MustWaitLoad()
	body := page.MustElement("body").MustText()

	var sessionData map[string]any
	if err := json.Unmarshal([]byte(body), &sessionData); err != nil {
		return "", fmt.Errorf("解析 session JSON 失败: %w", err)
	}
	accessToken, ok := sessionData["accessToken"].(string)
	if !ok || accessToken == "" {
		return "", fmt.Errorf("未找到 accessToken，可能未登录成功")
	}
	in.logf("🔑 accessToken 获取成功，浏览器将在 %d 秒后关闭", int(registrationSuccessHold/time.Second))
	waitRegistrationSuccessHold(ctx, registrationSuccessHold)
	return accessToken, nil
}

func waitRegistrationSuccessHold(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

type registrationPageState string

const (
	registrationStateWait         registrationPageState = "wait"
	registrationStateEmail        registrationPageState = "email"
	registrationStateCode         registrationPageState = "code"
	registrationStateCodeRejected registrationPageState = "code_rejected"
	registrationStatePassword     registrationPageState = "password"
	registrationStateProfile      registrationPageState = "profile"
	registrationStateReady        registrationPageState = "ready"
	registrationStateDisabled     registrationPageState = "disabled"
	registrationStateRetry        registrationPageState = "retry"
)

type registrationPageSignals struct {
	URL          string `json:"url"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	HasEmail     bool   `json:"hasEmail"`
	EmailValue   string `json:"emailValue"`
	HasCode      bool   `json:"hasCode"`
	CodeInvalid  bool   `json:"codeInvalid"`
	HasPassword  bool   `json:"hasPassword"`
	HasName      bool   `json:"hasName"`
	ProfileField string `json:"profileField"`
	HasReady     bool   `json:"hasReady"`
	HasRetry     bool   `json:"hasRetry"`
}

var (
	registrationEmailPattern  = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	registrationCodePattern   = regexp.MustCompile(`\b\d{6}\b`)
	registrationNumberPattern = regexp.MustCompile(`\+?\d[\d\s().-]{7,}\d`)
	registrationSpacePattern  = regexp.MustCompile(`\s+`)
	registrationDatePattern   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

func inspectRegistrationPage(page *rod.Page) (registrationPageSignals, error) {
	var signals registrationPageSignals
	if page == nil {
		return signals, fmt.Errorf("页面不存在")
	}
	result, err := page.CancelTimeout().Timeout(10 * time.Second).Eval(`() => {
		const visible = el => {
			if (!el) return false;
			const style = getComputedStyle(el);
			const rect = el.getBoundingClientRect();
			return style.display !== 'none' && style.visibility !== 'hidden' && rect.width > 0 && rect.height > 0;
		};
		const first = selector => Array.from(document.querySelectorAll(selector)).find(visible) || null;
		const email = first("#email,input[name='email'],input[type='email'],input[autocomplete='email']");
		const code = first("input[name='code'],input[autocomplete='one-time-code']");
		const password = first("input[type='password'],input[name='password']");
		const name = first("input[name='name']");
		const profile = first("input[name='age'],input[name='birthdate'],input[name='birthday'],input[type='date']");
		const actions = Array.from(document.querySelectorAll("button,a,[role='button']")).filter(visible).map(el => (el.innerText || el.textContent || '').trim()).join('\n');
		const alerts = Array.from(document.querySelectorAll("[role='alert'],[aria-live='assertive']")).filter(visible).map(el => (el.innerText || el.textContent || '').trim()).join('\n');
		return {
			url: location.href,
			title: document.title || '',
			body: (document.body?.innerText || '').slice(0, 2500),
			hasEmail: !!email,
			emailValue: email ? (email.value || '') : '',
			hasCode: !!code,
			codeInvalid: !!code && (code.getAttribute('aria-invalid') === 'true' || /invalid|incorrect|wrong|expired|错误|无效|过期|正しくありません|無効|有効期限/.test(alerts.toLowerCase())),
			hasPassword: !!password,
			hasName: !!name,
			profileField: profile ? (profile.getAttribute('name') || (profile.type === 'date' ? 'birthdate' : '')) : '',
			hasReady: !!first("textarea[name='prompt-textarea'],#prompt-textarea,[data-testid='composer'],[contenteditable='true'][data-lexical-editor='true']"),
			hasRetry: /try again|retry|重试|再試行|もう一度|다시 시도/i.test(actions)
		};
	}`)
	if err != nil {
		return signals, err
	}
	if err := result.Value.Unmarshal(&signals); err != nil {
		return signals, err
	}
	return signals, nil
}

func classifyRegistrationPage(signals registrationPageSignals) registrationPageState {
	body := strings.ToLower(signals.Body)
	if containsRegistrationText(body, "you do not have an account", "deleted or deactivated", "account has been deactivated", "账号不存在", "账号已被删除", "账号已停用") {
		return registrationStateDisabled
	}
	if signals.HasRetry {
		return registrationStateRetry
	}
	if signals.HasName && signals.ProfileField != "" {
		return registrationStateProfile
	}
	if signals.HasPassword {
		return registrationStatePassword
	}
	if signals.HasCode && (signals.CodeInvalid || registrationCodeRejected(body)) {
		return registrationStateCodeRejected
	}
	if signals.HasCode {
		return registrationStateCode
	}
	if signals.HasEmail {
		return registrationStateEmail
	}
	if signals.HasReady || registrationReadyURL(signals.URL, signals) {
		return registrationStateReady
	}
	return registrationStateWait
}

func registrationReadyURL(rawURL string, signals registrationPageSignals) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "chatgpt.com" && host != "chat.openai.com" && !strings.HasSuffix(host, ".chatgpt.com") {
		return false
	}
	if signals.HasEmail || signals.HasCode || signals.HasPassword || signals.HasName || strings.HasPrefix(strings.ToLower(parsed.Path), "/auth/") {
		return false
	}
	return parsed.Path == "" || parsed.Path == "/" || strings.HasPrefix(parsed.Path, "/c/") || strings.HasPrefix(parsed.Path, "/g/")
}

func registrationCodeRejected(body string) bool {
	return containsRegistrationText(body,
		"invalid code", "incorrect code", "wrong code", "code expired", "code has expired", "expired code",
		"验证码错误", "验证码无效", "验证码已过期", "验证码不正确",
		"コードが正しくありません", "コードが無効", "コードの有効期限", "認証コードが正しくありません", "認証コードが無効",
	)
}

func containsRegistrationText(value string, values ...string) bool {
	for _, candidate := range values {
		if strings.Contains(value, strings.ToLower(candidate)) {
			return true
		}
	}
	return false
}

func submitRegistrationEmail(page *rod.Page, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return fmt.Errorf("邮箱为空")
	}
	pg := page.CancelTimeout().Timeout(15 * time.Second)
	input, err := visibleRegistrationElement(pg, "#email,input[name='email'],input[type='email'],input[autocomplete='email']")
	if err != nil {
		return err
	}
	if err := replaceRegistrationInput(input, email); err != nil {
		return err
	}
	button, err := visibleRegistrationElement(pg, "button[type='submit']")
	if err != nil {
		return err
	}
	if _, err := button.Eval(`() => this.click()`); err != nil {
		signals, inspectErr := inspectRegistrationPage(page)
		if inspectErr == nil && !signals.HasEmail {
			return nil
		}
		return err
	}
	return nil
}

func submitRegistrationCode(page *rod.Page, code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("验证码为空")
	}
	pg := page.CancelTimeout().Timeout(15 * time.Second)
	input, err := visibleRegistrationElement(pg, "input[name='code'],input[autocomplete='one-time-code']")
	if err != nil {
		return err
	}
	if err := replaceRegistrationInput(input, code); err != nil {
		return err
	}
	button, buttonErr := visibleRegistrationElement(pg, "button[type='submit']")
	if buttonErr != nil {
		return nil
	}
	if _, err := button.Eval(`() => this.click()`); err != nil {
		signals, inspectErr := inspectRegistrationPage(page)
		if inspectErr == nil && !signals.HasCode {
			return nil
		}
		return err
	}
	return nil
}

func submitRegistrationPassword(page *rod.Page, password string) error {
	pg := page.CancelTimeout().Timeout(15 * time.Second)
	input, err := visibleRegistrationElement(pg, "input[type='password'],input[name='password']")
	if err != nil {
		return err
	}
	if err := replaceRegistrationInput(input, password); err != nil {
		return err
	}
	button, err := visibleRegistrationElement(pg, "button[type='submit']")
	if err != nil {
		return err
	}
	if _, err := button.Eval(`() => this.click()`); err != nil {
		signals, inspectErr := inspectRegistrationPage(page)
		if inspectErr == nil && !signals.HasPassword {
			return nil
		}
		return err
	}
	return nil
}

func submitRegistrationProfile(page *rod.Page, fullName, age string) (string, error) {
	pg := page.CancelTimeout().Timeout(15 * time.Second)
	nameInput, err := visibleRegistrationElement(pg, "input[name='name']")
	if err != nil {
		return "", err
	}
	if err := replaceRegistrationInput(nameInput, fullName); err != nil {
		return "", err
	}
	profileInput, err := visibleRegistrationElement(pg, "input[name='age'],input[name='birthdate'],input[name='birthday'],input[type='date']")
	if err != nil {
		return "", err
	}
	field := "age"
	if name, attrErr := profileInput.Attribute("name"); attrErr == nil && name != nil && strings.TrimSpace(*name) != "" {
		field = strings.ToLower(strings.TrimSpace(*name))
	} else if inputType, attrErr := profileInput.Attribute("type"); attrErr == nil && inputType != nil && strings.EqualFold(*inputType, "date") {
		field = "birthdate"
	}
	profileValue := strings.TrimSpace(age)
	if field == "birthdate" || field == "birthday" {
		profileValue = registrationBirthdate(age, time.Now())
	}
	if err := replaceRegistrationInput(profileInput, profileValue); err != nil {
		return "", err
	}
	button, err := visibleRegistrationElement(pg, "button[type='submit']")
	if err != nil {
		return "", err
	}
	if _, err := button.Eval(`() => this.click()`); err != nil {
		signals, inspectErr := inspectRegistrationPage(page)
		if inspectErr == nil && (!signals.HasName || signals.ProfileField == "") {
			return field, nil
		}
		return "", err
	}
	return field, nil
}

func registrationBirthdate(age string, now time.Time) string {
	age = strings.TrimSpace(age)
	if registrationDatePattern.MatchString(age) {
		return age
	}
	value, err := strconv.Atoi(age)
	if err != nil || value < 18 || value > 100 {
		value = 30
	}
	return now.AddDate(-value, 0, 0).Format("2006-01-02")
}

func replaceRegistrationInput(input *rod.Element, value string) error {
	if err := input.SelectAllText(); err != nil {
		return err
	}
	return input.Input(value)
}

func visibleRegistrationElement(page *rod.Page, selector string) (*rod.Element, error) {
	elements, err := page.Elements(selector)
	if err != nil {
		return nil, err
	}
	for _, element := range elements {
		visible, visibleErr := element.Visible()
		if visibleErr == nil && visible {
			return element, nil
		}
	}
	return nil, fmt.Errorf("未找到可见元素 %s", selector)
}

func clickRegistrationRetry(page *rod.Page) error {
	result, err := page.CancelTimeout().Timeout(10 * time.Second).Eval(`() => {
		const visible = el => {
			const style = getComputedStyle(el);
			const rect = el.getBoundingClientRect();
			return style.display !== 'none' && style.visibility !== 'hidden' && rect.width > 0 && rect.height > 0;
		};
		const action = Array.from(document.querySelectorAll("button,a,[role='button']")).find(el => visible(el) && /try again|retry|重试|再試行|もう一度|다시 시도/i.test((el.innerText || el.textContent || '').trim()));
		if (!action) return false;
		action.click();
		return true;
	}`)
	if err != nil {
		return err
	}
	if !result.Value.Bool() {
		return fmt.Errorf("重试入口已消失")
	}
	return nil
}

func clickRegistrationResend(page *rod.Page) (bool, error) {
	result, err := page.CancelTimeout().Timeout(10 * time.Second).Eval(`() => {
		const visible = el => {
			const style = getComputedStyle(el);
			const rect = el.getBoundingClientRect();
			return style.display !== 'none' && style.visibility !== 'hidden' && rect.width > 0 && rect.height > 0;
		};
		const action = Array.from(document.querySelectorAll("button,a,[role='button']")).find(el => visible(el) && /resend code|send again|request another code|resend email|重新发送|重发验证码|再次发送|コードを再送|再送信|다시 보내기/i.test((el.innerText || el.textContent || '').trim()));
		if (!action) return false;
		action.click();
		return true;
	}`)
	if err != nil {
		return false, err
	}
	return result.Value.Bool(), nil
}

func waitRegistrationPoll(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func registrationPageDiagnostic(signals registrationPageSignals) string {
	return fmt.Sprintf("state=%s url=%s title=%q body=%q",
		classifyRegistrationPage(signals), safeRegistrationURL(signals.URL), sanitizeRegistrationDiagnostic(signals.Title), sanitizeRegistrationDiagnostic(signals.Body))
}

func safeRegistrationURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return truncateRegistrationDiagnostic(sanitizeRegistrationDiagnostic(rawURL), 160)
	}
	return truncateRegistrationDiagnostic(parsed.Scheme+"://"+parsed.Host+parsed.EscapedPath(), 160)
}

func sanitizeRegistrationDiagnostic(value string) string {
	value = registrationEmailPattern.ReplaceAllString(value, "[email]")
	value = registrationCodePattern.ReplaceAllString(value, "[code]")
	value = registrationNumberPattern.ReplaceAllString(value, "[number]")
	value = registrationSpacePattern.ReplaceAllString(strings.TrimSpace(value), " ")
	return truncateRegistrationDiagnostic(value, 400)
}

func truncateRegistrationDiagnostic(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum]) + "…"
}
