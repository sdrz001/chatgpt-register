package mailcom

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func aliasTestServer(t *testing.T, createStatus int, created *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var baseURL string
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", baseURL+"/halogin?ott=one-time-token")
		w.WriteHeader(http.StatusSeeOther)
	})
	mux.HandleFunc("/halogin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", baseURL+"/mailbox?sid=session-1")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/oauth", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("scope") == settingsScope {
			auth := r.Header.Get("Authorization")
			if !strings.Contains(auth, "Basic ") {
				t.Fatalf("settings token missing basic auth: %q", auth)
			}
			if origin := r.Header.Get("Origin"); origin != settingsOrigin {
				t.Fatalf("settings token origin=%q", origin)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "token-" + r.Form.Get("scope")})
	})
	mux.HandleFunc("/addresses", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-"+settingsScope {
			t.Fatalf("authorization=%q", got)
		}
		if got := r.Header.Get("x-ui-app"); got != settingsUIApp {
			t.Fatalf("x-ui-app=%q", got)
		}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["state"] != "ACTIVE" || payload["deletable"] != true {
				t.Fatalf("payload=%v", payload)
			}
			if created != nil {
				*created = append(*created, payload["address"].(string))
			}
			w.WriteHeader(createStatus)
			return
		}
		if got := r.URL.Query().Get("q.state.in"); got != "ACTIVE" {
			t.Fatalf("q.state.in=%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"mailaddresslist": []map[string]any{
			{"address": "Base@mail.com"}, {"address": "base-child01@mail.com"}, {"address": ""},
		}})
	})
	mux.HandleFunc("/validations", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != validationAcceptContentType {
			t.Fatalf("accept=%q", got)
		}
		var addresses []string
		if err := json.NewDecoder(r.Body).Decode(&addresses); err != nil || len(addresses) != 1 {
			t.Fatalf("addresses=%v err=%v", addresses, err)
		}
		if strings.HasPrefix(addresses[0], "invalid") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	baseURL = server.URL
	return server
}

func aliasTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := New(Config{OAuthPublicSecret: "public-secret", Endpoints: Endpoints{
		LoginPageURL:          server.URL + "/",
		LoginURL:              server.URL + "/login",
		OAuthURL:              server.URL + "/oauth",
		MailListURL:           server.URL + "/list",
		MailBodyURL:           server.URL + "/body/{mail_id}/Body",
		SettingsAddressesURL:  server.URL + "/addresses",
		SettingsValidationURL: server.URL + "/validations",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestListAliasesNormalizesAddresses(t *testing.T) {
	server := aliasTestServer(t, http.StatusCreated, nil)
	defer server.Close()
	client := aliasTestClient(t, server)

	aliases, err := client.ListAliases(context.Background(), Account{Email: "base@mail.com", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 2 || aliases[0] != "base@mail.com" || aliases[1] != "base-child01@mail.com" {
		t.Fatalf("aliases=%v", aliases)
	}
}

func TestAddAliasValidatesThenCreates(t *testing.T) {
	var created []string
	server := aliasTestServer(t, http.StatusCreated, &created)
	defer server.Close()
	client := aliasTestClient(t, server)

	account := Account{Email: "base@mail.com", Password: "secret"}
	if err := client.AddAlias(context.Background(), account, "Base-Child02@Mail.com"); err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || created[0] != "base-child02@mail.com" {
		t.Fatalf("created=%v", created)
	}

	if err := client.AddAlias(context.Background(), account, "invalid@mail.com"); !errors.Is(err, ErrAliasInvalid) {
		t.Fatalf("err=%v", err)
	}
	if err := client.AddAlias(context.Background(), account, "no-at-sign"); !errors.Is(err, ErrAliasInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestAddAliasReportsQuotaExhausted(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests} {
		server := aliasTestServer(t, status, nil)
		client := aliasTestClient(t, server)
		err := client.AddAlias(context.Background(), Account{Email: "base@mail.com", Password: "secret"}, "base-child03@mail.com")
		if !errors.Is(err, ErrAliasLimit) {
			t.Fatalf("status=%d err=%v", status, err)
		}
		server.Close()
	}

	server := aliasTestServer(t, http.StatusInternalServerError, nil)
	defer server.Close()
	client := aliasTestClient(t, server)
	err := client.AddAlias(context.Background(), Account{Email: "base@mail.com", Password: "secret"}, "base-child04@mail.com")
	if !errors.Is(err, ErrSettings) || errors.Is(err, ErrAliasLimit) {
		t.Fatalf("err=%v", err)
	}
}

func TestAliasAddressUsesLocalPartWithoutPlus(t *testing.T) {
	got := AliasAddress("Base@Mail.com", "Abc123")
	if got != "baseabc123@mail.com" {
		t.Fatalf("address=%q", got)
	}
	if strings.Contains(got, "+") {
		t.Fatal("mail.com alias must not use plus syntax")
	}
	if AliasAddress("no-at-sign", "abc") != "" || AliasAddress("base@mail.com", " ") != "" {
		t.Fatal("expected empty address for invalid input")
	}
}

func TestRandomAliasSuffixShape(t *testing.T) {
	suffix := RandomAliasSuffix()
	if !regexp.MustCompile(`^[a-z]{3}[0-9]{3}$`).MatchString(suffix) {
		t.Fatalf("suffix=%q", suffix)
	}
}
