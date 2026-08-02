package codexoauth

import (
	"testing"
	"time"
)

func TestNormalizeAndParseProxy(t *testing.T) {
	normalized := normalizeProxy("proxy.example.test:8080:user:p@ss")
	parts, err := urlParts(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if parts.server != "http://proxy.example.test:8080" || parts.user != "user" || parts.password != "p@ss" {
		t.Fatalf("parts=%+v", parts)
	}
	for _, raw := range []string{"http://proxy.example.test:8080", "socks5://user:pass@proxy.example.test:1080"} {
		if _, err := proxyHTTPClient(raw, time.Second); err != nil {
			t.Fatalf("proxyHTTPClient(%q) error=%v", raw, err)
		}
	}
	if _, err := proxyHTTPClient("ftp://proxy.example.test", time.Second); err == nil {
		t.Fatal("unsupported proxy returned nil error")
	}
}
