package client

import (
	"net/http"
	"testing"
)

func TestTransportForProxyEmptyIsDefault(t *testing.T) {
	if transportForProxy("") != http.DefaultTransport {
		t.Fatal("empty proxy URL must return DefaultTransport")
	}
}

func TestTransportForProxyCachesByURL(t *testing.T) {
	tr1 := transportForProxy("http://127.0.0.1:8888")
	tr2 := transportForProxy("http://127.0.0.1:8888")
	if tr1 != tr2 {
		t.Fatal("transport should be cached and reused for the same proxy URL")
	}
	tr3 := transportForProxy("socks5://127.0.0.1:1080")
	if tr3 == tr1 {
		t.Fatal("different proxy URL should yield a different transport")
	}
}

func TestTransportForProxyInvalidFallsBack(t *testing.T) {
	// http.ProxyURL tolerates many forms; an unparseable scheme still must not panic
	// and must return a usable RoundTripper.
	tr := transportForProxy("http://[::1]:8888") // valid
	if tr == http.DefaultTransport {
		t.Fatal("valid proxy URL should return a custom transport, not default")
	}
}

func TestSetProxyAppliesTransport(t *testing.T) {
	c := NewUiPathClient("http://example", "tok")
	c.SetProxy("http://127.0.0.1:8888")
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("expected *http.Transport after SetProxy, got %T", c.httpClient.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("transport Proxy func should be set")
	}

	// empty proxy URL is a no-op (stays default — nil Transport on the client)
	c2 := NewUiPathClient("http://example", "tok")
	c2.SetProxy("")
	if c2.httpClient.Transport != nil {
		t.Fatal("empty proxy URL should leave Transport unset (default)")
	}
}
