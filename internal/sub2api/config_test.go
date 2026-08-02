package sub2api

import (
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		URL:         "https://sub2api.example.test/",
		APIKey:      " synthetic-admin-key ",
		GroupIDs:    []int{12, 13, 12},
		Concurrency: 10,
		Priority:    1,
		Timeout:     30,
	}
}

func TestConfigValidateNormalizes(t *testing.T) {
	config := Config{
		URL:         " https://sub2api.example.test/// ",
		APIKey:      " key ",
		GroupIDs:    []int{13, 12, 13},
		Concurrency: 10,
		Priority:    1,
		Timeout:     60,
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if config.URL != "https://sub2api.example.test" || config.APIKey != "key" {
		t.Fatalf("Validate() normalized config = %+v", config)
	}
	if got := config.GroupIDs; len(got) != 2 || got[0] != 13 || got[1] != 12 {
		t.Fatalf("Validate() GroupIDs = %v", got)
	}
}

func TestConfigValidateRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"URL scheme", func(c *Config) { c.URL = "ftp://example.test" }, "HTTP 或 HTTPS"},
		{"URL host", func(c *Config) { c.URL = "https:///path" }, "HTTP 或 HTTPS"},
		{"URL userinfo", func(c *Config) { c.URL = "https://name:secret@example.test" }, "认证信息"},
		{"URL query", func(c *Config) { c.URL = "https://example.test?token=value" }, "查询参数"},
		{"URL fragment", func(c *Config) { c.URL = "https://example.test/#fragment" }, "查询参数"},
		{"URL malformed", func(c *Config) { c.URL = "%" }, "HTTP 或 HTTPS"},
		{"API key", func(c *Config) { c.APIKey = "  " }, "Admin Key"},
		{"group zero", func(c *Config) { c.GroupIDs = []int{0} }, "大于 0"},
		{"concurrency zero", func(c *Config) { c.Concurrency = 0 }, "1 到 100"},
		{"concurrency low", func(c *Config) { c.Concurrency = -1 }, "1 到 100"},
		{"concurrency high", func(c *Config) { c.Concurrency = 101 }, "1 到 100"},
		{"priority zero", func(c *Config) { c.Priority = 0 }, "1 到 100"},
		{"priority low", func(c *Config) { c.Priority = -1 }, "1 到 100"},
		{"priority high", func(c *Config) { c.Priority = 101 }, "1 到 100"},
		{"timeout zero", func(c *Config) { c.Timeout = 0 }, "5 到 300"},
		{"timeout low", func(c *Config) { c.Timeout = 4 }, "5 到 300"},
		{"timeout high", func(c *Config) { c.Timeout = 301 }, "5 到 300"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			test.mutate(&config)
			err := config.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseGroupIDs(t *testing.T) {
	got, err := ParseGroupIDs(" 12,13;12,, 14 ")
	if err != nil {
		t.Fatalf("ParseGroupIDs() error = %v", err)
	}
	if len(got) != 3 || got[0] != 12 || got[1] != 13 || got[2] != 14 {
		t.Fatalf("ParseGroupIDs() = %v", got)
	}
	for _, input := range []string{"x", "-1"} {
		if _, err := ParseGroupIDs(input); err == nil {
			t.Fatalf("ParseGroupIDs(%q) returned nil error", input)
		}
	}
}
