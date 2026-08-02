package sub2api

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	DefaultConcurrency = 10
	DefaultPriority    = 1
	DefaultTimeout     = 60
)

type Config struct {
	URL         string `json:"url"`
	APIKey      string `json:"api_key"`
	GroupIDs    []int  `json:"group_ids"`
	Concurrency int    `json:"concurrency"`
	Priority    int    `json:"priority"`
	Timeout     int    `json:"timeout"`
}

func (c *Config) Validate() error {
	normalized, err := c.Validated()
	if err != nil {
		return err
	}
	*c = normalized
	return nil
}

func (c Config) Validated() (Config, error) {
	apiKey := strings.TrimSpace(c.APIKey)
	if apiKey == "" {
		return Config{}, newError("Sub2API Admin Key 未配置")
	}
	base, err := normalizeURL(c.URL)
	if err != nil {
		return Config{}, err
	}
	groupIDs, err := normalizeGroupIDs(c.GroupIDs)
	if err != nil {
		return Config{}, err
	}
	concurrency, err := boundedInt(c.Concurrency, 1, 100, "Sub2API 并发数")
	if err != nil {
		return Config{}, err
	}
	priority, err := boundedInt(c.Priority, 1, 100, "Sub2API 优先级")
	if err != nil {
		return Config{}, err
	}
	timeout, err := boundedInt(c.Timeout, 5, 300, "Sub2API 超时")
	if err != nil {
		return Config{}, err
	}
	return Config{
		URL:         base,
		APIKey:      apiKey,
		GroupIDs:    groupIDs,
		Concurrency: concurrency,
		Priority:    priority,
		Timeout:     timeout,
	}, nil
}

func normalizeURL(value string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed == nil {
		return "", newError("Sub2API 地址必须是完整的 HTTP 或 HTTPS URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "" || parsed.Host == "" || (scheme != "http" && scheme != "https") {
		return "", newError("Sub2API 地址必须是完整的 HTTP 或 HTTPS URL")
	}
	if parsed.User != nil {
		return "", newError("Sub2API 地址中不能包含认证信息")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", newError("Sub2API 地址中不能包含查询参数或片段")
	}
	return base, nil
}

func ParseGroupIDs(value string) ([]int, error) {
	if strings.TrimSpace(value) == "" {
		return []int{}, nil
	}
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' })
	ids := make([]int, 0, len(parts))
	for _, part := range parts {
		text := strings.TrimSpace(part)
		if text == "" {
			continue
		}
		id, err := strconv.Atoi(text)
		if err != nil {
			return nil, newError("Sub2API 分类 ID 必须是整数")
		}
		ids = append(ids, id)
	}
	return normalizeGroupIDs(ids)
}

func normalizeGroupIDs(ids []int) ([]int, error) {
	result := make([]int, 0, len(ids))
	seen := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, newError("Sub2API 分类 ID 必须大于 0")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result, nil
}

func boundedInt(value, minimum, maximum int, label string) (int, error) {
	if value < minimum || value > maximum {
		return 0, newError(fmt.Sprintf("%s必须在 %d 到 %d 之间", label, minimum, maximum))
	}
	return value, nil
}
