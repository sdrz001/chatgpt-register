package models

import "time"

type SMSActivation struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	RegistrationID uint       `gorm:"index;not null" json:"registration_id"`
	Provider       string     `gorm:"size:32;not null;uniqueIndex:idx_sms_provider_activation" json:"provider"`
	ActivationID   string     `gorm:"size:255;not null;uniqueIndex:idx_sms_provider_activation" json:"activation_id"`
	PhoneNumber    string     `gorm:"size:64;not null" json:"phone_number"`
	CountryID      int        `json:"country_id"`
	Service        string     `gorm:"size:32;not null" json:"service"`
	Status         string     `gorm:"size:32;not null;default:allocated" json:"status"`
	Error          string     `gorm:"type:text" json:"error"`
	ClosedAt       *time.Time `json:"closed_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}
