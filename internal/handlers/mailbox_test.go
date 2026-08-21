package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"chatgpt-register/internal/mailcom"
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
	router.POST("/mailboxes", handler.MailboxCreate)
	router.POST("/mailboxes/import", handler.MailboxImport)
	router.PUT("/mailboxes/:id", handler.MailboxUpdate)
	router.GET("/mailboxes/options", handler.MailboxOptions)
	router.GET("/mailboxes", handler.MailboxList)
	router.POST("/registrations/mailbox-links", handler.RegistrationMailboxLinks)
	router.POST("/registrations/plus-mail-check", handler.RegistrationPlusMailCheck)
	router.POST("/categories", handler.CategoryCreate)
	router.POST("/categories/assign", handler.CategoryAssign)
	return handler, router
}

func TestMailboxListDoesNotExposeCredentials(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	mailbox := models.Mailbox{
		Email: "sensitive@example.test", Password: "mail-password", ClientID: "client-secret",
		RefreshToken: "refresh-secret", CodeURL: "https://codes.example.test/api?token=code-secret", Status: "verified",
	}
	if err := handler.DB.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/mailboxes", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"mail-password", "client-secret", "refresh-secret", "code-secret", "https://codes.example.test"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("list leaked %q: %s", secret, response.Body.String())
		}
	}
	for _, flag := range []string{"\"password_configured\":true", "\"client_id_configured\":true", "\"refresh_token_configured\":true", "\"code_url_configured\":true"} {
		if !strings.Contains(response.Body.String(), flag) {
			t.Fatalf("list missing %s: %s", flag, response.Body.String())
		}
	}
}

func TestMailboxOptionsReturnsOnlyVerifiedIDAndEmail(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	mailboxes := []models.Mailbox{
		{Email: "verified@example.test", Password: "secret", RefreshToken: "refresh", CodeURL: "https://codes.example.test/value", Status: "verified"},
		{Email: "pending@example.test", Password: "pending-secret", Status: "unverified"},
	}
	if err := handler.DB.Create(&mailboxes).Error; err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/mailboxes/options", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "verified@example.test") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, forbidden := range []string{"pending@example.test", "secret", "refresh", "codes.example.test", "status", "password"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("options leaked %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestMailboxImportRejectsAccountCategory(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	category := models.Category{Scope: "account", Name: "账户分类"}
	if err := handler.DB.Create(&category).Error; err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"items":[{"email":"wrong-scope@example.test","code_url":"https://codes.example.test/value"}],"category_id":%d}`, category.ID)
	request := httptest.NewRequest(http.MethodPost, "/mailboxes/import", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "作用域") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var count int64
	if err := handler.DB.Model(&models.Mailbox{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("count=%d error=%v", count, err)
	}
}

func TestMailboxImportAppliesSelectedCategory(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	category := models.Category{Scope: "mailbox", Name: "批量导入分类"}
	if err := handler.DB.Create(&category).Error; err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"items":[{"email":"categorized@example.test","code_url":"https://codes.example.test/value"}],"category_id":%d}`, category.ID)
	request := httptest.NewRequest(http.MethodPost, "/mailboxes/import", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"added":1`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var mailbox models.Mailbox
	if err := handler.DB.Where("email = ?", "categorized@example.test").First(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	if mailbox.CategoryID == nil || *mailbox.CategoryID != category.ID {
		t.Fatalf("category_id=%v want=%d", mailbox.CategoryID, category.ID)
	}
}

func TestMailboxUpdateClearsCodeURLWhenSwitchingProvider(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	mailbox := models.Mailbox{
		Email: "switch@example.test", Provider: "api", CodeURL: "https://codes.example.test/api?token=old", Status: "verified",
	}
	if err := handler.DB.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/mailboxes/"+strconv.FormatUint(uint64(mailbox.ID), 10), strings.NewReader(`{"email":"switch@example.test","provider":"outlook","client_id":"new-client","refresh_token":"new-refresh","status":"verified"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "new-refresh") || strings.Contains(response.Body.String(), "old") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var updated models.Mailbox
	if err := handler.DB.First(&updated, mailbox.ID).Error; err != nil {
		t.Fatal(err)
	}
	if updated.Provider != "outlook" || updated.CodeURL != "" || updated.ClientID != "new-client" || updated.RefreshToken != "new-refresh" {
		t.Fatalf("mailbox=%+v", updated)
	}
}

func TestMailboxListFiltersByRegistrationStatus(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	mailboxes := []models.Mailbox{
		{Email: "alpha@example.test", Status: "verified"},
		{Email: "beta@example.test", Status: "verified"},
	}
	if err := handler.DB.Create(&mailboxes).Error; err != nil {
		t.Fatal(err)
	}
	if err := handler.DB.Create(&models.Registration{
		Email: mailboxes[1].Email, MailboxID: mailboxes[1].ID, Status: "registered",
	}).Error; err != nil {
		t.Fatal(err)
	}
	assertList := func(value, included, excluded string) {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/mailboxes?registration_status="+value, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), included) || strings.Contains(response.Body.String(), excluded) {
			t.Fatalf("filter=%s status=%d body=%s", value, response.Code, response.Body.String())
		}
	}
	assertList("unregistered", mailboxes[0].Email, mailboxes[1].Email)
	assertList("registered", mailboxes[1].Email, mailboxes[0].Email)
}

func TestRegistrationPlusMailCheckFindsCodeURLArchiveAndUpdatesPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("n") != "200" {
			t.Errorf("query=%v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body><div class="fr">OpenAI</div><div class="su">ChatGPT - 你的新套餐</div><div class="bd">你已成功订阅 ChatGPT Plus。<span>ChatGPT Plus Subscription</span><span>$20.00</span></div></body></html>`)
	}))
	defer server.Close()

	handler, router := mailboxTestHandler(t)
	handler.Mail = mailfetch.New(mailfetch.WithHTTPClient(server.Client()))
	mailbox := models.Mailbox{Email: "plus@example.test", CodeURL: server.URL + "/inbox?token=secret", Status: "verified"}
	if err := handler.DB.Create(&mailbox).Error; err != nil {
		t.Fatal(err)
	}
	registration := models.Registration{
		Email: mailbox.Email, MailboxID: mailbox.ID, Status: "registered", PlanType: "free",
		AuthData: `{"plan_type":"free","credentials":{"plan_type":"free"}}`,
	}
	if err := handler.DB.Create(&registration).Error; err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/registrations/plus-mail-check", strings.NewReader(fmt.Sprintf(`{"ids":[%d]}`, registration.ID)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"found":1`) || !strings.Contains(response.Body.String(), `"status":"found"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if err := handler.DB.First(&registration, registration.ID).Error; err != nil {
		t.Fatal(err)
	}
	if registration.PlanType != "plus" || registration.PlusMailStatus != "found" || registration.PlusMailCheckedAt == nil || registration.PlusMailSubject == "" {
		t.Fatalf("registration=%+v", registration)
	}
	if !strings.Contains(registration.AuthData, `"plan_type": "plus"`) {
		t.Fatalf("auth_data=%s", registration.AuthData)
	}
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

func TestMailboxImportMailComProvider(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/mailboxes/import", strings.NewReader(`{"items":[{"email":"user@mail.com","password":"mail-password","provider":"mailcom"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"added":1`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var mailbox models.Mailbox
	if err := handler.DB.First(&mailbox, "email = ?", "user@mail.com").Error; err != nil {
		t.Fatal(err)
	}
	if mailbox.Provider != "mailcom" || mailbox.Password != "mail-password" || mailbox.Status != "verifying" {
		t.Fatalf("mailbox=%+v", mailbox)
	}
	if strings.Contains(response.Body.String(), "mail-password") {
		t.Fatalf("import response leaked password: %s", response.Body.String())
	}
}

func TestMailboxImportRecognizesMailComDomainWithEmailAndPassword(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/mailboxes/import", strings.NewReader(`{"items":[{"email":"Rivers_Prattyor@musician.org","password":"mail-password"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"added":1`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var mailbox models.Mailbox
	if err := handler.DB.First(&mailbox, "email = ?", "Rivers_Prattyor@musician.org").Error; err != nil {
		t.Fatal(err)
	}
	if mailbox.Provider != mailcom.Provider || mailbox.Password != "mail-password" || mailbox.Status != "verifying" {
		t.Fatalf("mailbox=%+v", mailbox)
	}
}

func TestMailboxImportRejectsInvalidMailComCredentials(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/mailboxes/import", strings.NewReader(`{"items":[{"email":"user@example.com","password":"mail-password","provider":"mailcom"},{"email":"second@mail.com","provider":"mailcom"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"added":0`) || !strings.Contains(response.Body.String(), `"skipped":2`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var count int64
	if err := handler.DB.Model(&models.Mailbox{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("count=%d error=%v", count, err)
	}
}

func TestMailboxCreateRejectsMailComWithoutPassword(t *testing.T) {
	_, router := mailboxTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/mailboxes", strings.NewReader(`{"email":"user@mail.com","provider":"mailcom"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "mail.com") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
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

func TestRegistrationMailboxLinksReturnsSelectedOrderAndSkipsMissingURL(t *testing.T) {
	handler, router := mailboxTestHandler(t)
	mailboxes := []models.Mailbox{
		{Email: "first@example.test", CodeURL: "https://codes.example.test/first?token=one", Status: "verified"},
		{Email: "second@example.test", Status: "verified"},
		{Email: "third@example.test", CodeURL: "https://codes.example.test/third?token=three", Status: "verified"},
	}
	if err := handler.DB.Create(&mailboxes).Error; err != nil {
		t.Fatal(err)
	}
	registrations := []models.Registration{
		{Email: "first+account@example.test", MailboxID: mailboxes[0].ID},
		{Email: "second+account@example.test", MailboxID: mailboxes[1].ID},
		{Email: "third+account@example.test", MailboxID: mailboxes[2].ID},
	}
	if err := handler.DB.Create(&registrations).Error; err != nil {
		t.Fatal(err)
	}

	body := `{"ids":[` + strconv.FormatUint(uint64(registrations[2].ID), 10) + `,` + strconv.FormatUint(uint64(registrations[1].ID), 10) + `,` + strconv.FormatUint(uint64(registrations[0].ID), 10) + `,999999]}`
	request := httptest.NewRequest(http.MethodPost, "/registrations/mailbox-links", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Items []struct {
			RegistrationID uint   `json:"registration_id"`
			Email          string `json:"email"`
			CodeURL        string `json:"code_url"`
		} `json:"items"`
		Count   int `json:"count"`
		Skipped int `json:"skipped"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 2 || result.Skipped != 2 || len(result.Items) != 2 {
		t.Fatalf("result=%+v", result)
	}
	if result.Items[0].RegistrationID != registrations[2].ID || result.Items[0].Email != registrations[2].Email || result.Items[0].CodeURL != mailboxes[2].CodeURL {
		t.Fatalf("first item=%+v", result.Items[0])
	}
	if result.Items[1].RegistrationID != registrations[0].ID || result.Items[1].Email != registrations[0].Email || result.Items[1].CodeURL != mailboxes[0].CodeURL {
		t.Fatalf("second item=%+v", result.Items[1])
	}
}
