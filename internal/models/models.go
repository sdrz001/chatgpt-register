package models

import "time"

type ProxyPool struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	Name       string    `gorm:"size:64;not null;uniqueIndex" json:"name"`
	Proxies    string    `gorm:"type:text" json:"proxies,omitempty"`
	ProxyCount int       `gorm:"-" json:"proxy_count"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Category struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Scope     string    `gorm:"size:16;not null;uniqueIndex:idx_category_scope_name" json:"scope"`
	Name      string    `gorm:"size:64;not null;uniqueIndex:idx_category_scope_name" json:"name"`
	ItemCount int64     `gorm:"-" json:"item_count"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Mailbox 邮箱管理
//
// Status 流转: unverified(待验证) / verifying(验证中) / verify_failed(验证失败) / verified(已验证)
type Mailbox struct {
	ID                uint      `gorm:"primaryKey" json:"id"`
	Email             string    `gorm:"size:255;not null;uniqueIndex" json:"email"`
	Password          string    `gorm:"size:255" json:"password"`
	Provider          string    `gorm:"size:64" json:"provider"` // gmail / outlook / 临时邮箱...
	ClientID          string    `gorm:"size:255" json:"client_id"`
	RefreshToken      string    `gorm:"type:text" json:"refresh_token"`
	CodeURL           string    `gorm:"type:text" json:"-"`
	RemoteMailboxID   string    `gorm:"size:255;index" json:"-"`
	Status            string    `gorm:"size:32;default:unverified" json:"status"`
	CategoryID        *uint     `gorm:"index" json:"category_id"`
	Category          *Category `gorm:"constraint:OnUpdate:CASCADE,OnDelete:SET NULL;" json:"category,omitempty"`
	Note              string    `gorm:"type:text" json:"note"`
	CodeURLConfigured bool      `gorm:"-" json:"code_url_configured"`
	RegisterCount     int       `gorm:"-" json:"register_count"`
	RegisterLimit     int       `gorm:"-" json:"register_limit"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Setting 系统设置 (key-value)
type Setting struct {
	Key       string    `gorm:"primaryKey;size:128" json:"key"`
	Value     string    `gorm:"type:text" json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Admin 后台管理员账户。
//
// Token 存当前唯一有效的 JWT：签发即写库、换新即作废旧的（旧 token 立即失效）。
// TokenIssuedAt 记录本 token 的签发时间，用于“超过 2 小时自动续期”。
type Admin struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	Username      string    `gorm:"size:64;uniqueIndex;not null" json:"username"`
	PasswordHash  string    `gorm:"size:255;not null" json:"-"`
	Token         string    `gorm:"type:text" json:"-"`
	TokenIssuedAt time.Time `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
