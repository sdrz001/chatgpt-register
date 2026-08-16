package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"chatgpt-register/internal/emailalias"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

const (
	plusMailLimit        = 200
	plusMailRecentLimit  = 50
	plusMailSearchPhrase = "plus"
)

var (
	plusMailPhrases = []string{
		"successfully subscribed to chatgpt plus",
		"chatgpt plus receipt",
		"chatgpt plus subscription confirmation",
		"your chatgpt plus subscription is active",
		"your subscription to chatgpt plus",
		"chatgpt plus subscription",
		"chatgpt plus renewal",
		"chatgpt plus renewed",
		"welcome to chatgpt plus",
		"upgraded to chatgpt plus",
		"purchase of chatgpt plus",
		"payment for chatgpt plus",
		"receipt for chatgpt plus",
		"invoice for chatgpt plus",
		"你已成功订阅 chatgpt plus",
		"你已成功訂閱 chatgpt plus",
		"chatgpt plus 订阅成功",
		"chatgpt plus 訂閱成功",
		"chatgpt plus 开通成功",
		"chatgpt plus 開通成功",
		"chatgpt plus 续订成功",
		"chatgpt plus 續訂成功",
		"chatgpt plus 自动续费",
		"chatgpt plus 自動續費",
		"升级至 chatgpt plus",
		"升級至 chatgpt plus",
		"chatgpt plus 收据",
		"chatgpt plus 收據",
		"chatgpt plus 发票",
		"chatgpt plus 發票",
	}
	plusMailNegativePhrases = []string{
		"subscription canceled",
		"subscription cancelled",
		"payment failed",
		"payment declined",
		"payment unsuccessful",
		"couldn't process your payment",
		"could not process your payment",
		"subscription expired",
		"subscription paused",
		"refund issued",
		"订阅已取消",
		"訂閱已取消",
		"付款失败",
		"付款失敗",
		"支付失败",
		"支付失敗",
		"已退款",
		"续订失败",
		"續訂失敗",
		"扣款失败",
		"扣款失敗",
		"订阅已过期",
		"訂閱已過期",
	}
	plusMailEvidencePhrases = []string{
		"subscription", "subscribed", "membership", "receipt", "invoice", "renewal", "renewed",
		"purchase", "payment", "paid", "order", "billing", "charged", "amount", "total", "welcome",
		"订阅", "訂閱", "开通", "開通", "续订", "續訂", "续费", "續費", "收据", "收據", "发票", "發票",
		"购买", "購買", "付款", "支付", "扣款", "订单", "訂單", "金额", "金額", "合计", "合計", "套餐", "方案",
	}
	plusMailSenderPhrases = []string{"openai", "chatgpt", "stripe", "apple", "google play"}
)

type plusMailCheckItem struct {
	RegistrationID uint       `json:"registration_id"`
	Email          string     `json:"email"`
	Status         string     `json:"status"`
	Subject        string     `json:"subject,omitempty"`
	ReceivedAt     *time.Time `json:"received_at,omitempty"`
	Error          string     `json:"error,omitempty"`
}

func (h *Handler) RegistrationPlusMailCheck(c *gin.Context) {
	var in struct {
		IDs []uint `json:"ids"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(in.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择账号"})
		return
	}
	if h.Mail == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "邮件服务未初始化"})
		return
	}

	var registrations []models.Registration
	if err := h.DB.Select("id", "email", "mailbox_id", "plan_type", "auth_data").Where("id IN ?", in.IDs).Find(&registrations).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	registrationByID := make(map[uint]models.Registration, len(registrations))
	for _, registration := range registrations {
		registrationByID[registration.ID] = registration
	}

	items := make([]plusMailCheckItem, 0, len(in.IDs))
	found, notFound, failed := 0, 0, 0
	for _, id := range in.IDs {
		registration, exists := registrationByID[id]
		if !exists {
			items = append(items, plusMailCheckItem{RegistrationID: id, Status: "error", Error: "账号不存在"})
			failed++
			continue
		}
		item := h.checkRegistrationPlusMail(c.Request.Context(), registration)
		items = append(items, item)
		switch item.Status {
		case "found":
			found++
		case "not_found":
			notFound++
		default:
			failed++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"items": items, "checked": len(items), "found": found, "not_found": notFound, "failed": failed,
	})
}

func (h *Handler) checkRegistrationPlusMail(ctx context.Context, registration models.Registration) (item plusMailCheckItem) {
	item = plusMailCheckItem{RegistrationID: registration.ID, Email: registration.Email, Status: "not_found"}
	checkedAt := time.Now()
	defer func() {
		updates := map[string]any{
			"plus_mail_status": item.Status, "plus_mail_subject": item.Subject,
			"plus_mail_error": item.Error, "plus_mail_checked_at": checkedAt,
			"plus_mail_received_at": item.ReceivedAt,
		}
		if item.Status == "found" {
			updates["plan_type"] = "plus"
			updates["auth_data"] = authDataWithPlan(registration.AuthData, "plus")
		}
		h.DB.Model(&models.Registration{}).Where("id = ?", registration.ID).Updates(updates)
	}()
	mailbox, err := h.registrationMailbox(registration)
	if err != nil {
		item.Status = "error"
		item.Error = "未找到关联邮箱"
		return item
	}
	account, accountErr := h.mailboxAccount(mailbox)
	if accountErr != nil {
		item.Status = "error"
		item.Error = plusMailError(accountErr)
		return item
	}
	var messages []mailfetch.Message
	if strings.TrimSpace(mailbox.CodeURL) != "" {
		archive, fetchErr := h.Mail.GetCodeURLArchive(ctx, account, plusMailLimit)
		if fetchErr != nil {
			item.Status = "error"
			item.Error = plusMailError(fetchErr)
			return item
		}
		messages = []mailfetch.Message{archive}
	} else {
		messages, err = h.Mail.SearchMessages(ctx, account, plusMailSearchPhrase, plusMailLimit)
		if err != nil {
			messages, err = h.Mail.ListMessages(ctx, account, plusMailRecentLimit)
		}
		if err != nil {
			item.Status = "error"
			item.Error = plusMailError(err)
			return item
		}
	}

	detailFailed := false
	for _, message := range messages {
		full := message
		if strings.TrimSpace(full.Text) == "" && strings.TrimSpace(full.HTML) == "" {
			full, err = h.Mail.GetMessage(ctx, account, message.ID)
			if err != nil {
				detailFailed = true
				continue
			}
		}
		if !looksLikePlusActivationMail(full) {
			continue
		}
		item.Status = "found"
		item.Subject = strings.TrimSpace(full.Subject)
		if item.Subject == "" || item.Subject == "邮箱历史邮件" {
			item.Subject = "ChatGPT Plus 订阅确认"
		}
		if full.Subject != "邮箱历史邮件" && !full.ReceivedAt.IsZero() {
			receivedAt := full.ReceivedAt
			item.ReceivedAt = &receivedAt
		}
		return item
	}
	if detailFailed {
		item.Status = "error"
		item.Error = "部分 Plus 候选邮件正文读取失败"
	}
	return item
}

func (h *Handler) registrationMailbox(registration models.Registration) (models.Mailbox, error) {
	var mailbox models.Mailbox
	if registration.MailboxID != 0 {
		if err := h.DB.First(&mailbox, registration.MailboxID).Error; err == nil {
			return mailbox, nil
		}
	}
	baseEmail := emailalias.Base(registration.Email)
	if err := h.DB.Where("LOWER(email) = ?", strings.ToLower(baseEmail)).First(&mailbox).Error; err != nil {
		return models.Mailbox{}, err
	}
	return mailbox, nil
}

func looksLikePlusActivationMail(message mailfetch.Message) bool {
	header := strings.ToLower(strings.Join([]string{message.From, message.FromName, message.Subject}, " "))
	content := strings.ToLower(strings.Join([]string{header, message.Text}, " "))
	if strings.TrimSpace(message.Text) == "" && strings.TrimSpace(message.HTML) != "" {
		content += " " + strings.ToLower(message.HTML)
	}
	if !strings.Contains(content, "plus") {
		return false
	}
	if message.Subject != "邮箱历史邮件" {
		return looksLikePlusEvidence(header, content)
	}
	for offset := 0; offset < len(content); {
		index := strings.Index(content[offset:], "plus")
		if index < 0 {
			break
		}
		index += offset
		start := index - 800
		if start < 0 {
			start = 0
		}
		end := index + 800
		if end > len(content) {
			end = len(content)
		}
		if looksLikePlusEvidence("", content[start:end]) {
			return true
		}
		offset = index + len("plus")
	}
	return false
}

func looksLikePlusEvidence(header, content string) bool {
	if containsAnyPhrase(content, plusMailNegativePhrases) {
		return false
	}
	if containsAnyPhrase(content, plusMailPhrases) {
		return true
	}
	senderMatched := containsAnyPhrase(header+" "+content, plusMailSenderPhrases)
	brandMatched := strings.Contains(content, "chatgpt") || strings.Contains(content, "openai")
	evidenceMatched := containsAnyPhrase(content, plusMailEvidencePhrases)
	return evidenceMatched && (brandMatched || senderMatched)
}

func containsAnyPhrase(content string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(content, phrase) {
			return true
		}
	}
	return false
}

func plusMailError(err error) string {
	switch {
	case errors.Is(err, mailfetch.ErrMissingCreds):
		return "关联邮箱缺少读取凭据"
	case errors.Is(err, mailfetch.ErrAuthFailed):
		return "关联邮箱鉴权失败"
	case errors.Is(err, mailfetch.ErrCodeNotFound):
		return "取件 URL 暂无邮件"
	case errors.Is(err, mailfetch.ErrInvalidCodeURL):
		return "取件 URL 格式错误"
	case errors.Is(err, mailfetch.ErrCodeAPIFailed):
		return "取件 URL 访问失败"
	default:
		return "读取邮箱失败"
	}
}
