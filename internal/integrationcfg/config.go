package integrationcfg

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"chatgpt-register/internal/models"
	"chatgpt-register/internal/smsactivate"
	"chatgpt-register/internal/sub2api"

	"gorm.io/gorm"
)

const (
	defaultSMSCountries     = "187,16,36,43"
	defaultSMSMaxPrice      = 0.5
	defaultSMSTimeout       = 180
	DefaultSMSPhoneAttempts = 3
	MaximumSMSPhoneAttempts = 10
)

func DefaultSMSRandomCountries() string {
	return defaultSMSCountries
}

type Values map[string]string

type SMSConfig struct {
	Client           smsactivate.Config
	PollTimeout      time.Duration
	PollInterval     time.Duration
	MaxPhoneAttempts int
}

func Load(db *gorm.DB) (Values, error) {
	var settings []models.Setting
	if err := db.Find(&settings).Error; err != nil {
		return nil, err
	}
	values := make(Values, len(settings))
	for _, setting := range settings {
		values[setting.Key] = setting.Value
	}
	return values, nil
}

func (v Values) CodexAutoAuthorize() bool {
	return strings.TrimSpace(v["codex_auto_authorize"]) == "1"
}

func (v Values) Sub2APIAutoImport() bool {
	return strings.TrimSpace(v["sub2api_auto_import"]) == "1"
}

func (v Values) SMS() (SMSConfig, error) {
	platform := smsactivate.Platform(strings.TrimSpace(v["sms_platform"]))
	if platform == "" {
		platform = smsactivate.PlatformHeroSMS
	}
	countryValue := strings.TrimSpace(v["sms_country"])
	if countryValue == "" {
		countryValue = "random"
	}
	clientConfig := smsactivate.Config{
		Platform: platform,
		APIKey:   strings.TrimSpace(v["sms_api_key"]),
		MaxPrice: defaultSMSMaxPrice,
		Timeout:  15 * time.Second,
	}
	if countryValue == "random" {
		countries, err := parseIDs(defaultValue(v["sms_random_countries"], defaultSMSCountries))
		if err != nil {
			return SMSConfig{}, fmt.Errorf("随机候选国家配置错误: %w", err)
		}
		clientConfig.RandomCountries = countries
	} else {
		country, err := strconv.Atoi(countryValue)
		if err != nil {
			return SMSConfig{}, fmt.Errorf("接码国家必须是 random 或数字 ID")
		}
		clientConfig.Country = country
	}
	if value := strings.TrimSpace(v["sms_max_price"]); value != "" {
		price, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return SMSConfig{}, fmt.Errorf("最高单价必须是数字")
		}
		clientConfig.MaxPrice = price
	}
	seconds, err := boundedInt(v["sms_timeout"], defaultSMSTimeout, 30, 600, "收码超时")
	if err != nil {
		return SMSConfig{}, err
	}
	attempts, err := boundedInt(v["sms_phone_attempts"], DefaultSMSPhoneAttempts, 1, MaximumSMSPhoneAttempts, "号码尝试上限")
	if err != nil {
		return SMSConfig{}, err
	}
	if err := clientConfig.Validate(); err != nil {
		return SMSConfig{}, err
	}
	return SMSConfig{
		Client: clientConfig, PollTimeout: time.Duration(seconds) * time.Second,
		PollInterval: 5 * time.Second, MaxPhoneAttempts: attempts,
	}, nil
}

func (v Values) Sub2API(overrides ...string) (sub2api.Config, error) {
	baseURL := strings.TrimSpace(v["sub2api_url"])
	apiKey := strings.TrimSpace(v["sub2api_api_key"])
	if len(overrides) > 0 && strings.TrimSpace(overrides[0]) != "" {
		baseURL = strings.TrimSpace(overrides[0])
	}
	if len(overrides) > 1 && strings.TrimSpace(overrides[1]) != "" {
		apiKey = strings.TrimSpace(overrides[1])
	}
	groupIDs, err := sub2api.ParseGroupIDs(v["sub2api_group_ids"])
	if err != nil {
		return sub2api.Config{}, err
	}
	concurrency, err := boundedInt(v["sub2api_concurrency"], sub2api.DefaultConcurrency, 1, 100, "Sub2API 并发数")
	if err != nil {
		return sub2api.Config{}, err
	}
	priority, err := boundedInt(v["sub2api_priority"], sub2api.DefaultPriority, 1, 100, "Sub2API 优先级")
	if err != nil {
		return sub2api.Config{}, err
	}
	timeout, err := boundedInt(v["sub2api_timeout"], sub2api.DefaultTimeout, 5, 300, "Sub2API 超时")
	if err != nil {
		return sub2api.Config{}, err
	}
	config := sub2api.Config{
		URL: baseURL, APIKey: apiKey, GroupIDs: groupIDs,
		Concurrency: concurrency, Priority: priority, Timeout: timeout,
	}
	if err := config.Validate(); err != nil {
		return sub2api.Config{}, err
	}
	return config, nil
}

func defaultValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func parseIDs(value string) ([]int, error) {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' })
	ids := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		id, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("国家 ID 必须是正整数")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("候选列表为空")
	}
	return ids, nil
}

func boundedInt(value string, fallback, minimum, maximum int, label string) (int, error) {
	parsed := fallback
	if strings.TrimSpace(value) != "" {
		var err error
		parsed, err = strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, fmt.Errorf("%s必须是整数", label)
		}
	}
	if parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("%s必须在 %d 到 %d 之间", label, minimum, maximum)
	}
	return parsed, nil
}
