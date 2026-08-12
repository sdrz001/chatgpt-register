package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

func proxyPoolRequest(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	return response
}

func TestProxyHTTPClientSupportsPoolFormats(t *testing.T) {
	for _, value := range []string{
		"proxy.test:8080",
		"proxy.test:8080:user:pass",
		"user:pass@proxy.test:8080",
		"http://user:pass@proxy.test:8080",
		"https://proxy.test:8080",
		"socks5://user:pass@proxy.test:1080",
	} {
		if _, err := newProxyHTTPClient(value, 0); err != nil {
			t.Fatalf("%q rejected: %v", value, err)
		}
	}
	for _, value := range []string{
		"proxy.test",
		"proxy.test:0",
		"proxy.test:65536",
		"http://user@proxy.test:8080",
		"http://proxy.test:8080/path",
	} {
		if _, err := newProxyHTTPClient(value, 0); err == nil {
			t.Fatalf("%q accepted", value)
		}
	}
}

func TestProxyPoolCRUDAndDefaultSelection(t *testing.T) {
	handler, router := settingsTestHandler(t)
	response := proxyPoolRequest(router, http.MethodPost, "/proxy-pools", `{"name":"日本节点","proxies":"user:pass@proxy.test:8080\nuser:pass@proxy.test:8080\nproxy-2.test:8080"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created models.ProxyPool
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ProxyCount != 2 || created.Proxies != "user:pass@proxy.test:8080\nproxy-2.test:8080" {
		t.Fatalf("created=%+v", created)
	}

	response = proxyPoolRequest(router, http.MethodGet, "/proxy-pools", "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "proxy.test") {
		t.Fatalf("list leaked proxies: status=%d body=%s", response.Code, response.Body.String())
	}
	var listed struct {
		Data               []models.ProxyPool `json:"data"`
		DefaultProxyPoolID uint               `json:"default_proxy_pool_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || len(listed.Data) != 1 || listed.Data[0].ProxyCount != 2 {
		t.Fatalf("listed=%+v error=%v", listed, err)
	}

	path := "/proxy-pools/" + strconv.FormatUint(uint64(created.ID), 10)
	response = proxyPoolRequest(router, http.MethodGet, path, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "proxy.test") {
		t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
	}
	response = proxyPoolRequest(router, http.MethodPut, path, `{"name":"日本主池","proxies":"proxy-3.test:8080","is_default":true}`)
	if response.Code != http.StatusOK || handler.defaultProxyPoolID() != created.ID {
		t.Fatalf("update status=%d default=%d body=%s", response.Code, handler.defaultProxyPoolID(), response.Body.String())
	}
	response = proxyPoolRequest(router, http.MethodDelete, path, "")
	if response.Code != http.StatusOK || handler.defaultProxyPoolID() != 0 {
		t.Fatalf("delete status=%d default=%d body=%s", response.Code, handler.defaultProxyPoolID(), response.Body.String())
	}
}

func TestProxyPoolValidation(t *testing.T) {
	_, router := settingsTestHandler(t)
	for name, body := range map[string]string{
		"empty name":    `{"name":"","proxies":"proxy.test:8080"}`,
		"invalid proxy": `{"name":"bad","proxies":"proxy.test"}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := proxyPoolRequest(router, http.MethodPost, "/proxy-pools", body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	response := proxyPoolRequest(router, http.MethodPost, "/proxy-pools", `{"name":"Pool","proxies":""}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("empty pool create status=%d body=%s", response.Code, response.Body.String())
	}
	var empty models.ProxyPool
	if err := json.Unmarshal(response.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	response = proxyPoolRequest(router, http.MethodPut, "/proxy-pools/default", `{"proxy_pool_id":`+strconv.FormatUint(uint64(empty.ID), 10)+`}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "至少需要一个代理") {
		t.Fatalf("empty default status=%d body=%s", response.Code, response.Body.String())
	}
	response = proxyPoolRequest(router, http.MethodPut, "/proxy-pools/"+strconv.FormatUint(uint64(empty.ID), 10), `{"name":"Pool","proxies":"","is_default":true}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "至少需要一个代理") {
		t.Fatalf("empty atomic default status=%d body=%s", response.Code, response.Body.String())
	}
	response = proxyPoolRequest(router, http.MethodPost, "/proxy-pools", `{"name":"pool","proxies":"proxy.test:8080"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s", response.Code, response.Body.String())
	}
	response = proxyPoolRequest(router, http.MethodPut, "/proxy-pools/default", `{"proxy_pool_id":999}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing default status=%d body=%s", response.Code, response.Body.String())
	}
}
