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

func TestLoadConfigDefaultsBrowserBackendToRod(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Setting{}); err != nil {
		t.Fatal(err)
	}
	config := (&Producer{db: database}).loadConfig()
	if config.BrowserBackend != "rod" || config.PythonExecutable != "" {
		t.Fatalf("config=%+v", config)
	}
}

func TestLoadConfigReadsCloakBrowserSettings(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Setting{}); err != nil {
		t.Fatal(err)
	}
	settings := []models.Setting{
		{Key: "browser_backend", Value: "cloakbrowser"},
		{Key: "python_executable", Value: `C:\Python\python.exe`},
	}
	if err := database.Create(&settings).Error; err != nil {
		t.Fatal(err)
	}
	config := (&Producer{db: database}).loadConfig()
	if config.BrowserBackend != "cloakbrowser" || config.PythonExecutable != `C:\Python\python.exe` {
		t.Fatalf("config=%+v", config)
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
	claimedMailbox, email, isMother, ok := producer.nextJob(config, Scope{})
	if !ok || !isMother || claimedMailbox.ID != mailbox.ID || email != mailbox.Email {
		t.Fatalf("first job mailbox=%d email=%q isMother=%v ok=%v", claimedMailbox.ID, email, isMother, ok)
	}
	producer.releaseInflight(email)
	if err := database.Create(&models.Registration{
		MailboxID: mailbox.ID, Email: mailbox.Email, Status: "registered", IsMother: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, email, _, ok := producer.nextJob(config, Scope{}); ok || email != "" {
		t.Fatalf("unexpected fission job email=%q ok=%v", email, ok)
	}
}

func TestNextJobFiltersMailboxScope(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Category{}, &models.Mailbox{}, &models.Registration{}); err != nil {
		t.Fatal(err)
	}
	firstCategory := models.Category{Scope: "mailbox", Name: "第一组"}
	secondCategory := models.Category{Scope: "mailbox", Name: "第二组"}
	if err := database.Create(&firstCategory).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&secondCategory).Error; err != nil {
		t.Fatal(err)
	}
	mailboxes := []models.Mailbox{
		{Email: "first@example.test", Status: "verified", CategoryID: &firstCategory.ID},
		{Email: "second@example.test", Status: "verified", CategoryID: &secondCategory.ID},
		{Email: "third@example.test", Status: "verified", CategoryID: &firstCategory.ID},
	}
	if err := database.Create(&mailboxes).Error; err != nil {
		t.Fatal(err)
	}
	producer := &Producer{db: database, inflight: map[string]uint{}}
	config := Config{FissionCount: 0}
	claimed, _, _, ok := producer.nextJob(config, Scope{CategoryID: &secondCategory.ID})
	if !ok || claimed.ID != mailboxes[1].ID {
		t.Fatalf("category claimed=%d ok=%v", claimed.ID, ok)
	}
	producer.releaseInflight(claimed.Email)
	claimed, _, _, ok = producer.nextJob(config, Scope{MailboxIDs: []uint{mailboxes[2].ID}})
	if !ok || claimed.ID != mailboxes[2].ID {
		t.Fatalf("mailboxes claimed=%d ok=%v", claimed.ID, ok)
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
