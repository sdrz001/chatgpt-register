package db

import (
	"path/filepath"
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
	for _, field := range []string{
		"proxy",
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
