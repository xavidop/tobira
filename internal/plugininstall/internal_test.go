package plugininstall

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func redirectReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{URL: u}
}

func TestCheckRedirectRefusesADowngrade(t *testing.T) {
	via := []*http.Request{redirectReq(t, "https://example.com/tobira-driver-x")}
	err := checkRedirect(redirectReq(t, "http://example.com/tobira-driver-x"), via)
	if err == nil || !strings.Contains(err.Error(), "https to http") {
		t.Errorf("https -> http redirect: err = %v, want a refused downgrade", err)
	}
	// Once on https, staying on https is fine, and http may move to https.
	if err := checkRedirect(redirectReq(t, "https://cdn.example.com/x"), via); err != nil {
		t.Errorf("https -> https: %v", err)
	}
	httpVia := []*http.Request{redirectReq(t, "http://example.com/x")}
	if err := checkRedirect(redirectReq(t, "https://example.com/x"), httpVia); err != nil {
		t.Errorf("http -> https: %v", err)
	}
	// A downgrade anywhere in the chain counts: https, then https, then http.
	chain := []*http.Request{redirectReq(t, "https://a.example/x"), redirectReq(t, "https://b.example/x")}
	if err := checkRedirect(redirectReq(t, "http://c.example/x"), chain); err == nil {
		t.Error("a downgrade after two https hops was followed")
	}
}

func TestClientHasTransportTimeouts(t *testing.T) {
	c := newClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout != 30*time.Second || tr.TLSHandshakeTimeout <= 0 || tr.TLSHandshakeTimeout > 30*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, TLSHandshakeTimeout = %v", tr.ResponseHeaderTimeout, tr.TLSHandshakeTimeout)
	}
	if tr.DialContext == nil || tr.Proxy == nil {
		t.Error("the transport has no bounded dialer or ignores proxy settings")
	}
	if c.CheckRedirect == nil {
		t.Error("the client has no redirect policy")
	}
}

func TestIsURL(t *testing.T) {
	for s, want := range map[string]bool{
		"https://example.com/x":            true,
		"HTTP://example.com/x":             true,
		"file:///etc/passwd":               true,
		"git+ssh://host/x":                 true,
		"/tmp/weird://dir/tobira-driver-x": false,
		"./a://b":                          false,
		"tobira-driver-x":                  false,
		"1http://x":                        false,
		"://x":                             false,
		`C:\plugins\tobira-driver-x.exe`:   false,
	} {
		if got := isURL(s); got != want {
			t.Errorf("isURL(%q) = %v, want %v", s, got, want)
		}
	}
}
