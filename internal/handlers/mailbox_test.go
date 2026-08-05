package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func mailboxTestHandler(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&models.Category{}, &models.Mailbox{}, &models.Registration{}, &models.Setting{}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{DB: database, Mail: mailfetch.New()}
	router := gin.New()
	router.POST("/mailboxes/import", handler.MailboxImport)
	router.GET("/mailboxes", handler.MailboxList)
	router.POST("/categories", handler.CategoryCreate)
	router.POST("/categories/assign", handler.CategoryAssign)
	return handler, router
}

func TestMailboxImportAPICodeURLIsWriteOnly(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	codeURL := "https://codes.example.test/api/code?username=user%40icloud.com&password=secret-value"
	request := httptest.NewRequest(http.MethodPost, "/mailboxes/import", strings.NewReader(`{"items":[{"email":"user@icloud.com","password":"unused-middle","code_url":"`+codeURL+`"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"added":1`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), codeURL) || strings.Contains(response.Body.String(), "secret-value") {
		t.Fatalf("import response leaked URL: %s", response.Body.String())
	}
	var mailbox models.Mailbox
	if err := handler.DB.First(&mailbox, "email = ?", "user@icloud.com").Error; err != nil {
		t.Fatal(err)
	}
	if mailbox.Provider != "api" || mailbox.Status != "verified" || mailbox.CodeURL != codeURL {
		t.Fatalf("mailbox=%+v", mailbox)
	}
	if mailbox.Password != "" || mailbox.ClientID != "" || mailbox.RefreshToken != "" {
		t.Fatalf("unused credentials persisted: %+v", mailbox)
	}

	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/mailboxes", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	for _, secret := range []string{codeURL, "secret-value"} {
		if strings.Contains(listResponse.Body.String(), secret) {
			t.Fatalf("list leaked %q: %s", secret, listResponse.Body.String())
		}
	}
	var list struct {
		Data []models.Mailbox `json:"data"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || !list.Data[0].CodeURLConfigured {
		t.Fatalf("list=%+v", list.Data)
	}
}

func TestMailboxCategoryAssignFilterAndClear(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	mailbox := models.Mailbox{Email: "classified@example.test", Status: "verified"}
	if err := handler.DB.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	category := models.Category{Scope: "mailbox", Name: "主力邮箱"}
	if err := handler.DB.Create(&category).Error; err != nil {
		t.Fatal(err)
	}
	assign := func(categoryJSON string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/categories/assign", strings.NewReader(`{"scope":"mailbox","ids":[`+strconv.FormatUint(uint64(mailbox.ID), 10)+`],"category_id":`+categoryJSON+`}`))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		return response
	}
	if response := assign(strconv.FormatUint(uint64(category.ID), 10)); response.Code != http.StatusOK {
		t.Fatalf("assign status=%d body=%s", response.Code, response.Body.String())
	}
	list := httptest.NewRecorder()
	router.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/mailboxes?category_id="+strconv.FormatUint(uint64(category.ID), 10), nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "主力邮箱") || !strings.Contains(list.Body.String(), "classified@example.test") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	if response := assign("null"); response.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", response.Code, response.Body.String())
	}
	if err := handler.DB.First(&mailbox, mailbox.ID).Error; err != nil || mailbox.CategoryID != nil {
		t.Fatalf("mailbox=%+v error=%v", mailbox, err)
	}
}

func TestMailboxImportRejectsInvalidCodeURL(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/mailboxes/import", strings.NewReader(`{"items":[{"email":"user@icloud.com","code_url":"file:///private/code"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"skipped":1`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var count int64
	if err := handler.DB.Model(&models.Mailbox{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("count=%d error=%v", count, err)
	}
}
