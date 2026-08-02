package producer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/codexoauth"
	"chatgpt-register/internal/integrationcfg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/smsactivate"
	"chatgpt-register/internal/sub2api"

	"gorm.io/gorm"
)

var integrationLocks [64]sync.Mutex

func (p *Producer) AuthorizeCodex(ctx context.Context, registrationID uint) (codexoauth.Tokens, error) {
	lock := &integrationLocks[int(registrationID)%len(integrationLocks)]
	lock.Lock()
	locked := true
	unlock := func() {
		if locked {
			locked = false
			lock.Unlock()
		}
	}
	defer unlock()
	var registration models.Registration
	if err := p.db.First(&registration, registrationID).Error; err != nil {
		unlock()
		return codexoauth.Tokens{}, err
	}
	if registration.Status != "registered" {
		unlock()
		return codexoauth.Tokens{}, fmt.Errorf("账号尚未注册成功")
	}
	if strings.TrimSpace(registration.Password) == "" {
		unlock()
		return codexoauth.Tokens{}, fmt.Errorf("账号缺少登录密码")
	}
	p.db.Model(&models.Registration{}).Where("id = ?", registration.ID).Updates(map[string]any{
		"codex_status": "authorizing", "codex_error": "",
	})
	p.appendIntegrationLog(registration.ID, "Codex OAuth 开始授权")

	var mailbox models.Mailbox
	if err := p.db.First(&mailbox, registration.MailboxID).Error; err != nil {
		p.setCodexFailed(registration.ID, err)
		unlock()
		return codexoauth.Tokens{}, fmt.Errorf("读取邮箱失败: %w", err)
	}
	mailAccount := mailfetch.Account{Email: mailbox.Email, ClientID: mailbox.ClientID, RefreshToken: mailbox.RefreshToken}
	existingMessages, err := p.mail.ListMessages(ctx, mailAccount, 30)
	if err != nil {
		safeErr := codexSafeError(fmt.Errorf("读取授权前邮件快照失败: %w", err), registration)
		p.setCodexFailed(registration.ID, safeErr)
		unlock()
		return codexoauth.Tokens{}, safeErr
	}
	ignoredMessageIDs := make(map[string]struct{}, len(existingMessages))
	for _, message := range existingMessages {
		ignoredMessageIDs[message.ID] = struct{}{}
	}
	authorizer := p.authorizeCodex
	if authorizer == nil {
		authorizer = codexoauth.Authorize
	}
	acquirePhone := p.acquirePhone
	if acquirePhone == nil {
		acquirePhone = p.acquireSMSPhone
	}
	since := time.Now()
	tokens, err := authorizer(ctx, codexoauth.Input{
		Email: registration.Email, Password: registration.Password, Proxy: registration.Proxy,
		Headless: p.getSetting("headless") != "0",
		FetchEmailCode: func(fetchCtx context.Context) (string, error) {
			return p.fetchCodeAfter(fetchCtx, mailbox, since, ignoredMessageIDs)
		},
		AcquirePhone: func(phoneCtx context.Context) (*codexoauth.PhoneSession, error) {
			return acquirePhone(phoneCtx, registration.ID)
		},
		Log: func(format string, values ...any) {
			p.appendIntegrationLog(registration.ID, fmt.Sprintf(format, values...))
		},
		SaveShot: func(image []byte) {
			p.db.Model(&models.Registration{}).Where("id = ?", registration.ID).Update("shot", image)
		},
	})
	if err != nil {
		safeErr := codexSafeError(err, registration)
		p.setCodexFailed(registration.ID, safeErr)
		unlock()
		return codexoauth.Tokens{}, safeErr
	}
	if tokens.Email == "" {
		tokens.Email = registration.Email
	}
	if err := p.saveCodexTokens(registration, tokens); err != nil {
		p.setCodexFailed(registration.ID, err)
		unlock()
		return codexoauth.Tokens{}, err
	}
	p.appendIntegrationLog(registration.ID, "Codex OAuth 授权成功")
	unlock()

	values, configErr := integrationcfg.Load(p.db)
	if configErr == nil && values.Sub2APIAutoImport() {
		if _, importErr := p.ImportSub2API(ctx, registration.ID); importErr != nil {
			return tokens, fmt.Errorf("Codex OAuth 已成功，Sub2API 自动导入失败: %w", importErr)
		}
	}
	return tokens, nil
}

func (p *Producer) acquireSMSPhone(ctx context.Context, registrationID uint) (*codexoauth.PhoneSession, error) {
	values, err := integrationcfg.Load(p.db)
	if err != nil {
		return nil, err
	}
	config, err := values.SMS()
	if err != nil {
		return nil, err
	}
	newClient := p.newSMSClient
	if newClient == nil {
		newClient = func(clientConfig smsactivate.Config) (*smsactivate.Client, error) {
			return smsactivate.New(clientConfig)
		}
	}
	client, err := newClient(config.Client)
	if err != nil {
		return nil, err
	}
	activation, err := client.Allocate(ctx)
	if err != nil {
		return nil, err
	}
	row := models.SMSActivation{
		RegistrationID: registrationID, Provider: string(config.Client.Platform),
		ActivationID: activation.ActivationID, PhoneNumber: activation.E164,
		CountryID: activation.Country, Service: smsactivate.ServiceOpenAI, Status: "allocated",
	}
	if err := p.db.Create(&row).Error; err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, _ = activation.Close(closeCtx, false)
		cancel()
		return nil, err
	}
	p.appendIntegrationLog(registrationID, fmt.Sprintf("按需购买接码号码 %s（%s #%s）", maskPhone(activation.E164), config.Client.Platform, activation.ActivationID))
	var finishMu sync.Mutex
	finishChosen := false
	finishSuccess := false
	return &codexoauth.PhoneSession{
		Number: activation.E164,
		WaitCode: func(waitCtx context.Context) (string, error) {
			p.db.Model(&models.SMSActivation{}).Where("id = ?", row.ID).Update("status", "waiting")
			pollCtx, cancel := context.WithTimeout(waitCtx, config.PollTimeout)
			defer cancel()
			code, pollErr := activation.PollCode(pollCtx, config.PollInterval)
			if pollErr != nil {
				p.db.Model(&models.SMSActivation{}).Where("id = ?", row.ID).Updates(map[string]any{
					"status": "failed", "error": truncateStr(pollErr.Error(), 500),
				})
				return "", pollErr
			}
			p.db.Model(&models.SMSActivation{}).Where("id = ?", row.ID).Update("status", "code_received")
			return code, nil
		},
		Finish: func(closeCtx context.Context, success bool) error {
			finishMu.Lock()
			defer finishMu.Unlock()
			if !finishChosen {
				finishChosen = true
				finishSuccess = success
			}
			_, closeErr := activation.Close(closeCtx, finishSuccess)
			now := time.Now()
			status := "cancelled"
			if finishSuccess {
				status = "completed"
			}
			updates := map[string]any{"status": status, "closed_at": now, "error": ""}
			if closeErr != nil {
				updates["status"] = "close_failed"
				updates["error"] = truncateStr(closeErr.Error(), 500)
			}
			if dbErr := p.db.Model(&models.SMSActivation{}).Where("id = ?", row.ID).Updates(updates).Error; dbErr != nil && closeErr == nil {
				return dbErr
			}
			return closeErr
		},
	}, nil
}

func (p *Producer) saveCodexTokens(registration models.Registration, tokens codexoauth.Tokens) error {
	var authData map[string]any
	if err := json.Unmarshal([]byte(registration.AuthData), &authData); err != nil || authData == nil {
		authData = make(map[string]any)
	}
	authData["auth_mode"] = "oauth"
	authData["email"] = tokens.Email
	authData["access_token"] = tokens.AccessToken
	authData["refresh_token"] = tokens.RefreshToken
	authData["id_token"] = tokens.IDToken
	authData["expires_in"] = tokens.ExpiresIn
	authData["expires_at"] = time.Now().UTC().Add(time.Duration(tokens.ExpiresIn) * time.Second).Format(time.RFC3339)
	if tokens.ChatGPTAccountID != "" {
		authData["account_id"] = tokens.ChatGPTAccountID
	}
	if tokens.ChatGPTUserID != "" {
		authData["chatgpt_user_id"] = tokens.ChatGPTUserID
	}
	if tokens.PlanType != "" {
		authData["plan_type"] = tokens.PlanType
	}
	encoded, err := json.MarshalIndent(authData, "", "  ")
	if err != nil {
		return err
	}
	now := time.Now()
	updates := map[string]any{
		"auth_data": string(encoded), "codex_status": "authorized", "codex_error": "", "codex_authorized_at": now,
	}
	if tokens.ChatGPTAccountID != "" {
		updates["account_id"] = tokens.ChatGPTAccountID
	}
	if tokens.ChatGPTUserID != "" {
		updates["user_id"] = tokens.ChatGPTUserID
	}
	if tokens.PlanType != "" {
		updates["plan_type"] = tokens.PlanType
	}
	return p.db.Model(&models.Registration{}).Where("id = ?", registration.ID).Updates(updates).Error
}

func codexSafeError(cause error, registration models.Registration) error {
	message := cause.Error()
	for _, secret := range []string{registration.Email, registration.Password, registration.Proxy} {
		if strings.TrimSpace(secret) != "" {
			message = strings.ReplaceAll(message, secret, "<redacted>")
		}
	}
	return fmt.Errorf("%s", truncateStr(message, 500))
}

func (p *Producer) setCodexFailed(registrationID uint, cause error) {
	message := truncateStr(cause.Error(), 500)
	p.db.Model(&models.Registration{}).Where("id = ?", registrationID).Updates(map[string]any{
		"codex_status": "failed", "codex_error": message,
	})
	p.appendIntegrationLog(registrationID, "Codex OAuth 授权失败: "+message)
}

func maskPhone(phone string) string {
	phone = strings.TrimSpace(phone)
	if len(phone) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(phone)-4) + phone[len(phone)-4:]
}

func (p *Producer) ImportSub2API(ctx context.Context, registrationID uint) (sub2api.ImportResult, error) {
	lock := &integrationLocks[int(registrationID)%len(integrationLocks)]
	lock.Lock()
	defer lock.Unlock()

	var registration models.Registration
	if err := p.db.First(&registration, registrationID).Error; err != nil {
		return sub2api.ImportResult{}, err
	}
	if registration.Status != "registered" {
		return sub2api.ImportResult{}, fmt.Errorf("账号尚未注册成功")
	}
	source, err := oauthSourceFromRegistration(registration)
	if err != nil {
		p.setSub2APIFailed(registration.ID, err, source)
		return sub2api.ImportResult{}, err
	}
	p.db.Model(&models.Registration{}).Where("id = ?", registration.ID).Updates(map[string]any{
		"sub2api_status": "importing", "sub2api_error": "",
	})
	p.appendIntegrationLog(registration.ID, "Sub2API 开始导入")

	values, err := integrationcfg.Load(p.db)
	if err != nil {
		p.setSub2APIFailed(registration.ID, err, source)
		return sub2api.ImportResult{}, err
	}
	config, err := values.Sub2API()
	if err != nil {
		p.setSub2APIFailed(registration.ID, err, source)
		return sub2api.ImportResult{}, err
	}
	result, err := sub2api.ImportOpenAIOAuthAccount(ctx, source, config)
	if err != nil {
		p.setSub2APIFailed(registration.ID, err, source)
		return sub2api.ImportResult{}, err
	}
	now := time.Now()
	accountID := int64(result.AccountID)
	if err := p.db.Model(&models.Registration{}).Where("id = ?", registration.ID).Updates(map[string]any{
		"sub2api_status": "imported", "sub2api_account_id": accountID,
		"sub2api_error": "", "sub2api_imported_at": now,
	}).Error; err != nil {
		return sub2api.ImportResult{}, err
	}
	p.appendIntegrationLog(registration.ID, fmt.Sprintf("Sub2API %s成功，远端账号 #%d", sub2APIActionLabel(result.Action), result.AccountID))
	return result, nil
}

func oauthSourceFromRegistration(registration models.Registration) (sub2api.OAuthSource, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(registration.AuthData), &root); err != nil {
		return sub2api.OAuthSource{}, fmt.Errorf("本地 OAuth 数据格式错误")
	}
	sourceMap := root
	if credentials, ok := root["credentials"].(map[string]any); ok {
		sourceMap = credentials
	}
	value := func(keys ...string) string {
		for _, key := range keys {
			if text, ok := sourceMap[key].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
			if text, ok := root[key].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		return ""
	}
	source := sub2api.OAuthSource{
		Email:            value("email"),
		AccessToken:      value("access_token"),
		RefreshToken:     value("refresh_token"),
		IDToken:          value("id_token"),
		ExpiresIn:        intValue(sourceMap["expires_in"]),
		ChatGPTAccountID: value("account_id", "chatgpt_account_id"),
		ChatGPTUserID:    value("chatgpt_user_id", "user_id"),
		PlanType:         value("plan_type"),
	}
	if source.Email == "" {
		source.Email = registration.Email
	}
	if source.ChatGPTAccountID == "" {
		source.ChatGPTAccountID = registration.AccountID
	}
	if source.ChatGPTUserID == "" {
		source.ChatGPTUserID = registration.UserID
	}
	if source.PlanType == "" {
		source.PlanType = registration.PlanType
	}
	if source.AccessToken == "" || source.RefreshToken == "" {
		return source, fmt.Errorf("账号缺少完整 Codex OAuth 凭据")
	}
	return source, nil
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(typed))
		return parsed
	default:
		return 0
	}
}

func (p *Producer) setSub2APIFailed(registrationID uint, cause error, source sub2api.OAuthSource) {
	message := sub2api.Redact(cause, source.Email, source.AccessToken, source.RefreshToken, source.IDToken)
	p.db.Model(&models.Registration{}).Where("id = ?", registrationID).Updates(map[string]any{
		"sub2api_status": "failed", "sub2api_error": truncateStr(message, 500),
	})
	p.appendIntegrationLog(registrationID, "Sub2API 导入失败: "+message)
}

func (p *Producer) appendIntegrationLog(registrationID uint, message string) {
	entry := time.Now().Format("2006-01-02 15:04:05") + " " + message
	p.db.Model(&models.Registration{}).Where("id = ?", registrationID).Update("log",
		gorm.Expr("CASE WHEN log IS NULL OR log = '' THEN ? ELSE log || char(10) || ? END", entry, entry))
}

func sub2APIActionLabel(action string) string {
	if action == "updated" {
		return "更新"
	}
	return "导入"
}
