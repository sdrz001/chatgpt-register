package handlers

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/proxy"
)

type proxyTestInput struct {
	Proxy string `json:"proxy"`
}

// normalizeProxy 把 host:port:user:pass 之类的写法转成标准 URL；带 scheme 的原样返回。
func normalizeProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		return raw
	}
	if separator := strings.LastIndex(raw, "@"); separator > 0 {
		credentials, hostPort := raw[:separator], raw[separator+1:]
		credentialSeparator := strings.Index(credentials, ":")
		hostSeparator := strings.LastIndex(hostPort, ":")
		if credentialSeparator > 0 && credentialSeparator < len(credentials)-1 && hostSeparator > 0 && validHTTPProxyPort(hostPort[hostSeparator+1:]) {
			username, password := credentials[:credentialSeparator], credentials[credentialSeparator+1:]
			return "http://" + url.UserPassword(username, password).String() + "@" + hostPort
		}
	}
	parts := strings.Split(raw, ":")
	switch len(parts) {
	case 2: // host:port
		return "http://" + parts[0] + ":" + parts[1]
	case 4: // host:port:user:pass
		return "http://" + url.UserPassword(parts[2], parts[3]).String() + "@" + parts[0] + ":" + parts[1]
	default:
		return "http://" + raw
	}
}

func validHTTPProxyPort(value string) bool {
	port, err := strconv.Atoi(value)
	return err == nil && port >= 1 && port <= 65535
}

func newProxyHTTPClient(rawProxy string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{}
	proxyURL := normalizeProxy(rawProxy)
	if proxyURL == "" {
		return &http.Client{Transport: transport, Timeout: timeout}, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Hostname() == "" || !validHTTPProxyPort(u.Port()) || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("代理格式错误")
	}
	if u.User != nil {
		password, hasPassword := u.User.Password()
		if u.User.Username() == "" || !hasPassword || password == "" {
			return nil, fmt.Errorf("代理格式错误")
		}
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
			transport.DialContext = contextDialer.DialContext
		} else {
			transport.Dial = dialer.Dial
		}
	default:
		return nil, fmt.Errorf("不支持的代理类型: %s", u.Scheme)
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// ProxyTest 通过给定代理请求一个 IP 探测服务，返回出口 IP，用于验证代理可用。
func (h *Handler) ProxyTest(c *gin.Context) {
	var in proxyTestInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(in.Proxy) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "代理为空"})
		return
	}
	client, err := newProxyHTTPClient(in.Proxy, 12*time.Second)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	start := time.Now()
	resp, err := client.Get("https://api.ipify.org?format=text")
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	ip := strings.TrimSpace(string(buf[:n]))
	c.JSON(http.StatusOK, gin.H{"ok": true, "ip": ip, "ms": time.Since(start).Milliseconds()})
}
