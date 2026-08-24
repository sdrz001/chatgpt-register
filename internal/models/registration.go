package models

import "time"

// Registration 一个待生产 / 已生产的 ChatGPT + Codex 账号。
//
// Status 流转:
//
//	pending(待生产) / registering(注册中) / registered(已注册) / register_failed(注册失败)
//
// 生产成功后 AuthData 存完整的 auth.json（access_token + 账号信息），下载时导出。
// Shipped 表示是否已"出库"（下载即出库）。
type Registration struct {
	ID               uint   `gorm:"primaryKey" json:"id"`
	Email            string `gorm:"size:255;not null;uniqueIndex" json:"email"`
	MailboxID        uint   `gorm:"index" json:"mailbox_id"`
	Password         string `gorm:"size:255" json:"-"`
	Username         string `gorm:"size:255" json:"username"`
	RegistrationFlow string `gorm:"column:registration_flow;size:32;not null;default:email_code;index" json:"registration_flow"`
	Proxy            string `gorm:"type:text" json:"-"`
	RegisterCountry  string `gorm:"column:register_country;size:8;index" json:"register_country"`
	RegisterIP       string `gorm:"column:register_ip;size:64" json:"register_ip"`
	RegisterCity     string `gorm:"column:register_city;size:64" json:"register_city"`

	Status  string `gorm:"size:32;default:pending" json:"status"`
	Shipped bool   `gorm:"default:false" json:"shipped"` // 出库状态：true=已出库

	// 生产结果
	AuthData               string     `gorm:"type:text" json:"auth_data,omitempty"` // 完整 auth.json
	TwoFactorEnabled       bool       `gorm:"column:two_factor_enabled;not null;default:false;index" json:"two_factor_enabled"`
	TwoFactorSecret        string     `gorm:"column:two_factor_secret;type:text" json:"-"`
	TwoFactorFactorID      string     `gorm:"column:two_factor_factor_id;size:255" json:"-"`
	TwoFactorRecoveryCodes string     `gorm:"column:two_factor_recovery_codes;type:text" json:"-"`
	AccountID              string     `gorm:"size:255" json:"account_id"`
	UserID                 string     `gorm:"size:255" json:"user_id"`
	PlanType               string     `gorm:"size:64" json:"plan_type"`
	ATStatus               string     `gorm:"column:at_status;size:16;not null;default:unchecked;index" json:"at_status"`
	ATError                string     `gorm:"column:at_error;type:text" json:"at_error"`
	ATCheckedAt            *time.Time `gorm:"column:at_checked_at" json:"at_checked_at"`
	ATExpiresAt            *time.Time `gorm:"column:at_expires_at" json:"at_expires_at"`
	TrialStatus            string     `gorm:"column:trial_status;size:16;not null;default:unchecked;index" json:"trial_status"`
	TrialPlan              string     `gorm:"column:trial_plan;size:32" json:"trial_plan"`
	TrialLabel             string     `gorm:"column:trial_label;size:128" json:"trial_label"`
	TrialPercent           int        `gorm:"column:trial_percent;not null;default:0" json:"trial_percent"`
	TrialPeriods           int        `gorm:"column:trial_periods;not null;default:0" json:"trial_periods"`
	TrialPeriodUnit        string     `gorm:"column:trial_period_unit;size:16" json:"trial_period_unit"`
	TrialAutoRenew         bool       `gorm:"column:trial_auto_renew;not null;default:false" json:"trial_auto_renew"`
	TrialError             string     `gorm:"column:trial_error;type:text" json:"trial_error"`
	TrialCheckedAt         *time.Time `gorm:"column:trial_checked_at" json:"trial_checked_at"`
	PlusMailStatus         string     `gorm:"column:plus_mail_status;size:16;not null;default:unchecked;index" json:"plus_mail_status"`
	PlusMailSubject        string     `gorm:"column:plus_mail_subject;type:text" json:"plus_mail_subject"`
	PlusMailError          string     `gorm:"column:plus_mail_error;type:text" json:"plus_mail_error"`
	PlusMailCheckedAt      *time.Time `gorm:"column:plus_mail_checked_at" json:"plus_mail_checked_at"`
	PlusMailReceivedAt     *time.Time `gorm:"column:plus_mail_received_at" json:"plus_mail_received_at"`
	CategoryID             *uint      `gorm:"index" json:"category_id"`
	Category               *Category  `gorm:"constraint:OnUpdate:CASCADE,OnDelete:SET NULL;" json:"category,omitempty"`
	CodexStatus            string     `gorm:"size:32;not null;default:pending" json:"codex_status"`
	CodexError             string     `gorm:"type:text" json:"codex_error"`
	CodexAuthorizedAt      *time.Time `json:"codex_authorized_at"`
	Sub2APIStatus          string     `gorm:"column:sub2api_status;size:32;not null;default:not_imported" json:"sub2api_status"`
	Sub2APIAccountID       *int64     `gorm:"column:sub2api_account_id" json:"sub2api_account_id"`
	Sub2APIError           string     `gorm:"column:sub2api_error;type:text" json:"sub2api_error"`
	Sub2APIImportedAt      *time.Time `gorm:"column:sub2api_imported_at" json:"sub2api_imported_at"`
	IsMother               bool       `gorm:"default:false" json:"is_mother"` // 是否母号（该邮箱主号）

	Log       string    `gorm:"type:text" json:"log,omitempty"` // 本账号执行日志
	Shot      []byte    `gorm:"type:blob" json:"-"`             // 注册失败时的页面截图(PNG)
	Note      string    `gorm:"type:text" json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
