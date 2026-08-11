package categorysync

import (
	"chatgpt-register/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func AccountCategoryID(db *gorm.DB, mailboxCategoryID *uint) (*uint, error) {
	if mailboxCategoryID == nil {
		return nil, nil
	}
	var mailboxCategory models.Category
	if err := db.Where("id = ? AND scope = ?", *mailboxCategoryID, "mailbox").First(&mailboxCategory).Error; err != nil {
		return nil, err
	}
	accountCategory := models.Category{Scope: "account", Name: mailboxCategory.Name}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&accountCategory).Error; err != nil {
		return nil, err
	}
	if accountCategory.ID == 0 {
		if err := db.Where("scope = ? AND name = ?", "account", mailboxCategory.Name).First(&accountCategory).Error; err != nil {
			return nil, err
		}
	}
	return &accountCategory.ID, nil
}

func BackfillRegistrations(db *gorm.DB) {
	var registrations []models.Registration
	if err := db.Where("mailbox_id <> 0 AND category_id IS NULL").Find(&registrations).Error; err != nil {
		return
	}
	for _, registration := range registrations {
		var mailbox models.Mailbox
		if err := db.Select("id", "category_id").First(&mailbox, registration.MailboxID).Error; err != nil || mailbox.CategoryID == nil {
			continue
		}
		categoryID, err := AccountCategoryID(db, mailbox.CategoryID)
		if err == nil && categoryID != nil {
			db.Model(&models.Registration{}).Where("id = ? AND category_id IS NULL", registration.ID).Update("category_id", *categoryID)
		}
	}
}
