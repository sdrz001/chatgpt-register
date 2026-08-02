package integrationcfg

import (
	"testing"
	"time"

	"chatgpt-register/internal/models"
	"chatgpt-register/internal/smsactivate"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestLoadAndBooleanSettings(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Setting{}); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"codex_auto_authorize": "1", "sub2api_auto_import": "0"} {
		if err := database.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	values, err := Load(database)
	if err != nil {
		t.Fatal(err)
	}
	if !values.CodexAutoAuthorize() || values.Sub2APIAutoImport() {
		t.Fatalf("boolean settings=%v", values)
	}
}

func TestSMSConfigDefaultsAndModes(t *testing.T) {
	values := Values{"sms_api_key": "key"}
	config, err := values.SMS()
	if err != nil {
		t.Fatal(err)
	}
	if config.Client.Platform != smsactivate.PlatformHeroSMS || config.Client.Country != 0 || len(config.Client.RandomCountries) != 4 {
		t.Fatalf("default SMS config=%+v", config)
	}
	if config.Client.MaxPrice != 0.5 || config.PollTimeout != 180*time.Second || config.PollInterval != 5*time.Second {
		t.Fatalf("default SMS timing=%+v", config)
	}

	values = Values{
		"sms_platform": "smsbower", "sms_api_key": "key", "sms_country": "16",
		"sms_max_price": "1.25", "sms_timeout": "90",
	}
	config, err = values.SMS()
	if err != nil {
		t.Fatal(err)
	}
	if config.Client.Platform != smsactivate.PlatformSMSBower || config.Client.Country != 16 || len(config.Client.RandomCountries) != 0 || config.Client.MaxPrice != 1.25 {
		t.Fatalf("fixed SMS config=%+v", config)
	}
}

func TestSMSConfigRejectsInvalidValues(t *testing.T) {
	for name, values := range map[string]Values{
		"key":        {},
		"country":    {"sms_api_key": "key", "sms_country": "bad"},
		"candidates": {"sms_api_key": "key", "sms_country": "random", "sms_random_countries": "x"},
		"price":      {"sms_api_key": "key", "sms_max_price": "6"},
		"timeout":    {"sms_api_key": "key", "sms_timeout": "10"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := values.SMS(); err == nil {
				t.Fatal("SMS returned nil error")
			}
		})
	}
}

func TestSub2APIConfigDefaultsAndOverrides(t *testing.T) {
	values := Values{
		"sub2api_url": "https://stored.example.test", "sub2api_api_key": "stored-key",
		"sub2api_group_ids": "3,2,3",
	}
	config, err := values.Sub2API("https://override.example.test", "new-key")
	if err != nil {
		t.Fatal(err)
	}
	if config.URL != "https://override.example.test" || config.APIKey != "new-key" || config.Concurrency != 10 || config.Priority != 1 || config.Timeout != 60 {
		t.Fatalf("config=%+v", config)
	}
	if len(config.GroupIDs) != 2 || config.GroupIDs[0] != 3 || config.GroupIDs[1] != 2 {
		t.Fatalf("groups=%v", config.GroupIDs)
	}
}
