package netpolicy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":          true,
		"LOCALHOST":          true,
		"LocalHost":          true,
		"127.0.0.1":          true,
		"127.1.2.3":          true,
		"::1":                true,
		"[::1]":              true,
		"[0:0:0:0:0:0:0:1]":  true,
		"":                   false,
		"[]":                 false,
		"[localhost]":        false,
		"localhost.evil.com": false,
		"127.0.0.1.nip.io":   false,
		"2130706433":         false,
		"127.1":              false,
		"0177.0.0.1":         false,
		"10.0.0.1":           false,
		"[::2]":              false,
		"::ffff:10.0.0.1":    false,
		"[::1":               false,
		"::1]":               false,
		"example.com":        false,
	} {
		if got := IsLoopback(host); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestInCloudZone(t *testing.T) {
	zones := []string{"pinecone.io", "amazonaws.com.cn"}
	for host, want := range map[string]bool{
		"pinecone.io":                   true,
		"api.pinecone.io":               true,
		"API.Pinecone.IO":               true,
		"api.pinecone.io.":              true,
		"x.cn-north-1.amazonaws.com.cn": true,
		"evilpinecone.io":               false,
		"pinecone.io.evil.com":          false,
		"amazonaws.com":                 false,
		"":                              false,
		".":                             false,
	} {
		if got := InCloudZone(host, zones...); got != want {
			t.Errorf("InCloudZone(%q) = %v, want %v", host, got, want)
		}
	}
	if InCloudZone("pinecone.io") {
		t.Error("no zones must match nothing")
	}
	if InCloudZone("anything", "") {
		t.Error("an empty zone must match nothing")
	}
}

func TestCheckPlaintext(t *testing.T) {
	zones := []string{"turso.io"}
	cases := []struct {
		host  string
		allow bool
		want  Verdict
	}{
		{"localhost", false, Allowed},
		{"[::1]", false, Allowed},
		{"db.example.com", false, NeedsOptIn},
		{"db.example.com", true, Allowed},
		{"x.turso.io", false, RefusedCloud},
		{"X.TURSO.IO.", true, RefusedCloud},
	}
	for _, c := range cases {
		if got := CheckPlaintext(c.host, c.allow, zones...); got != c.want {
			t.Errorf("CheckPlaintext(%q, %v) = %v, want %v", c.host, c.allow, got, c.want)
		}
	}
}

// TestRefuseRedirects proves a client using RefuseRedirects sends nothing to
// the redirect target, even on the same host name (where Go would keep
// Authorization).
func TestRefuseRedirects(t *testing.T) {
	target, hits := countingServer(t)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer src.Close()
	c := &http.Client{CheckRedirect: RefuseRedirects}
	req, _ := http.NewRequest(http.MethodPost, src.URL, strings.NewReader("secret"))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the 307 handed back", resp.StatusCode)
	}
	if *hits != 0 {
		t.Errorf("redirect target got %d requests, want 0", *hits)
	}
}

type closer struct {
	http.RoundTripper
	closed bool
}

func (c *closer) CloseIdleConnections() { c.closed = true }

// TestDropRedirectLocation proves an http.Client that follows redirects
// hands every 3xx back when its transport drops Location, and that
// CloseIdleConnections reaches the wrapped transport.
func TestDropRedirectLocation(t *testing.T) {
	target, hits := countingServer(t)
	for _, code := range []int{301, 302, 303, 307, 308} {
		src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, code)
		}))
		inner := &closer{RoundTripper: http.DefaultTransport.(*http.Transport).Clone()}
		c := &http.Client{Transport: DropRedirectLocation(inner)}
		resp, err := c.Post(src.URL, "text/plain", strings.NewReader("password"))
		if err != nil {
			t.Fatalf("%d: %v", code, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != code || resp.Header.Get("Location") != "" {
			t.Errorf("%d: got status %d, Location %q", code, resp.StatusCode, resp.Header.Get("Location"))
		}
		c.CloseIdleConnections()
		if !inner.closed {
			t.Errorf("%d: CloseIdleConnections did not reach the wrapped transport", code)
		}
		src.Close()
	}
	if *hits != 0 {
		t.Errorf("redirect target got %d requests, want 0", *hits)
	}
}

func TestIsTLSMismatch(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plain.Close()
	_, err := (&http.Client{}).Get("https://" + plain.Listener.Addr().String())
	if err == nil {
		t.Fatal("https to a plain server succeeded")
	}
	if !IsTLSMismatch(err) {
		t.Errorf("IsTLSMismatch(%v) = false", err)
	}
	// A raw TLS handshake against a server that answers in clear.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
		_ = c.Close()
	}()
	_, err = (&http.Client{Transport: &http.Transport{}}).Get("https://" + ln.Addr().String())
	if err == nil || !IsTLSMismatch(err) {
		t.Errorf("IsTLSMismatch(%v) = false", err)
	}
	wrapped := fmt.Errorf("rpc error: code = Unavailable desc = connection error: desc = %q", "transport: authentication handshake failed: tls: first record does not look like a TLS handshake")
	if !IsTLSMismatch(wrapped) {
		t.Errorf("IsTLSMismatch(%v) = false", wrapped)
	}
	for _, e := range []error{nil, errors.New("connection refused"), errors.New("401 unauthorized")} {
		if IsTLSMismatch(e) {
			t.Errorf("IsTLSMismatch(%v) = true", e)
		}
	}
}

func countingServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	n := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { *n++ }))
	t.Cleanup(srv.Close)
	return srv, n
}
