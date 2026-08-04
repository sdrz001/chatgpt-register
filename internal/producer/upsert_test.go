package producer

import (
	"testing"
	"time"

	"chatgpt-register/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestMailAccountIncludesCodeURL(t *testing.T) {
	mailbox := models.Mailbox{
		Email: "user@icloud.com", Provider: "api", ClientID: "unused-client", RefreshToken: "unused-refresh",
		CodeURL: "https://codes.example.test/api/code?username=user%40icloud.com&password=secret",
	}
	account := mailAccount(mailbox)
	if account.Email != mailbox.Email || account.Provider != mailbox.Provider || account.ClientID != mailbox.ClientID ||
		account.RefreshToken != mailbox.RefreshToken || account.CodeURL != mailbox.CodeURL {
		t.Fatalf("account=%+v", account)
	}
}

func TestZeroFissionCountOnlyClaimsMotherAccount(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Mailbox{}, &models.Registration{}, &models.Setting{}); err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.Setting{Key: "fission_count", Value: "0"}).Error; err != nil {
		t.Fatal(err)
	}
	mailbox := models.Mailbox{Email: "mother@example.test", Status: "verified"}
	if err := database.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	producer := &Producer{db: database, inflight: map[string]uint{}}
	config := producer.loadConfig()
	if config.FissionCount != 0 {
		t.Fatalf("FissionCount=%d", config.FissionCount)
	}
	claimedMailbox, email, isMother, ok := producer.nextJob(config)
	if !ok || !isMother || claimedMailbox.ID != mailbox.ID || email != mailbox.Email {
		t.Fatalf("first job mailbox=%d email=%q isMother=%v ok=%v", claimedMailbox.ID, email, isMother, ok)
	}
	producer.releaseInflight(email)
	if err := database.Create(&models.Registration{
		MailboxID: mailbox.ID, Email: mailbox.Email, Status: "registered", IsMother: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, email, _, ok := producer.nextJob(config); ok || email != "" {
		t.Fatalf("unexpected fission job email=%q ok=%v", email, ok)
	}
}

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
