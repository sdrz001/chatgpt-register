package db

import (
	"path/filepath"
	"strconv"
	"testing"

	"chatgpt-register/internal/models"
)

func TestInitMigratesIntegrationModels(t *testing.T) {
	database, err := Init(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if !database.Migrator().HasTable(&models.SMSActivation{}) {
		t.Fatal("sms_activations table missing")
	}
	if !database.Migrator().HasTable(&models.Category{}) {
		t.Fatal("categories table missing")
	}
	if !database.Migrator().HasTable(&models.ProxyPool{}) {
		t.Fatal("proxy_pools table missing")
	}
	for _, field := range []string{
		"proxy",
		"at_status",
		"at_error",
		"at_checked_at",
		"at_expires_at",
		"trial_status",
		"trial_plan",
		"trial_label",
		"trial_percent",
		"trial_periods",
		"trial_period_unit",
		"trial_auto_renew",
		"trial_error",
		"trial_checked_at",
		"category_id",
		"codex_status",
		"codex_error",
		"codex_authorized_at",
		"sub2api_status",
		"sub2api_account_id",
		"sub2api_error",
		"sub2api_imported_at",
	} {
		if !database.Migrator().HasColumn(&models.Registration{}, field) {
			t.Fatalf("registrations.%s missing", field)
		}
	}
	if !database.Migrator().HasColumn(&models.Mailbox{}, "category_id") {
		t.Fatal("mailboxes.category_id missing")
	}
}

func TestInitMigratesLegacyProxyListOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&[]models.Setting{
		{Key: "proxy_enabled", Value: "1"},
		{Key: "proxy_list", Value: "proxy-a:8080\nproxy-b:8080"},
	}).Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := database.DB()
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Init(path)
	if err != nil {
		t.Fatal(err)
	}
	var pool models.ProxyPool
	if err := database.First(&pool).Error; err != nil {
		t.Fatal(err)
	}
	if pool.Name != "默认代理池" || pool.Proxies != "proxy-a:8080\nproxy-b:8080" {
		t.Fatalf("pool=%+v", pool)
	}
	var setting models.Setting
	if err := database.First(&setting, "key = ?", "default_proxy_pool_id").Error; err != nil || setting.Value != strconv.FormatUint(uint64(pool.ID), 10) {
		t.Fatalf("setting=%+v error=%v", setting, err)
	}
	sqlDB, _ = database.DB()
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Init(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { secondSQL, _ := database.DB(); _ = secondSQL.Close() }()
	var count int64
	if err := database.Model(&models.ProxyPool{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("count=%d error=%v", count, err)
	}
}

func TestInitBackfillsRegistrationCategoryFromMailbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	mailboxCategory := models.Category{Scope: "mailbox", Name: "老邮箱组"}
	if err := database.Create(&mailboxCategory).Error; err != nil {
		t.Fatal(err)
	}
	mailbox := models.Mailbox{Email: "legacy@example.test", Status: "verified", CategoryID: &mailboxCategory.ID}
	if err := database.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	registration := models.Registration{Email: "legacy+account@example.test", MailboxID: mailbox.ID, Status: "registered"}
	if err := database.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}
	firstSQL, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := firstSQL.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Init(path)
	if err != nil {
		t.Fatal(err)
	}
	secondSQL, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer secondSQL.Close()
	if err := database.First(&registration, registration.ID).Error; err != nil {
		t.Fatal(err)
	}
	if registration.CategoryID == nil {
		t.Fatal("registration category was not backfilled")
	}
	var category models.Category
	if err := database.First(&category, *registration.CategoryID).Error; err != nil {
		t.Fatal(err)
	}
	if category.Scope != "account" || category.Name != mailboxCategory.Name {
		t.Fatalf("category=%+v", category)
	}
}

func TestInitReclaimsOrphanATCheckStates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	registration := models.Registration{
		Email: "at-check@example.test", Status: "registered",
		ATStatus: "checking", ATError: "old", TrialStatus: "checking", TrialError: "old",
	}
	if err := database.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}
	firstSQL, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := firstSQL.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Init(path)
	if err != nil {
		t.Fatal(err)
	}
	secondSQL, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer secondSQL.Close()
	if err := database.First(&registration, registration.ID).Error; err != nil {
		t.Fatal(err)
	}
	if registration.ATStatus != "unchecked" || registration.TrialStatus != "unchecked" || registration.ATError != "" || registration.TrialError != "" {
		t.Fatalf("registration=%+v", registration)
	}
}

func TestInitReclaimsOrphanIntegrationStates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	firstSQL, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	reg := models.Registration{
		Email: "user@example.test", Status: "registered",
		CodexStatus: "authorizing", Sub2APIStatus: "importing",
	}
	if err := database.Create(&reg).Error; err != nil {
		t.Fatal(err)
	}
	activation := models.SMSActivation{
		RegistrationID: reg.ID, Provider: "hero-sms", ActivationID: "activation-1",
		PhoneNumber: "+12025550123", CountryID: 187, Service: "dr", Status: "waiting",
	}
	if err := database.Create(&activation).Error; err != nil {
		t.Fatal(err)
	}
	if err := firstSQL.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Init(path)
	if err != nil {
		t.Fatal(err)
	}
	secondSQL, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer secondSQL.Close()
	if err := database.First(&reg, reg.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reg.CodexStatus != "failed" || reg.Sub2APIStatus != "failed" {
		t.Fatalf("statuses=%s/%s", reg.CodexStatus, reg.Sub2APIStatus)
	}
	if err := database.First(&activation, activation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if activation.Status != "orphaned" {
		t.Fatalf("activation status=%s", activation.Status)
	}
}
