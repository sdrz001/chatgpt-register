package emailalias

import (
	"regexp"
	"testing"
)

func TestAddress(t *testing.T) {
	got := Address("ldhbq73981352@hotmail.com", "abcdefgh")
	want := "ldhbq73981352+abcdefgh@hotmail.com"
	if got != want {
		t.Fatalf("Address() = %q, want %q", got, want)
	}
}

func TestRandomSuffix(t *testing.T) {
	got := RandomSuffix(8)
	if !regexp.MustCompile(`^[a-z]{8}$`).MatchString(got) {
		t.Fatalf("RandomSuffix() = %q, want 8 lowercase letters", got)
	}
}

func TestBase(t *testing.T) {
	tests := map[string]string{
		"ldhbq73981352+abcdefgh@hotmail.com": "ldhbq73981352@hotmail.com",
		"ldhbq73981352-001@hotmail.com":      "ldhbq73981352@hotmail.com",
		"ldhbq73981352@hotmail.com":          "ldhbq73981352@hotmail.com",
		"name-part@hotmail.com":              "name-part@hotmail.com",
	}
	for input, want := range tests {
		if got := Base(input); got != want {
			t.Errorf("Base(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLikePatterns(t *testing.T) {
	got := LikePatterns("ldhbq73981352@hotmail.com")
	if len(got) != 2 {
		t.Fatalf("LikePatterns() count = %d, want 2", len(got))
	}
	if got[0] != "ldhbq73981352+%@hotmail.com" {
		t.Errorf("plus pattern = %q", got[0])
	}
	if got[1] != "ldhbq73981352-%@hotmail.com" {
		t.Errorf("legacy pattern = %q", got[1])
	}
}
