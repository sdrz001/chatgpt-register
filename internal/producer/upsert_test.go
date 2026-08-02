package producer

import (
	"testing"
	"time"

	"chatgpt-register/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestUpsertPersistsProxyWithoutClearingIntegrationState(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Registration{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	accountID := int64(91)
	existing := models.Registration{
		Email: "user@example.test", Status: "registered", Proxy: "http://old-proxy",
		CodexStatus: "authorized", CodexAuthorizedAt: &now,
		Sub2APIStatus: "imported", Sub2APIAccountID: &accountID, Sub2APIImportedAt: &now,
	}
	if err := database.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	producer := &Producer{db: database}
	producer.upsert(models.Registration{
		Email: existing.Email, Password: "new-password", Status: "registered", Proxy: "http://new-proxy",
	})
	if err := database.First(&existing, existing.ID).Error; err != nil {
		t.Fatal(err)
	}
	if existing.Proxy != "http://new-proxy" || existing.Password != "new-password" {
		t.Fatalf("registration=%+v", existing)
	}
	if existing.CodexStatus != "authorized" || existing.Sub2APIStatus != "imported" || existing.Sub2APIAccountID == nil || *existing.Sub2APIAccountID != 91 {
		t.Fatalf("integration state cleared: %+v", existing)
	}
}
