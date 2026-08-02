package sub2api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

type Error struct {
	message string
}

func (e *Error) Error() string {
	return e.message
}

func newError(message string) error {
	return &Error{message: message}
}

var (
	jwtPattern     = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	refreshPattern = regexp.MustCompile(`\brt\.[A-Za-z0-9._-]+`)
	bearerPattern  = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._-]+`)
	emailPattern   = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
)

func Redact(value any, secrets ...string) string {
	text := fmt.Sprint(value)
	filtered := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" {
			filtered = append(filtered, secret)
		}
	}
	sort.SliceStable(filtered, func(left, right int) bool { return len(filtered[left]) > len(filtered[right]) })
	for _, secret := range filtered {
		variants := []string{secret, url.QueryEscape(secret)}
		for _, variant := range variants {
			text = strings.ReplaceAll(text, variant, "<redacted>")
		}
	}
	text = jwtPattern.ReplaceAllString(text, "<jwt>")
	text = refreshPattern.ReplaceAllString(text, "<refresh_token>")
	text = bearerPattern.ReplaceAllString(text, "Bearer <redacted>")
	text = emailPattern.ReplaceAllString(text, "<email>")
	if len(text) > 500 {
		text = text[:500]
	}
	return text
}

func sensitiveValues(value any) []string {
	values := make([]string, 0, 4)
	visitSensitive(reflect.ValueOf(value), "", &values)
	return values
}

func visitSensitive(value reflect.Value, key string, found *[]string) {
	if !value.IsValid() {
		return
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
		visitSensitive(value.Elem(), key, found)
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		typeOf := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := typeOf.Field(i)
			childKey := strings.Split(field.Tag.Get("json"), ",")[0]
			if childKey == "" {
				childKey = field.Name
			}
			visitSensitive(value.Field(i), strings.ToLower(childKey), found)
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			childKey := fmt.Sprint(iterator.Key().Interface())
			visitSensitive(iterator.Value(), strings.ToLower(childKey), found)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			visitSensitive(value.Index(i), key, found)
		}
	case reflect.String:
		if isSensitiveKey(key) {
			if text := value.String(); text != "" {
				*found = append(*found, text)
			}
		}
	default:
		if isSensitiveKey(key) && value.CanInterface() {
			if text := fmt.Sprint(value.Interface()); text != "" {
				*found = append(*found, text)
			}
		}
	}
}

func isSensitiveKey(key string) bool {
	switch key {
	case "access_token", "refresh_token", "id_token", "api_key":
		return true
	default:
		return false
	}
}

func wrapRedacted(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	return newError(Redact(err.Error(), secrets...))
}

func errorDetail(body json.RawMessage) string {
	var object map[string]any
	if json.Unmarshal(body, &object) == nil {
		for _, key := range []string{"message", "msg", "error"} {
			if value, ok := object[key]; ok && value != nil {
				return fmt.Sprint(value)
			}
		}
	}
	return string(body)
}
