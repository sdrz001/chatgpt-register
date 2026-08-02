package sub2api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func accountItems(body json.RawMessage) []map[string]any {
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		return nil
	}
	if object, ok := decoded.(map[string]any); ok {
		if data, exists := object["data"]; exists {
			decoded = data
		}
	}
	if object, ok := decoded.(map[string]any); ok {
		for _, key := range []string{"items", "list", "data"} {
			if value := object[key]; value != nil {
				decoded = value
				break
			}
		}
	}
	items, ok := decoded.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

func dataObject(body json.RawMessage) map[string]any {
	var object map[string]any
	if json.Unmarshal(body, &object) != nil {
		return map[string]any{}
	}
	if data, ok := object["data"].(map[string]any); ok {
		return data
	}
	return object
}

func accountEmail(account map[string]any) string {
	for _, key := range []string{"credentials", "extra"} {
		if object, ok := account[key].(map[string]any); ok {
			if email := strings.TrimSpace(stringValue(object["email"])); email != "" {
				return email
			}
		}
	}
	return strings.TrimSpace(stringValue(account["name"]))
}

func accountGroupIDs(account map[string]any) ([]int, error) {
	if value, exists := account["group_ids"]; exists && value != nil && stringValue(value) != "" {
		return groupIDsFromAny(value)
	}
	groups, ok := account["groups"].([]any)
	if !ok {
		return []int{}, nil
	}
	values := make([]any, 0, len(groups))
	for _, group := range groups {
		if object, ok := group.(map[string]any); ok {
			values = append(values, object["id"])
		} else {
			values = append(values, group)
		}
	}
	return groupIDsFromAny(values)
}

func groupIDsFromAny(value any) ([]int, error) {
	var values []any
	switch typed := value.(type) {
	case []any:
		values = typed
	case []int:
		values = make([]any, len(typed))
		for index, id := range typed {
			values[index] = id
		}
	case string:
		return ParseGroupIDs(typed)
	default:
		values = []any{typed}
	}
	ids := make([]int, 0, len(values))
	for _, item := range values {
		id, ok := anyInt(item)
		if !ok {
			return nil, newError("Sub2API 分类 ID 必须是整数")
		}
		ids = append(ids, id)
	}
	return normalizeGroupIDs(ids)
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func positiveInt(value any) (int, bool) {
	parsed, ok := anyInt(value)
	return parsed, ok && parsed > 0
}

func anyInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case uint:
		return int(typed), true
	case uint8:
		return int(typed), true
	case uint16:
		return int(typed), true
	case uint32:
		return int(typed), true
	case uint64:
		return int(typed), true
	case float64:
		if typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return 0, false
	}
}

func containsAll(values, required []int) bool {
	set := make(map[int]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	for _, value := range required {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}
