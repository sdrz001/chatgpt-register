package smsactivate

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Platform string

const (
	PlatformHeroSMS  Platform = "hero-sms"
	PlatformSMSBower Platform = "smsbower"

	HeroSMSEndpoint  = "https://hero-sms.com/stubs/handler_api.php"
	SMSBowerEndpoint = "https://smsbower.page/stubs/handler_api.php"
	ServiceOpenAI    = "dr"
	MaxPriceLimit    = 5.0
)

type Config struct {
	Platform        Platform
	APIKey          string
	Country         int
	RandomCountries []int
	MaxPrice        float64
	Timeout         time.Duration
}

func (c Config) Validate() error {
	if _, err := endpointFor(c.Platform); err != nil {
		return err
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return configError("API Key 为空")
	}
	if c.Country < 0 {
		return configError("国家代码须为正整数")
	}
	if c.Country > 0 && !countryExists(c.Country) {
		return configError("不支持的国家代码")
	}
	fixed := c.Country > 0
	random := len(c.RandomCountries) > 0
	if fixed == random {
		return configError("请设置一个固定国家或一组随机候选国家")
	}
	for _, country := range c.RandomCountries {
		if country <= 0 {
			return configError("随机候选国家代码须为正整数")
		}
		if !countryExists(country) {
			return configError("随机候选列表包含不支持的国家代码")
		}
	}
	if math.IsNaN(c.MaxPrice) || math.IsInf(c.MaxPrice, 0) || c.MaxPrice <= 0 {
		return configError("最高单价须为大于 0 的数字")
	}
	if c.MaxPrice > MaxPriceLimit {
		return configError(fmt.Sprintf("最高单价上限为 $%.2f", MaxPriceLimit))
	}
	if c.Timeout <= 0 {
		return configError("请求超时须大于 0")
	}
	return nil
}

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type Option func(*clientOptions) error

type clientOptions struct {
	httpClient HTTPClient
	endpoint   string
}

func WithHTTPClient(client HTTPClient) Option {
	return func(options *clientOptions) error {
		if client == nil {
			return configError("HTTP client 为空")
		}
		options.httpClient = client
		return nil
	}
}

func WithEndpoint(endpoint string) Option {
	return func(options *clientOptions) error {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			return configError("endpoint 为空")
		}
		options.endpoint = endpoint
		return nil
	}
}

func endpointFor(platform Platform) (string, error) {
	switch platform {
	case PlatformHeroSMS:
		return HeroSMSEndpoint, nil
	case PlatformSMSBower:
		return SMSBowerEndpoint, nil
	default:
		return "", configError("接码平台须为 hero-sms 或 smsbower")
	}
}

func parseEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, configError("endpoint 格式错误")
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, configError("endpoint 须使用 HTTP 或 HTTPS")
	}
	return endpoint, nil
}

func normalizedCountries(countries []int) []int {
	result := make([]int, 0, len(countries))
	seen := make(map[int]struct{}, len(countries))
	for _, country := range countries {
		if _, ok := seen[country]; ok {
			continue
		}
		seen[country] = struct{}{}
		result = append(result, country)
	}
	return result
}
