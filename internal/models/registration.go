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
	ID        uint   `gorm:"primaryKey" json:"id"`
	Email     string `gorm:"size:255;not null;uniqueIndex" json:"email"`
	MailboxID uint   `gorm:"index" json:"mailbox_id"`
	Password  string `gorm:"size:255" json:"-"`
	Username  string `gorm:"size:255" json:"username"`
	Proxy     string `gorm:"type:text" json:"-"`

	Status  string `gorm:"size:32;default:pending" json:"status"`
	Shipped bool   `gorm:"default:false" json:"shipped"` // 出库状态：true=已出库

	// 生产结果
	AuthData          string     `gorm:"type:text" json:"auth_data,omitempty"` // 完整 auth.json
	AccountID         string     `gorm:"size:255" json:"account_id"`
	UserID            string     `gorm:"size:255" json:"user_id"`
	PlanType          string     `gorm:"size:64" json:"plan_type"`
	ATStatus          string     `gorm:"column:at_status;size:16;not null;default:unchecked;index" json:"at_status"`
	ATError           string     `gorm:"column:at_error;type:text" json:"at_error"`
	ATCheckedAt       *time.Time `gorm:"column:at_checked_at" json:"at_checked_at"`
	ATExpiresAt       *time.Time `gorm:"column:at_expires_at" json:"at_expires_at"`
	CategoryID        *uint      `gorm:"index" json:"category_id"`
	Category          *Category  `gorm:"constraint:OnUpdate:CASCADE,OnDelete:SET NULL;" json:"category,omitempty"`
	CodexStatus       string     `gorm:"size:32;not null;default:pending" json:"codex_status"`
	CodexError        string     `gorm:"type:text" json:"codex_error"`
	CodexAuthorizedAt *time.Time `json:"codex_authorized_at"`
	Sub2APIStatus     string     `gorm:"column:sub2api_status;size:32;not null;default:not_imported" json:"sub2api_status"`
	Sub2APIAccountID  *int64     `gorm:"column:sub2api_account_id" json:"sub2api_account_id"`
	Sub2APIError      string     `gorm:"column:sub2api_error;type:text" json:"sub2api_error"`
	Sub2APIImportedAt *time.Time `gorm:"column:sub2api_imported_at" json:"sub2api_imported_at"`
	IsMother          bool       `gorm:"default:false" json:"is_mother"` // 是否母号（该邮箱主号）

	Log       string    `gorm:"type:text" json:"log,omitempty"` // 本账号执行日志
	Shot      []byte    `gorm:"type:blob" json:"-"`             // 注册失败时的页面截图(PNG)
	Note      string    `gorm:"type:text" json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
