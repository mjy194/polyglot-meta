package auth

import (
	"net/http"
	"testing"
)

// transport 上的 Proxy 函数对给定请求解析出的代理 URL（无代理时返回空串）。
func resolvedProxy(t *testing.T, tr *http.Transport) string {
	t.Helper()
	if tr.Proxy == nil {
		return ""
	}
	req, err := http.NewRequest(http.MethodGet, "https://cloud.uipath.com/identity_/connect/token", nil)
	if err != nil {
		t.Fatalf("build probe request: %v", err)
	}
	u, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("proxy func returned error: %v", err)
	}
	if u == nil {
		return ""
	}
	return u.String()
}

// 登录 transport 始终强制 IPv4 dialer；proxyURL 非空时叠加出站代理。
func TestNewAuthTransportProxyAndIPv4(t *testing.T) {
	t.Run("no proxy -> direct, still has dialer", func(t *testing.T) {
		tr := newAuthTransport("")
		if tr.DialContext == nil {
			t.Fatal("expected forced-IPv4 DialContext to be set")
		}
		if got := resolvedProxy(t, tr); got != "" {
			t.Fatalf("expected no proxy, got %q", got)
		}
	})

	t.Run("with proxy -> Proxy set, dialer preserved", func(t *testing.T) {
		const proxyURL = "http://user:pass@127.0.0.1:8888"
		tr := newAuthTransport(proxyURL)
		if tr.DialContext == nil {
			t.Fatal("forced-IPv4 DialContext must survive proxy injection")
		}
		if got := resolvedProxy(t, tr); got != proxyURL {
			t.Fatalf("proxy = %q, want %q", got, proxyURL)
		}
	})

	t.Run("invalid proxy -> falls back to direct, dialer preserved", func(t *testing.T) {
		// url.Parse 对 "://bad" 报错 → 回退无代理，但仍保留 IPv4 dialer。
		tr := newAuthTransport("://bad")
		if tr.DialContext == nil {
			t.Fatal("forced-IPv4 DialContext must survive invalid proxy")
		}
		if got := resolvedProxy(t, tr); got != "" {
			t.Fatalf("invalid proxy should fall back to direct, got %q", got)
		}
	})
}

// SetProxy 热更新代理时必须保留同一个 cookie jar（已建立的 web session 不能丢）。
func TestSetProxyPreservesJar(t *testing.T) {
	a, err := NewUiPathAuth("a@example.com", "pw", "")
	if err != nil {
		t.Fatalf("NewUiPathAuth: %v", err)
	}
	jarBefore := a.httpClient.Jar
	if jarBefore == nil {
		t.Fatal("expected cookie jar to be initialized")
	}
	if got := resolvedProxy(t, a.httpClient.Transport.(*http.Transport)); got != "" {
		t.Fatalf("fresh auth should be direct, got %q", got)
	}

	const proxyURL = "http://10.0.0.1:3128"
	a.SetProxy(proxyURL)

	if a.httpClient.Jar != jarBefore {
		t.Fatal("SetProxy must preserve the existing cookie jar (web session cookies)")
	}
	if got := resolvedProxy(t, a.httpClient.Transport.(*http.Transport)); got != proxyURL {
		t.Fatalf("after SetProxy, proxy = %q, want %q", got, proxyURL)
	}
	if a.proxyURL != proxyURL {
		t.Fatalf("proxyURL field = %q, want %q", a.proxyURL, proxyURL)
	}

	// 切回直连
	a.SetProxy("")
	if a.httpClient.Jar != jarBefore {
		t.Fatal("SetProxy(\"\") must still preserve the jar")
	}
	if got := resolvedProxy(t, a.httpClient.Transport.(*http.Transport)); got != "" {
		t.Fatalf("after SetProxy(\"\"), expected direct, got %q", got)
	}
}
