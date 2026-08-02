package emailalias

import (
	"math/rand/v2"
	"strconv"
	"strings"
)

func Base(address string) string {
	address = strings.TrimSpace(address)
	at := strings.LastIndex(address, "@")
	if at <= 0 {
		return address
	}
	local := address[:at]
	if plus := strings.Index(local, "+"); plus > 0 {
		return local[:plus] + address[at:]
	}
	dash := strings.LastIndex(local, "-")
	if dash < 1 {
		return address
	}
	if _, err := strconv.Atoi(local[dash+1:]); err != nil {
		return address
	}
	return local[:dash] + address[at:]
}

func Address(base string, suffix string) string {
	base = strings.TrimSpace(base)
	at := strings.LastIndex(base, "@")
	if at <= 0 {
		return base
	}
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return base
	}
	return base[:at] + "+" + suffix + base[at:]
}

func RandomSuffix(length int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	result := make([]byte, length)
	for i := range result {
		result[i] = letters[rand.IntN(len(letters))]
	}
	return string(result)
}

func LikePatterns(base string) []string {
	base = strings.TrimSpace(base)
	at := strings.LastIndex(base, "@")
	if at <= 0 {
		return nil
	}
	local := escapeLike(base[:at])
	domain := escapeLike(base[at:])
	return []string{local + "+%" + domain, local + "-%" + domain}
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
