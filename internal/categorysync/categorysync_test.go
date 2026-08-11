package categorysync

import (
	"testing"

	"chatgpt-register/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestAccountCategoryIDCreatesAndReusesMatchingAccountCategory(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Category{}); err != nil {
		t.Fatal(err)
	}
	mailboxCategory := models.Category{Scope: "mailbox", Name: "Plus 邮箱"}
	if err := database.Create(&mailboxCategory).Error; err != nil {
		t.Fatal(err)
	}
	firstID, err := AccountCategoryID(database, &mailboxCategory.ID)
	if err != nil || firstID == nil {
		t.Fatalf("first id=%v error=%v", firstID, err)
	}
	secondID, err := AccountCategoryID(database, &mailboxCategory.ID)
	if err != nil || secondID == nil || *secondID != *firstID {
		t.Fatalf("second id=%v error=%v", secondID, err)
	}
	var category models.Category
	if err := database.First(&category, *firstID).Error; err != nil {
		t.Fatal(err)
	}
	if category.Scope != "account" || category.Name != mailboxCategory.Name {
		t.Fatalf("category=%+v", category)
	}
	var count int64
	if err := database.Model(&models.Category{}).Where("scope = ? AND name = ?", "account", mailboxCategory.Name).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("count=%d error=%v", count, err)
	}
}

func TestBackfillRegistrationsUsesLinkedMailboxCategory(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Category{}, &models.Mailbox{}, &models.Registration{}); err != nil {
		t.Fatal(err)
	}
	mailboxCategory := models.Category{Scope: "mailbox", Name: "批量一组"}
	if err := database.Create(&mailboxCategory).Error; err != nil {
		t.Fatal(err)
	}
	mailbox := models.Mailbox{Email: "source@example.test", Status: "verified", CategoryID: &mailboxCategory.ID}
	if err := database.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	registration := models.Registration{Email: "source+account@example.test", MailboxID: mailbox.ID, Status: "registered"}
	if err := database.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}

	BackfillRegistrations(database)
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
