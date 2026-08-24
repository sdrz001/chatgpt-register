package producer

import (
	"reflect"
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

func TestMailAccountLoadsDomainMailSettings(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Setting{}); err != nil {
		t.Fatal(err)
	}
	settings := []models.Setting{
		{Key: "mailbox_source", Value: "domain_api"},
		{Key: "domain_mail_url", Value: "https://mail.example.test"},
		{Key: "domain_mail_api_key", Value: "secret"},
		{Key: "domain_mail_domain", Value: "moemail.app"},
	}
	if err := database.Create(&settings).Error; err != nil {
		t.Fatal(err)
	}
	mailbox := models.Mailbox{Email: "generated@moemail.app", Provider: "domain_api", RemoteMailboxID: "remote-1"}
	account, err := (&Producer{db: database}).mailAccount(mailbox)
	if err != nil {
		t.Fatal(err)
	}
	if account.RemoteMailboxID != "remote-1" || account.DomainMailBaseURL != "https://mail.example.test" || account.DomainMailAPIKey != "secret" {
		t.Fatalf("account=%+v", account)
	}
}

func TestDomainMailMailboxNeverClaimsFissionJob(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Mailbox{}, &models.Registration{}); err != nil {
		t.Fatal(err)
	}
	mailbox := models.Mailbox{Email: "generated@moemail.app", Provider: "domain_api", RemoteMailboxID: "remote-1", Status: "verified"}
	if err := database.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.Registration{Email: mailbox.Email, MailboxID: mailbox.ID, Status: "registered"}).Error; err != nil {
		t.Fatal(err)
	}
	producer := &Producer{db: database, inflight: map[string]uint{}, attempts: map[string]registrationAttempt{}}
	if _, email, _, ok := producer.nextJob(Config{FissionCount: 10}, Scope{}); ok || email != "" {
		t.Fatalf("unexpected domain mail fission email=%q ok=%v", email, ok)
	}
}

func TestRegistrationPasswordReusesExistingPassword(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Registration{}); err != nil {
		t.Fatal(err)
	}
	registration := models.Registration{Email: "retry@example.test", Password: "existing-password", Status: "register_failed"}
	if err := database.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}
	producer := &Producer{db: database}
	if password := producer.registrationPassword(registration.Email); password != registration.Password {
		t.Fatalf("password=%q", password)
	}
	if password := producer.registrationPassword("new@example.test"); password == "" || password == registration.Password {
		t.Fatalf("generated password=%q", password)
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
	if config.BrowserBackend != "rod" || config.RegistrationFlow != "email_code" || config.PythonExecutable != "" {
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
		{Key: "registration_flow", Value: "password"},
		{Key: "python_executable", Value: `C:\Python\python.exe`},
	}
	if err := database.Create(&settings).Error; err != nil {
		t.Fatal(err)
	}
	config := (&Producer{db: database}).loadConfig()
	if config.BrowserBackend != "cloakbrowser" || config.RegistrationFlow != "password" || config.PythonExecutable != `C:\Python\python.exe` {
		t.Fatalf("config=%+v", config)
	}
}

func TestNextProxyRotatesTaskProxySnapshot(t *testing.T) {
	producer := &Producer{}
	config := Config{Proxies: []string{"proxy-a", "proxy-b", "proxy-c"}}
	got := []string{producer.nextProxy(config), producer.nextProxy(config), producer.nextProxy(config), producer.nextProxy(config)}
	want := []string{"proxy-a", "proxy-b", "proxy-c", "proxy-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rotated proxies=%v want=%v", got, want)
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
	producer := &Producer{db: database, inflight: map[string]uint{}, attempts: map[string]registrationAttempt{}}
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
	producer := &Producer{db: database, inflight: map[string]uint{}, attempts: map[string]registrationAttempt{}}
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

func TestNextJobLimitsFailuresAndLetsOtherMailboxesRun(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Mailbox{}, &models.Registration{}); err != nil {
		t.Fatal(err)
	}
	mailboxes := []models.Mailbox{
		{Email: "first@example.test", Status: "verified"},
		{Email: "second@example.test", Status: "verified"},
	}
	if err := database.Create(&mailboxes).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	originalNow := producerNow
	producerNow = func() time.Time { return now }
	t.Cleanup(func() { producerNow = originalNow })
	producer := &Producer{db: database, inflight: map[string]uint{}, attempts: map[string]registrationAttempt{}}
	config := Config{FissionCount: 0}

	claimed, email, _, ok := producer.nextJob(config, Scope{})
	if !ok || claimed.ID != mailboxes[0].ID {
		t.Fatalf("first claim mailbox=%d email=%q ok=%v", claimed.ID, email, ok)
	}
	producer.releaseInflight(email)
	producer.scheduleRegistrationRetry(email)
	claimed, email, _, ok = producer.nextJob(config, Scope{})
	if !ok || claimed.ID != mailboxes[1].ID {
		t.Fatalf("second claim mailbox=%d email=%q ok=%v", claimed.ID, email, ok)
	}
	producer.releaseInflight(email)
	producer.scheduleRegistrationRetry(email)

	for attempt := 1; attempt < registrationMaxAttempts; attempt++ {
		now = now.Add(time.Hour)
		for _, mailbox := range mailboxes {
			claimed, email, _, ok = producer.nextJob(config, Scope{})
			if !ok || claimed.ID != mailbox.ID {
				t.Fatalf("attempt=%d mailbox=%d claimed=%d email=%q ok=%v", attempt+1, mailbox.ID, claimed.ID, email, ok)
			}
			producer.releaseInflight(email)
			producer.scheduleRegistrationRetry(email)
		}
	}
	if _, email, _, ok = producer.nextJob(config, Scope{}); ok || email != "" {
		t.Fatalf("exhausted job email=%q ok=%v", email, ok)
	}
}

func TestExhaustRegistrationRetrySkipsAddressAndLetsOtherMailboxRun(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Mailbox{}, &models.Registration{}); err != nil {
		t.Fatal(err)
	}
	mailboxes := []models.Mailbox{
		{Email: "terminal@example.test", Status: "verified"},
		{Email: "available@example.test", Status: "verified"},
	}
	if err := database.Create(&mailboxes).Error; err != nil {
		t.Fatal(err)
	}
	producer := &Producer{db: database, inflight: map[string]uint{}, attempts: map[string]registrationAttempt{}}
	producer.exhaustRegistrationRetry(mailboxes[0].Email)
	claimed, email, _, ok := producer.nextJob(Config{FissionCount: 0}, Scope{})
	if !ok || claimed.ID != mailboxes[1].ID || email != mailboxes[1].Email {
		t.Fatalf("claimed=%d email=%q ok=%v", claimed.ID, email, ok)
	}
	if producer.registrationAttemptReady(mailboxes[0].Email) {
		t.Fatal("terminal address remained retryable")
	}
}

func TestFailedFissionRetriesThenConsumesOneSlot(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Mailbox{}, &models.Registration{}); err != nil {
		t.Fatal(err)
	}
	mailbox := models.Mailbox{Email: "mother@example.test", Status: "verified"}
	if err := database.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.Registration{MailboxID: mailbox.ID, Email: mailbox.Email, Status: "registered", IsMother: true}).Error; err != nil {
		t.Fatal(err)
	}
	alias := "child@example.test"
	if err := database.Create(&models.Registration{MailboxID: mailbox.ID, Email: alias, Status: "register_failed"}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	originalNow := producerNow
	producerNow = func() time.Time { return now }
	t.Cleanup(func() { producerNow = originalNow })
	producer := &Producer{db: database, inflight: map[string]uint{}, attempts: map[string]registrationAttempt{}}
	producer.scheduleRegistrationRetry(alias)
	now = now.Add(time.Hour)
	claimed, email, isMother, ok := producer.nextJob(Config{FissionCount: 1}, Scope{})
	if !ok || isMother || claimed.ID != mailbox.ID || email != alias {
		t.Fatalf("retry claim mailbox=%d email=%q isMother=%v ok=%v", claimed.ID, email, isMother, ok)
	}
	producer.releaseInflight(email)
	producer.scheduleRegistrationRetry(alias)
	producer.scheduleRegistrationRetry(alias)
	if _, email, _, ok = producer.nextJob(Config{FissionCount: 1}, Scope{}); ok || email != "" {
		t.Fatalf("exhausted fission email=%q ok=%v", email, ok)
	}
	if count := producer.fissionCount(mailbox); count != 1 {
		t.Fatalf("fission count=%d", count)
	}
}

func TestUpsertClearsStaleFailureShotOnRetryAndSuccess(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Registration{}); err != nil {
		t.Fatal(err)
	}
	existing := models.Registration{Email: "retry@example.test", Status: "register_failed", Shot: []byte("old-shot")}
	if err := database.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	producer := &Producer{db: database}
	producer.upsert(models.Registration{Email: existing.Email, Status: "registering"})
	var reloaded models.Registration
	if err := database.First(&reloaded, existing.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Shot) != 0 {
		t.Fatalf("retry retained stale shot: %d bytes", len(reloaded.Shot))
	}
	if err := database.Model(&reloaded).Update("shot", []byte("new-failure-shot")).Error; err != nil {
		t.Fatal(err)
	}
	producer.upsert(models.Registration{Email: existing.Email, Status: "registered", AuthData: `{"access_token":"new"}`, ATStatus: "valid"})
	reloaded = models.Registration{}
	if err := database.First(&reloaded, existing.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Shot) != 0 {
		t.Fatalf("success retained stale shot: %d bytes", len(reloaded.Shot))
	}
}

func TestUpsertPersistsPasswordAndTwoFactorCredentials(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Registration{}); err != nil {
		t.Fatal(err)
	}
	existing := models.Registration{Email: "password@example.test", Status: "registering"}
	if err := database.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	producer := &Producer{db: database}
	producer.upsert(models.Registration{
		Email: existing.Email, Password: "generated-password", RegistrationFlow: "password", Status: "registered",
		AuthData: `{"access_token":"token"}`, TwoFactorEnabled: true, TwoFactorSecret: "TOTP-SECRET",
		TwoFactorFactorID: "FACTOR-ID", TwoFactorRecoveryCodes: `["RECOVERY-1"]`,
	})
	var reloaded models.Registration
	if err := database.First(&reloaded, existing.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.Password != "generated-password" || reloaded.RegistrationFlow != "password" || !reloaded.TwoFactorEnabled || reloaded.TwoFactorSecret != "TOTP-SECRET" || reloaded.TwoFactorFactorID != "FACTOR-ID" || reloaded.TwoFactorRecoveryCodes != `["RECOVERY-1"]` {
		t.Fatalf("registration=%+v", reloaded)
	}
}

func TestUpsertWithNewAuthResetsTrialEligibility(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Registration{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	existing := models.Registration{
		Email: "trial@example.test", Status: "registered", AuthData: `{"access_token":"old"}`,
		TrialStatus: "eligible", TrialPlan: "plus", TrialLabel: "1-month free trial",
		TrialPercent: 100, TrialPeriods: 1, TrialPeriodUnit: "month", TrialAutoRenew: true,
		TrialCheckedAt: &now,
	}
	if err := database.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	producer := &Producer{db: database}
	producer.upsert(models.Registration{
		Email: existing.Email, Status: "registered", AuthData: `{"access_token":"new"}`,
		ATStatus: "valid", TrialStatus: "eligible",
	})
	var reloaded models.Registration
	if err := database.First(&reloaded, existing.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.TrialStatus != "unchecked" || reloaded.TrialPlan != "" || reloaded.TrialLabel != "" || reloaded.TrialPercent != 0 || reloaded.TrialPeriods != 0 || reloaded.TrialPeriodUnit != "" || reloaded.TrialAutoRenew || reloaded.TrialCheckedAt != nil {
		t.Fatalf("trial was not reset: %+v", reloaded)
	}
}

func TestUpsertPersistsRegisterLocation(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Registration{}); err != nil {
		t.Fatal(err)
	}
	existing := models.Registration{Email: "geo@example.test", Status: "registering"}
	if err := database.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	producer := &Producer{db: database}
	producer.upsert(models.Registration{
		Email: existing.Email, Status: "registered",
		RegisterCountry: "JP", RegisterIP: "203.0.113.8", RegisterCity: "Tokyo",
	})
	if err := database.First(&existing, existing.ID).Error; err != nil {
		t.Fatal(err)
	}
	if existing.RegisterCountry != "JP" || existing.RegisterIP != "203.0.113.8" || existing.RegisterCity != "Tokyo" {
		t.Fatalf("registration=%+v", existing)
	}
}

func TestUpsertPersistsCategoryWithoutClearingIntegrationState(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Category{}, &models.Registration{}); err != nil {
		t.Fatal(err)
	}
	category := models.Category{Scope: "account", Name: "Plus 邮箱"}
	if err := database.Create(&category).Error; err != nil {
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
		Email: existing.Email, Password: "new-password", Status: "registered", Proxy: "http://new-proxy", CategoryID: &category.ID,
	})
	if err := database.First(&existing, existing.ID).Error; err != nil {
		t.Fatal(err)
	}
	if existing.Proxy != "http://new-proxy" || existing.Password != "new-password" || existing.CategoryID == nil || *existing.CategoryID != category.ID {
		t.Fatalf("registration=%+v", existing)
	}
	if existing.CodexStatus != "authorized" || existing.Sub2APIStatus != "imported" || existing.Sub2APIAccountID == nil || *existing.Sub2APIAccountID != 91 {
		t.Fatalf("integration state cleared: %+v", existing)
	}
}
