package mailcom

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

const maxOAuthConfigAssets = 32

var (
	oauthAssetRE  = regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["']([^"']+\.js(?:\?[^"']*)?)["']`)
	oauthImportRE = regexp.MustCompile(`(?i)(?:from\s*|import\s*)["']([^"']+\.js(?:\?[^"']*)?)["']`)
	oauthSecretRE = regexp.MustCompile(`(?i)clientSecret\s*:\s*["']([^"']+)["']`)
	oauthSecrets  = struct {
		sync.Mutex
		entries map[string]*oauthSecretEntry
	}{entries: make(map[string]*oauthSecretEntry)}
)

type oauthSecretEntry struct {
	ready  chan struct{}
	secret string
	err    error
}

func (c *Client) oauthPublicSecret(ctx context.Context, refresh bool) (string, error) {
	if secret := strings.TrimSpace(c.config.OAuthPublicSecret); secret != "" {
		return secret, nil
	}
	if refresh {
		invalidateOAuthPublicSecret(c.endpoints.OAuthConfigURL)
	}
	return cachedOAuthPublicSecret(ctx, c.http, c.endpoints.OAuthConfigURL)
}

func cachedOAuthPublicSecret(ctx context.Context, client *http.Client, configURL string) (string, error) {
	key := strings.TrimSpace(configURL)
	oauthSecrets.Lock()
	if entry := oauthSecrets.entries[key]; entry != nil {
		oauthSecrets.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-entry.ready:
			return entry.secret, entry.err
		}
	}
	entry := &oauthSecretEntry{ready: make(chan struct{})}
	oauthSecrets.entries[key] = entry
	oauthSecrets.Unlock()

	entry.secret, entry.err = discoverOAuthPublicSecret(ctx, client, key)
	close(entry.ready)
	if entry.err != nil {
		oauthSecrets.Lock()
		if oauthSecrets.entries[key] == entry {
			delete(oauthSecrets.entries, key)
		}
		oauthSecrets.Unlock()
	}
	return entry.secret, entry.err
}

func invalidateOAuthPublicSecret(configURL string) {
	oauthSecrets.Lock()
	delete(oauthSecrets.entries, strings.TrimSpace(configURL))
	oauthSecrets.Unlock()
}

func discoverOAuthPublicSecret(ctx context.Context, client *http.Client, configURL string) (string, error) {
	root, err := url.Parse(configURL)
	if err != nil || root.Scheme == "" || root.Host == "" {
		return "", fmt.Errorf("%w: OAuth config URL", ErrInvalidConfig)
	}
	queue := []string{root.String()}
	seen := make(map[string]struct{})
	for len(queue) > 0 && len(seen) < maxOAuthConfigAssets {
		current := queue[0]
		queue = queue[1:]
		if _, exists := seen[current]; exists {
			continue
		}
		seen[current] = struct{}{}
		body, err := fetchOAuthConfigAsset(ctx, client, current)
		if err != nil {
			if current == root.String() {
				return "", err
			}
			continue
		}
		if secret := extractOAuthPublicSecret(body); secret != "" {
			return secret, nil
		}
		base, _ := url.Parse(current)
		for _, pattern := range []*regexp.Regexp{oauthAssetRE, oauthImportRE} {
			for _, match := range pattern.FindAllStringSubmatch(body, -1) {
				asset, resolveErr := base.Parse(html.UnescapeString(match[1]))
				if resolveErr == nil && asset.Scheme == root.Scheme && asset.Host == root.Host {
					queue = append(queue, asset.String())
				}
			}
		}
	}
	return "", fmt.Errorf("%w: mail.com OAuth public secret 自动发现失败", ErrInvalidConfig)
}

func fetchOAuthConfigAsset(ctx context.Context, client *http.Client, assetURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: create OAuth config request", ErrRequest)
	}
	req.Header.Set("Accept", "text/html,application/javascript,*/*")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: OAuth config request: %v", ErrRequest, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: OAuth config status=%d", ErrRequest, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("%w: read OAuth config: %v", ErrRequest, err)
	}
	return string(body), nil
}

func extractOAuthPublicSecret(source string) string {
	for _, match := range oauthSecretRE.FindAllStringSubmatch(source, -1) {
		secret := strings.TrimSpace(match[1])
		if len(secret) >= 8 && strings.Trim(secret, "*") != "" {
			return secret
		}
	}
	return ""
}
