package codexoauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)

const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"

func driveBrowser(ctx context.Context, in Input, flow Flow, broker *CallbackBroker) (err error) {
	launcherInstance := launcher.New().Context(ctx).
		Headless(in.Headless).
		NoSandbox(true).
		Set("disable-dev-shm-usage").
		Append("--disable-blink-features", "AutomationControlled").
		Append("--disable-infobars", "").
		Append("--no-first-run", "").
		Append("--no-default-browser-check", "").
		Append("--window-size", "1280,800")

	proxyURL := normalizeProxy(in.Proxy)
	var proxyUser, proxyPassword string
	if proxyURL != "" {
		parsedProxy, parseErr := urlParts(proxyURL)
		if parseErr != nil {
			return parseErr
		}
		launcherInstance = launcherInstance.Set("proxy-server", parsedProxy.server).
			Set("proxy-bypass-list", "localhost;127.0.0.1")
		proxyUser, proxyPassword = parsedProxy.user, parsedProxy.password
	}

	controlURL, err := launcherInstance.Launch()
	if err != nil {
		return fmt.Errorf("启动 Codex OAuth 浏览器失败: %w", err)
	}
	browser := rod.New().Context(ctx).ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		return fmt.Errorf("连接 Codex OAuth 浏览器失败: %w", err)
	}
	defer browser.Close()
	if proxyUser != "" || proxyPassword != "" {
		go func() {
			defer func() { _ = recover() }()
			wait := browser.HandleAuth(proxyUser, proxyPassword)
			_ = wait()
		}()
	}

	page, err := stealth.Page(browser)
	if err != nil {
		return fmt.Errorf("创建 Codex OAuth 页面失败: %w", err)
	}
	page = page.Context(ctx)
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("Codex OAuth 浏览器异常: %v", recovered)
		}
		if err != nil && in.SaveShot != nil {
			shotPage := page.Context(context.Background()).Timeout(15 * time.Second)
			if image, shotErr := shotPage.Screenshot(false, nil); shotErr == nil && len(image) > 0 {
				in.SaveShot(image)
			}
		}
	}()
	if err := page.SetUserAgent(&proto.NetworkSetUserAgentOverride{
		UserAgent: browserUserAgent, AcceptLanguage: "en-US,en;q=0.9", Platform: "Win32",
	}); err != nil {
		return fmt.Errorf("设置 Codex OAuth 浏览器标识失败: %w", err)
	}
	if err := page.Navigate(flow.AuthorizationURL(in.Email)); err != nil {
		return fmt.Errorf("打开 Codex OAuth 授权页失败: %w", err)
	}

	var phone *PhoneSession
	var emailSubmitted, passwordSubmitted, emailOTPSubmitted, phoneSubmitted, phoneOTPSubmitted bool
	phoneFinished := false
	personalChoicePage := ""
	defer func() {
		if phone == nil || phoneFinished || phone.Finish == nil {
			return
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = phone.Finish(closeCtx, phoneOTPSubmitted && errors.Is(context.Cause(ctx), errOAuthCallback))
	}()

	deadline := time.NewTimer(6 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	lastURL := ""
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("Codex OAuth 浏览器流程超时")
		case <-ticker.C:
		}

		info, infoErr := page.Info()
		if infoErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		currentURL := info.URL
		if currentURL != lastURL {
			in.logf("Codex OAuth 页面: %s", safePageLabel(currentURL))
			lastURL = currentURL
		}
		body := visibleBodyText(page)
		emailInput := visibleElement(page, "input[type='email'],input[name='username'],input[name='email'],#email")
		passwordInput := visibleElement(page, "input[type='password'],input[name='password']")
		phoneInput := visibleElement(page, "input[type='tel'],input[name='phone'],input[name='phone_number'],input[autocomplete='tel'],input[autocomplete='tel-national']")
		otpInput := visibleElement(page, "input[name='code'],input[autocomplete='one-time-code'],input[inputmode='numeric']")
		actionButton := visibleAction(page)
		state := classifyPage(pageSignals{
			URL: currentURL, Body: body, HasEmail: emailInput != nil, HasPassword: passwordInput != nil,
			HasPhone: phoneInput != nil, HasOTP: otpInput != nil, HasAction: actionButton != nil, PhoneAllocated: phone != nil,
		})
		if phoneOTPSubmitted && !phoneFinished && (state == stateConsent || state == stateCallback) {
			if phone.Finish != nil {
				closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				finishErr := phone.Finish(closeCtx, true)
				cancel()
				if finishErr != nil {
					return fmt.Errorf("完成 Codex OAuth 接码订单失败: %w", finishErr)
				}
			}
			phoneFinished = true
		}

		switch state {
		case stateCallback:
			broker.DispatchURL(currentURL)
			return nil
		case stateEmail:
			if emailSubmitted {
				continue
			}
			if err := replaceInput(emailInput, in.Email); err != nil {
				return fmt.Errorf("填写 Codex OAuth 邮箱失败: %w", err)
			}
			if err := clickAction(page, actionButton); err != nil {
				return fmt.Errorf("提交 Codex OAuth 邮箱失败: %w", err)
			}
			emailSubmitted = true
		case statePassword:
			if passwordSubmitted {
				continue
			}
			if err := replaceInput(passwordInput, in.Password); err != nil {
				return fmt.Errorf("填写 Codex OAuth 密码失败: %w", err)
			}
			if err := clickAction(page, actionButton); err != nil {
				return fmt.Errorf("提交 Codex OAuth 密码失败: %w", err)
			}
			passwordSubmitted = true
		case stateEmailOTP:
			if emailOTPSubmitted {
				continue
			}
			if in.FetchEmailCode == nil {
				return fmt.Errorf("Codex OAuth 需要邮件验证码")
			}
			code, fetchErr := in.FetchEmailCode(ctx)
			if fetchErr != nil {
				return fmt.Errorf("获取 Codex OAuth 邮件验证码失败: %w", fetchErr)
			}
			if err := inputOTP(page, otpInput, code); err != nil {
				return fmt.Errorf("填写 Codex OAuth 邮件验证码失败: %w", err)
			}
			if actionButton != nil {
				if err := clickAction(page, actionButton); err != nil {
					return fmt.Errorf("提交 Codex OAuth 邮件验证码失败: %w", err)
				}
			}
			emailOTPSubmitted = true
		case statePhone:
			if phoneSubmitted {
				continue
			}
			if in.AcquirePhone == nil {
				return fmt.Errorf("Codex OAuth 需要新增手机号，请先配置动态接码")
			}
			phone, err = in.AcquirePhone(ctx)
			if err != nil {
				return fmt.Errorf("购买 Codex OAuth 接码号码失败: %w", err)
			}
			if phone == nil || strings.TrimSpace(phone.Number) == "" || phone.WaitCode == nil {
				return fmt.Errorf("动态接码返回的号码会话无效")
			}
			if err := replaceInput(phoneInput, phone.Number); err != nil {
				return fmt.Errorf("填写 Codex OAuth 手机号失败: %w", err)
			}
			if err := clickAction(page, actionButton); err != nil {
				return fmt.Errorf("提交 Codex OAuth 手机号失败: %w", err)
			}
			phoneSubmitted = true
		case statePhoneOTP:
			if phoneOTPSubmitted {
				if containsAny(strings.ToLower(body), "invalid code", "incorrect code", "验证码错误", "验证码无效") {
					return fmt.Errorf("Codex OAuth 短信验证码校验失败")
				}
				continue
			}
			if phone == nil || phone.WaitCode == nil {
				return fmt.Errorf("账号要求已绑定手机号验证码，本次没有新增号码会话")
			}
			code, fetchErr := phone.WaitCode(ctx)
			if fetchErr != nil {
				return fmt.Errorf("获取 Codex OAuth 短信验证码失败: %w", fetchErr)
			}
			if err := inputOTP(page, otpInput, code); err != nil {
				return fmt.Errorf("填写 Codex OAuth 短信验证码失败: %w", err)
			}
			if actionButton != nil {
				if err := clickAction(page, actionButton); err != nil {
					return fmt.Errorf("提交 Codex OAuth 短信验证码失败: %w", err)
				}
			}
			phoneOTPSubmitted = true
		case stateConsent:
			if personalChoicePage != currentURL && containsAny(strings.ToLower(currentURL+" "+body), "organization", "workspace", "工作区") {
				if personal := visiblePersonalChoice(page); personal != nil {
					if err := personal.Click(proto.InputMouseButtonLeft, 1); err != nil {
						return fmt.Errorf("选择 Codex 个人工作区失败: %w", err)
					}
					personalChoicePage = currentURL
					continue
				}
			}
			if actionButton == nil {
				continue
			}
			if err := actionButton.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return fmt.Errorf("确认 Codex OAuth 授权失败: %w", err)
			}
		case stateWait:
			if fatalPage(body) {
				return fmt.Errorf("Codex OAuth 页面返回错误")
			}
		}
	}
}

type parsedProxy struct {
	server   string
	user     string
	password string
}

func urlParts(raw string) (parsedProxy, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return parsedProxy{}, fmt.Errorf("代理格式错误")
	}
	result := parsedProxy{server: parsed.Scheme + "://" + parsed.Host}
	if parsed.User != nil {
		result.user = parsed.User.Username()
		result.password, _ = parsed.User.Password()
	}
	return result, nil
}

func visibleElement(page *rod.Page, selector string) *rod.Element {
	elements, err := page.Elements(selector)
	if err != nil {
		return nil
	}
	for _, element := range elements {
		visible, visibleErr := element.Visible()
		if visibleErr == nil && visible {
			return element
		}
	}
	return nil
}

func visibleBodyText(page *rod.Page) string {
	body := visibleElement(page, "body")
	if body == nil {
		return ""
	}
	text, _ := body.Text()
	if len(text) > 4000 {
		text = text[:4000]
	}
	return text
}

func visiblePersonalChoice(page *rod.Page) *rod.Element {
	elements, err := page.Elements("button,label,[role='button']")
	if err != nil {
		return nil
	}
	for _, element := range elements {
		visible, visibleErr := element.Visible()
		if visibleErr != nil || !visible {
			continue
		}
		text, textErr := element.Text()
		if textErr == nil && containsAny(strings.ToLower(strings.TrimSpace(text)), "personal", "个人") {
			return element
		}
	}
	return nil
}

func visibleAction(page *rod.Page) *rod.Element {
	buttons, err := page.Elements("button,input[type='submit']")
	if err != nil {
		return nil
	}
	var fallback *rod.Element
	for _, button := range buttons {
		visible, visibleErr := button.Visible()
		if visibleErr != nil || !visible {
			continue
		}
		disabled, disabledErr := button.Disabled()
		if disabledErr == nil && disabled {
			continue
		}
		if fallback == nil {
			fallback = button
		}
		text, _ := button.Text()
		text = strings.ToLower(strings.TrimSpace(text))
		if containsAny(text, "continue", "next", "sign in", "log in", "verify", "send code", "authorize", "allow", "accept", "confirm", "继续", "下一步", "登录", "验证", "发送", "授权", "允许", "同意", "确认") {
			return button
		}
	}
	return fallback
}

func replaceInput(element *rod.Element, value string) error {
	if element == nil {
		return fmt.Errorf("输入框不存在")
	}
	if err := element.SelectAllText(); err != nil {
		return err
	}
	return element.Input(strings.TrimSpace(value))
}

func inputOTP(page *rod.Page, element *rod.Element, code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("验证码为空")
	}
	inputs, err := page.Elements("input[name='code'],input[autocomplete='one-time-code'],input[inputmode='numeric']")
	if err == nil {
		visible := make([]*rod.Element, 0, len(inputs))
		for _, input := range inputs {
			ok, visibleErr := input.Visible()
			if visibleErr == nil && ok {
				visible = append(visible, input)
			}
		}
		if len(visible) > 1 && len(visible) >= len(code) {
			for index, character := range code {
				if err := visible[index].Input(string(character)); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return replaceInput(element, code)
}

func clickAction(page *rod.Page, action *rod.Element) error {
	if action == nil {
		action = visibleAction(page)
	}
	if action == nil {
		return fmt.Errorf("提交按钮不存在")
	}
	return action.Click(proto.InputMouseButtonLeft, 1)
}

func safePageLabel(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "未知页面"
	}
	return parsed.Host + parsed.Path
}

func fatalPage(body string) bool {
	value := strings.ToLower(body)
	return containsAny(value, "access denied", "account has been deactivated", "you do not have an account", "账号已停用")
}
