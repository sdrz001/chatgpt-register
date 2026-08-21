package producer

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"chatgpt-register/internal/mailcom"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// aliasCandidateProducer 构造一个只需空注册表的 Producer，用于校验候选地址格式。
func aliasCandidateProducer(t *testing.T) *Producer {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Registration{}); err != nil {
		t.Fatal(err)
	}
	return &Producer{db: database, inflight: map[string]uint{}}
}

func TestNextFissionEmailUsesPlusAliasForRegularProviders(t *testing.T) {
	producer := aliasCandidateProducer(t)
	email := producer.nextFissionEmail(models.Mailbox{Email: "user@hotmail.com"})
	if !regexp.MustCompile(`^user\+[a-z]{8}@hotmail\.com$`).MatchString(email) {
		t.Fatalf("email=%q", email)
	}
}

func TestNextFissionEmailUsesLocalPartForMailCom(t *testing.T) {
	producer := aliasCandidateProducer(t)
	for _, test := range []struct {
		base    string
		pattern string
	}{
		{"user@mail.com", `^user[a-z]{3}[0-9]{3}@mail\.com$`},
		{"user@musician.org", `^user[a-z]{3}[0-9]{3}@musician\.org$`},
	} {
		email := producer.nextFissionEmail(models.Mailbox{Email: test.base, Provider: mailcom.Provider})
		if strings.Contains(email, "+") {
			t.Fatalf("mail.com alias must not use plus syntax: %q", email)
		}
		if !regexp.MustCompile(test.pattern).MatchString(email) {
			t.Fatalf("email=%q", email)
		}
	}
}

func TestEnsureRemoteAliasSkipsPlusAliasProviders(t *testing.T) {
	called := false
	producer := &Producer{mail: fakeMailClient{createAlias: func(context.Context, mailfetch.Account, string) error {
		called = true
		return nil
	}}}
	mailbox := models.Mailbox{Email: "user@hotmail.com", ClientID: "id", RefreshToken: "token"}
	if err := producer.ensureRemoteAlias(context.Background(), mailbox, "user+abcdefgh@hotmail.com"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("plus alias provider must not call remote alias creation")
	}
}

// mailComAliasProducer 构造一个带 mail.com secret 设置的 Producer 和对应邮箱。
// created 收集远端别名创建调用，createErr 用于模拟远端失败。
func mailComAliasProducer(t *testing.T, createErr error) (*Producer, models.Mailbox, *[]string) {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Setting{}, &models.Mailbox{}, &models.Registration{}); err != nil {
		t.Fatal(err)
	}
	if err := database.Save(&models.Setting{Key: "mail_com_oauth_public_secret", Value: "public-secret"}).Error; err != nil {
		t.Fatal(err)
	}
	mailbox := models.Mailbox{Email: "user@mail.com", Password: "mail-password", Provider: mailcom.Provider, Status: "verified"}
	if err := database.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	created := &[]string{}
	producer := &Producer{db: database, inflight: map[string]uint{}, mail: fakeMailClient{
		createAlias: func(_ context.Context, account mailfetch.Account, address string) error {
			if account.MailComOAuthPublicSecret != "public-secret" {
				t.Fatalf("secret=%q", account.MailComOAuthPublicSecret)
			}
			*created = append(*created, address)
			return createErr
		},
	}}
	return producer, mailbox, created
}

func TestEnsureRemoteAliasCreatesMailComAddress(t *testing.T) {
	producer, mailbox, created := mailComAliasProducer(t, nil)
	if err := producer.ensureRemoteAlias(context.Background(), mailbox, "userabc123@mail.com"); err != nil {
		t.Fatal(err)
	}
	if len(*created) != 1 || (*created)[0] != "userabc123@mail.com" {
		t.Fatalf("created=%v", *created)
	}
}

func TestEnsureRemoteAliasMarksQuotaExhausted(t *testing.T) {
	producer, mailbox, _ := mailComAliasProducer(t, fmt.Errorf("add alias: %w", mailcom.ErrAliasLimit))
	err := producer.ensureRemoteAlias(context.Background(), mailbox, "userabc123@mail.com")
	if !errors.Is(err, mailcom.ErrAliasLimit) {
		t.Fatalf("err=%v", err)
	}
	if !producer.aliasQuotaFull(mailbox.ID) {
		t.Fatal("expected alias quota to be marked exhausted")
	}
}

func TestEnsureRemoteAliasPropagatesOtherErrors(t *testing.T) {
	producer, mailbox, _ := mailComAliasProducer(t, fmt.Errorf("add alias: %w", mailcom.ErrSettings))
	err := producer.ensureRemoteAlias(context.Background(), mailbox, "userabc123@mail.com")
	if !errors.Is(err, mailcom.ErrSettings) {
		t.Fatalf("err=%v", err)
	}
	if producer.aliasQuotaFull(mailbox.ID) {
		t.Fatal("transient settings failure must not exhaust alias quota")
	}
}
