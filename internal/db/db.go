package db

import (
	"errors"
	"strconv"
	"strings"

	"chatgpt-register/internal/categorysync"
	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/emailalias"
	"chatgpt-register/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func Init(path string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&models.ProxyPool{}, &models.Category{}, &models.Registration{}, &models.SMSActivation{}, &models.Mailbox{}, &models.Setting{}, &models.Admin{}); err != nil {
		return nil, err
	}
	normalizeLegacyStatuses(db)
	reclaimOrphanRegistering(db)
	reclaimOrphanIntegrations(db)
	reclaimOrphanATChecks(db)
	backfillRegistrationMailboxIDs(db)
	backfillRegisterLocations(db)
	categorysync.BackfillRegistrations(db)
	if err := migrateLegacyProxyPool(db); err != nil {
		return nil, err
	}
	return db, nil
}

// reclaimOrphanRegistering 启动时把残留的 registering 记录标为 register_failed。
// 生产任务状态只在内存里，程序重启后这些"注册中"记录不会再有人推进，
// 置为失败后可在下次生产时被重新领取（母号+裂变补齐规则）。
func reclaimOrphanRegistering(db *gorm.DB) {
	db.Model(&models.Registration{}).Where("status = ?", "registering").
		Updates(map[string]any{"status": "register_failed", "note": "程序重启中断，可重新生产"})
}

func reclaimOrphanIntegrations(db *gorm.DB) {
	db.Model(&models.Registration{}).Where("codex_status = ?", "authorizing").
		Updates(map[string]any{"codex_status": "failed", "codex_error": "程序重启中断，可重新授权"})
	db.Model(&models.Registration{}).Where("sub2api_status = ?", "importing").
		Updates(map[string]any{"sub2api_status": "failed", "sub2api_error": "程序重启中断，可重新导入"})
	db.Model(&models.SMSActivation{}).Where("status IN ?", []string{"allocated", "waiting"}).
		Updates(map[string]any{"status": "orphaned", "error": "程序重启中断，需核对供应商订单"})
}

func reclaimOrphanATChecks(db *gorm.DB) {
	db.Model(&models.Registration{}).Where("at_status = ? OR trial_status = ?", "checking", "checking").
		Updates(map[string]any{"at_status": "unchecked", "at_error": "", "trial_status": "unchecked", "trial_error": ""})
}

// normalizeLegacyStatuses 把旧的 AdSkull 验证态注册记录迁移到新的生产态。
// Mailbox 的 unverified/verified 表示邮箱凭据是否校验通过，语义不变，保持原样。
func normalizeLegacyStatuses(db *gorm.DB) {
	regStatusMap := map[string]string{
		"unverified":    "pending",
		"verifying":     "registering",
		"verify_failed": "register_failed",
		"verified":      "registered",
	}
	for oldStatus, newStatus := range regStatusMap {
		db.Model(&models.Registration{}).Where("status = ?", oldStatus).Update("status", newStatus)
	}
}

func migrateLegacyProxyPool(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&models.ProxyPool{}).Count(&count).Error; err != nil || count > 0 {
			return err
		}
		var list models.Setting
		if err := tx.Where("key = ?", "proxy_list").First(&list).Error; errors.Is(err, gorm.ErrRecordNotFound) || strings.TrimSpace(list.Value) == "" {
			return nil
		} else if err != nil {
			return err
		}
		var enabled models.Setting
		if err := tx.Where("key = ?", "proxy_enabled").First(&enabled).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		pool := models.ProxyPool{Name: "默认代理池", Proxies: strings.TrimSpace(list.Value)}
		if err := tx.Create(&pool).Error; err != nil {
			return err
		}
		defaultID := "0"
		if strings.TrimSpace(enabled.Value) == "1" {
			defaultID = strconv.FormatUint(uint64(pool.ID), 10)
		}
		return tx.Save(&models.Setting{Key: "default_proxy_pool_id", Value: defaultID}).Error
	})
}

func backfillRegisterLocations(db *gorm.DB) {
	var regs []models.Registration
	if err := db.Select("id", "log", "register_country", "register_ip", "register_city").
		Where("(register_country = ? OR register_country IS NULL OR register_ip = ? OR register_ip IS NULL OR register_city = ? OR register_city IS NULL)", "", "", "").
		Where("log <> ? AND log IS NOT NULL", "").
		Find(&regs).Error; err != nil {
		return
	}
	for _, reg := range regs {
		loc := codexreg.ParseRegisterLocation(reg.Log)
		if loc.Country == "" && loc.IP == "" && loc.City == "" {
			continue
		}
		updates := map[string]any{}
		if loc.Country != "" && strings.TrimSpace(reg.RegisterCountry) == "" {
			updates["register_country"] = loc.Country
		}
		if loc.IP != "" && strings.TrimSpace(reg.RegisterIP) == "" {
			updates["register_ip"] = loc.IP
		}
		if loc.City != "" && strings.TrimSpace(reg.RegisterCity) == "" {
			updates["register_city"] = loc.City
		}
		if len(updates) > 0 {
			db.Model(&models.Registration{}).Where("id = ?", reg.ID).Updates(updates)
		}
	}
}

func backfillRegistrationMailboxIDs(db *gorm.DB) {
	var regs []models.Registration
	if err := db.Where("mailbox_id IS NULL OR mailbox_id = 0").Find(&regs).Error; err != nil {
		return
	}
	for _, reg := range regs {
		baseEmail := emailalias.Base(reg.Email)
		var mb models.Mailbox
		if err := db.Where("email = ?", baseEmail).First(&mb).Error; err == nil {
			db.Model(&models.Registration{}).Where("id = ?", reg.ID).Update("mailbox_id", mb.ID)
		}
	}
}
