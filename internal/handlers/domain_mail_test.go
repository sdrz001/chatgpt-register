package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

func domainMailTestRouter(t *testing.T, remote *httptest.Server) (*Handler, *gin.Engine) {
	handler, router := mailboxTestHandler(t)
	handler.Mail = mailfetch.New(mailfetch.WithHTTPClient(remote.Client()))
	router.POST("/domain-mail/config", handler.DomainMailConfig)
	router.POST("/mailboxes/generate", handler.MailboxGenerate)
	for key, value := range map[string]string{
		"mailbox_source": "domain_api", "domain_mail_url": remote.URL,
		"domain_mail_api_key": "stored-secret", "domain_mail_domain": "moemail.app", "domain_mail_expiry_time": "3600000",
	} {
		if err := handler.DB.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return handler, router
}

func TestDomainMailConfigUsesStoredKeyAndReturnsDomains(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config" || r.Header.Get("X-API-Key") != "stored-secret" {
			t.Fatalf("path=%s key=%q", r.URL.Path, r.Header.Get("X-API-Key"))
		}
		fmt.Fprint(w, `{"defaultRole":"CIVILIAN","emailDomains":"moemail.app,mail.example","adminContact":"admin@example.com","maxEmails":"10"}`)
	}))
	defer remote.Close()
	_, router := domainMailTestRouter(t, remote)
	request := httptest.NewRequest(http.MethodPost, "/domain-mail/config", strings.NewReader(`{"url":"","api_key":""}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "moemail.app") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMailboxGenerateRollsBackRemoteOnLocalConflict(t *testing.T) {
	deleted := ""
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/emails/generate":
			fmt.Fprint(w, `{"id":"remote-conflict","email":"conflict@moemail.app"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/emails/remote-conflict":
			deleted = "remote-conflict"
			fmt.Fprint(w, `{"success":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	handler, router := domainMailTestRouter(t, remote)
	if err := handler.DB.Create(&models.Mailbox{Email: "conflict@moemail.app", Status: "verified"}).Error; err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mailboxes/generate", strings.NewReader(`{"count":1,"name_prefix":"conflict","domain":"moemail.app","expiry_time":3600000}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || deleted != "remote-conflict" {
		t.Fatalf("status=%d deleted=%q body=%s", response.Code, deleted, response.Body.String())
	}
}

func TestMailboxGeneratePersistsRemoteMailbox(t *testing.T) {
	generated := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/emails/generate" || r.Header.Get("X-API-Key") != "stored-secret" {
			t.Fatalf("path=%s key=%q", r.URL.Path, r.Header.Get("X-API-Key"))
		}
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		generated++
		name, _ := input["name"].(string)
		fmt.Fprintf(w, `{"id":"remote-%d","email":"%s@moemail.app"}`, generated, name)
	}))
	defer remote.Close()
	handler, router := domainMailTestRouter(t, remote)
	request := httptest.NewRequest(http.MethodPost, "/mailboxes/generate", strings.NewReader(`{"count":2,"name_prefix":"Batch","domain":"moemail.app","expiry_time":3600000}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"added":2`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var mailboxes []models.Mailbox
	if err := handler.DB.Order("id").Find(&mailboxes).Error; err != nil {
		t.Fatal(err)
	}
	if len(mailboxes) != 2 || mailboxes[0].Provider != "domain_api" || mailboxes[0].RemoteMailboxID != "remote-1" || mailboxes[0].Status != "verified" {
		t.Fatalf("mailboxes=%+v", mailboxes)
	}
	list := httptest.NewRecorder()
	router.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/mailboxes", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"register_limit":1`) || strings.Contains(list.Body.String(), "remote-1") || strings.Contains(list.Body.String(), "stored-secret") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
}
