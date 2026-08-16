package domainmail

import (
	"encoding/json"
	"strings"
	"time"
)

func unwrapObject(value any) any {
	current := value
	for range 4 {
		object, ok := current.(map[string]any)
		if !ok {
			break
		}
		advanced := false
		for _, key := range []string{"data", "result", "email", "mailbox", "message"} {
			nested, exists := object[key]
			if !exists {
				continue
			}
			switch nested.(type) {
			case map[string]any, []any:
				current = nested
				advanced = true
			}
			if advanced {
				break
			}
		}
		if !advanced {
			break
		}
	}
	return current
}

func stringField(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, exists := object[key]; exists {
			if text := scalarString(value); text != "" {
				return text
			}
		}
	}
	return ""
}

func decodeMailbox(value any) (Mailbox, bool) {
	object, ok := unwrapObject(value).(map[string]any)
	if !ok {
		return Mailbox{}, false
	}
	mailbox := Mailbox{
		ID:     stringField(object, "emailId", "email_id", "mailboxId", "mailbox_id", "id", "_id"),
		Email:  stringField(object, "email", "address", "emailAddress", "email_address"),
		Name:   stringField(object, "name", "localPart", "local_part", "username"),
		Domain: stringField(object, "domain"),
	}
	if mailbox.Email == "" && mailbox.Name != "" && mailbox.Domain != "" {
		mailbox.Email = mailbox.Name + "@" + mailbox.Domain
	}
	if mailbox.Name == "" || mailbox.Domain == "" {
		parts := strings.Split(mailbox.Email, "@")
		if len(parts) == 2 {
			if mailbox.Name == "" {
				mailbox.Name = parts[0]
			}
			if mailbox.Domain == "" {
				mailbox.Domain = parts[1]
			}
		}
	}
	return mailbox, mailbox.ID != "" && strings.Contains(mailbox.Email, "@")
}

func decodeMailboxes(value any) []Mailbox {
	array := findArray(value, "emails", "mailboxes", "items", "list", "records")
	out := make([]Mailbox, 0, len(array))
	seen := map[string]struct{}{}
	for _, item := range array {
		mailbox, ok := decodeMailbox(item)
		if !ok {
			continue
		}
		if _, exists := seen[mailbox.ID]; exists {
			continue
		}
		seen[mailbox.ID] = struct{}{}
		out = append(out, mailbox)
	}
	return out
}

func decodeMessage(value any) (Message, bool) {
	object, ok := unwrapObject(value).(map[string]any)
	if !ok {
		return Message{}, false
	}
	from, fromName := decodeSender(object)
	message := Message{
		ID:         stringField(object, "messageId", "message_id", "mailId", "mail_id", "id", "_id"),
		From:       from,
		FromName:   fromName,
		Subject:    stringField(object, "subject", "title"),
		ReceivedAt: decodeTime(firstValue(object, "receivedAt", "received_at", "createdAt", "created_at", "date", "timestamp", "time")),
		HTML:       stringField(object, "html", "htmlContent", "html_content", "bodyHtml", "body_html"),
		Text:       stringField(object, "text", "textContent", "text_content", "bodyText", "body_text", "content", "body"),
	}
	if nested, exists := object["body"].(map[string]any); exists {
		if message.HTML == "" {
			message.HTML = stringField(nested, "html", "content")
		}
		if message.Text == "" {
			message.Text = stringField(nested, "text", "plain")
		}
	}
	return message, message.ID != ""
}

func decodeMessages(value any) []Message {
	array := findArray(value, "messages", "mails", "items", "list", "records")
	out := make([]Message, 0, len(array))
	seen := map[string]struct{}{}
	for _, item := range array {
		message, ok := decodeMessage(item)
		if !ok {
			continue
		}
		if _, exists := seen[message.ID]; exists {
			continue
		}
		seen[message.ID] = struct{}{}
		out = append(out, message)
	}
	return out
}

func decodeSender(object map[string]any) (string, string) {
	from := firstValue(object, "from", "sender", "fromAddress", "from_address")
	switch typed := from.(type) {
	case string:
		return strings.TrimSpace(typed), stringField(object, "fromName", "from_name", "senderName", "sender_name")
	case map[string]any:
		address := stringField(typed, "email", "address", "emailAddress", "email_address")
		name := stringField(typed, "name", "displayName", "display_name")
		return address, name
	default:
		return stringField(object, "fromEmail", "from_email", "senderEmail", "sender_email"), stringField(object, "fromName", "from_name", "senderName", "sender_name")
	}
}

func decodeDomains(value any) []string {
	values := findDomainValues(value, 0)
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, item := range values {
		candidates := []string{scalarString(item)}
		if object, ok := item.(map[string]any); ok {
			candidates = []string{stringField(object, "domain", "name", "value")}
		}
		for _, candidate := range candidates {
			for _, domain := range strings.Split(candidate, ",") {
				domain = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(domain, "@")))
				if domain == "" || strings.ContainsAny(domain, " /@") {
					continue
				}
				if _, exists := seen[domain]; exists {
					continue
				}
				seen[domain] = struct{}{}
				out = append(out, domain)
			}
		}
	}
	return out
}

func findDomainValues(value any, depth int) []any {
	if depth > 5 {
		return nil
	}
	switch typed := value.(type) {
	case []any:
		return typed
	case string:
		return []any{typed}
	case map[string]any:
		var values []any
		for _, key := range []string{"emailDomains", "email_domains", "domains", "availableDomains", "available_domains", "items"} {
			if nested, exists := typed[key]; exists {
				values = append(values, findDomainValues(nested, depth+1)...)
			}
		}
		if len(values) > 0 {
			return values
		}
		for _, key := range []string{"data", "result", "config", "settings"} {
			if nested, exists := typed[key]; exists {
				if values := findDomainValues(nested, depth+1); len(values) > 0 {
					return values
				}
			}
		}
	}
	return nil
}

func decodeNextCursor(value any) string {
	for depth := 0; depth < 5; depth++ {
		object, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		if cursor := stringField(object, "nextCursor", "next_cursor", "cursor"); cursor != "" {
			return cursor
		}
		advanced := false
		for _, key := range []string{"pagination", "pageInfo", "page_info", "meta", "data", "result"} {
			if nested, exists := object[key].(map[string]any); exists {
				value = nested
				advanced = true
				break
			}
		}
		if !advanced {
			return ""
		}
	}
	return ""
}

func findArray(value any, keys ...string) []any {
	if array, ok := value.([]any); ok {
		return array
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range keys {
		if array, ok := object[key].([]any); ok {
			return array
		}
	}
	for _, key := range []string{"data", "result", "pagination"} {
		if nested, exists := object[key]; exists {
			if array := findArray(nested, keys...); len(array) > 0 {
				return array
			}
		}
	}
	return nil
}

func firstValue(object map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, exists := object[key]; exists {
			return value
		}
	}
	return nil
}

func decodeTime(value any) time.Time {
	text := scalarString(value)
	if text == "" {
		return time.Time{}
	}
	if number, err := json.Number(text).Int64(); err == nil {
		if number > 100000000000 {
			return time.UnixMilli(number)
		}
		if number > 1000000000 {
			return time.Unix(number, 0)
		}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
