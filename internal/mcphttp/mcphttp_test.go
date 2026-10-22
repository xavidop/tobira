package mcphttp

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseBind(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		loopback bool
	}{
		{"127.0.0.1:8765", "127.0.0.1:8765", true},
		{":8765", "127.0.0.1:8765", true}, // an empty host is loopback, never every interface
		{"localhost:0", "localhost:0", true},
		{"LocalHost:0", "LocalHost:0", true},
		{"[::1]:9000", "[::1]:9000", true},
		{"127.0.0.2:1", "127.0.0.2:1", true},
		{"0.0.0.0:8765", "0.0.0.0:8765", false},
		{"[::]:8765", "[::]:8765", false},
		{"192.168.1.10:80", "192.168.1.10:80", false},
		{"db.example.com:80", "db.example.com:80", false}, // a name other than localhost is never resolved
		{"localhost.:80", "localhost.:80", false},
	} {
		b, err := ParseBind(tc.in)
		if err != nil {
			t.Errorf("ParseBind(%q): %v", tc.in, err)
			continue
		}
		if b.String() != tc.want || b.Loopback() != tc.loopback {
			t.Errorf("ParseBind(%q) = %q loopback %v, want %q loopback %v", tc.in, b.String(), b.Loopback(), tc.want, tc.loopback)
		}
	}
	for _, in := range []string{"", "8765", "127.0.0.1", "127.0.0.1:", "127.0.0.1:http", "127.0.0.1:65536", "127.0.0.1:-1", "::1:80", "a b:80", "http://127.0.0.1:80"} {
		if _, err := ParseBind(in); err == nil {
			t.Errorf("ParseBind(%q) accepted", in)
		}
	}
}

func TestCheckBind(t *testing.T) {
	lo, _ := ParseBind("127.0.0.1:0")
	wide, _ := ParseBind("0.0.0.0:0")
	if err := CheckBind(lo, false); err != nil {
		t.Errorf("loopback refused: %v", err)
	}
	if err := CheckBind(wide, false); !errors.Is(err, ErrInsecureBind) {
		t.Errorf("non-loopback without --insecure-bind = %v, want ErrInsecureBind", err)
	}
	if err := CheckBind(wide, true); err != nil {
		t.Errorf("non-loopback with --insecure-bind refused: %v", err)
	}
}

func TestParseAllowedHost(t *testing.T) {
	for in, want := range map[string]string{"tobira.local": "tobira.local", " Box.LAN ": "box.lan", "box:9000": "box:9000", "[fd00::1]:80": "[fd00::1]:80", "10.0.0.5": "10.0.0.5",
		"box:080": "box:80", "[fd00::1]:0080": "[fd00::1]:80"} {
		got, err := ParseAllowedHost(in)
		if err != nil || got != want {
			t.Errorf("ParseAllowedHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "  ", "a/b", "u@h", "h:port", "h\x1b", "a b", "http://h"} {
		if _, err := ParseAllowedHost(in); err == nil {
			t.Errorf("ParseAllowedHost(%q) accepted", in)
		}
	}
}

// guarded is a Guard over a handler that reports what it saw.
func guarded(c GuardConfig) http.Handler {
	return Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		_, _ = io.WriteString(w, "ok "+string(b[:min(len(b), 8)]))
	}), c)
}

func do(h http.Handler, host string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const tok = "s3cret-token"

func loopbackGuard(t *testing.T, extra ...string) http.Handler {
	t.Helper()
	b, err := ParseBind("127.0.0.1:4100")
	if err != nil {
		t.Fatal(err)
	}
	return guarded(GuardConfig{Bind: b, Listen: "127.0.0.1:4100", AllowedHosts: extra, Token: []byte(tok)})
}

func TestGuardToken(t *testing.T) {
	h := loopbackGuard(t)
	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"right token", "Bearer " + tok, 200},
		{"scheme in any case", "bearer " + tok, 200},
		{"no header", "", 401},
		{"wrong token", "Bearer nope", 401},
		{"token prefix", "Bearer " + tok[:5], 401},
		{"token with a suffix", "Bearer " + tok + "x", 401},
		{"basic scheme", "Basic " + tok, 401},
		{"bare token", tok, 401},
		{"empty bearer", "Bearer ", 401},
	} {
		hdr := map[string]string{}
		if tc.auth != "" {
			hdr["Authorization"] = tc.auth
		}
		w := do(h, "127.0.0.1:4100", hdr, "{}")
		if w.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, w.Code, tc.want)
		}
		if tc.want == 401 && w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: WWW-Authenticate = %q, want Bearer", tc.name, w.Header().Get("WWW-Authenticate"))
		}
	}
}

func TestGuardNoTokenConfiguredRefusesEverything(t *testing.T) {
	b, _ := ParseBind("127.0.0.1:4100")
	h := guarded(GuardConfig{Bind: b, Listen: "127.0.0.1:4100"})
	for _, auth := range []string{"", "Bearer ", "Bearer x"} {
		if w := do(h, "127.0.0.1:4100", map[string]string{"Authorization": auth}, ""); w.Code != 401 {
			t.Errorf("auth %q: status %d, want 401", auth, w.Code)
		}
	}
}

func TestGuardHostOnLoopback(t *testing.T) {
	h := loopbackGuard(t, "alias.test")
	auth := map[string]string{"Authorization": "Bearer " + tok}
	for host, want := range map[string]int{
		"127.0.0.1:4100":  200,
		"localhost:4100":  200,
		"LOCALHOST:4100":  200,
		"[::1]:4100":      200,
		"alias.test:4100": 200,
		"evil.test:4100":  403, // DNS rebinding: a foreign name pointed at 127.0.0.1
		"localhost:4101":  403,
		"127.0.0.1":       403,
		"localhost":       403,
		"":                403,
		"alias.test:9":    403,
	} {
		if w := do(h, host, auth, ""); w.Code != want {
			t.Errorf("Host %q: status %d, want %d", host, w.Code, want)
		}
	}
	// The host is checked before the token: a foreign Host learns nothing.
	if w := do(h, "evil.test:4100", nil, ""); w.Code != 403 {
		t.Errorf("foreign Host without token: status %d, want 403", w.Code)
	}
}

func TestGuardHostOnInsecureBind(t *testing.T) {
	b, _ := ParseBind("0.0.0.0:4100")
	h := guarded(GuardConfig{Bind: b, Listen: "[::]:4100", AllowedHosts: []string{"box.lan", "proxy:8443"}, Token: []byte(tok)})
	auth := map[string]string{"Authorization": "Bearer " + tok}
	for host, want := range map[string]int{
		"0.0.0.0:4100":   200, // the bound address
		"[::]:4100":      200, // what the listener reports
		"box.lan:4100":   200,
		"BOX.lan:4100":   200,
		"proxy:8443":     200, // an allowed host given with its port matches exactly
		"proxy:4100":     403,
		"localhost:4100": 403, // only a loopback bind allows the loopback names
		"127.0.0.1:4100": 403,
		"evil.test:4100": 403,
	} {
		if w := do(h, host, auth, ""); w.Code != want {
			t.Errorf("Host %q: status %d, want %d", host, w.Code, want)
		}
	}
}

func TestGuardOrigin(t *testing.T) {
	h := loopbackGuard(t)
	for origin, want := range map[string]int{
		"http://localhost:3000":   200,
		"https://127.0.0.1":       200,
		"http://[::1]:8080":       200,
		"http://LocalHost":        200,
		"https://evil.test":       403,
		"http://127.0.0.1.evil.x": 403,
		"null":                    403,
		"file://":                 403,
		"http://localhost.evil.x": 403,
		"not a url":               403,
	} {
		hdr := map[string]string{"Authorization": "Bearer " + tok, "Origin": origin}
		if w := do(h, "127.0.0.1:4100", hdr, ""); w.Code != want {
			t.Errorf("Origin %q: status %d, want %d", origin, w.Code, want)
		}
	}
	// No Origin at all (a non-browser client) is fine.
	if w := do(h, "127.0.0.1:4100", map[string]string{"Authorization": "Bearer " + tok}, ""); w.Code != 200 {
		t.Errorf("no Origin: status %d", w.Code)
	}
}

func TestGuardLimitsTheBody(t *testing.T) {
	h := loopbackGuard(t)
	auth := map[string]string{"Authorization": "Bearer " + tok}
	if w := do(h, "127.0.0.1:4100", auth, strings.Repeat("a", MaxBody)); w.Code != 200 {
		t.Errorf("a body of exactly MaxBody: status %d", w.Code)
	}
	if w := do(h, "127.0.0.1:4100", auth, strings.Repeat("a", MaxBody+1)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over MaxBody: status %d, want the handler to fail reading it", w.Code)
	}
	if MaxBody != 4<<20 {
		t.Errorf("MaxBody = %d, want 4 MiB", MaxBody)
	}
}

func TestListenAddress(t *testing.T) {
	for in, want := range map[string]string{"localhost:0": "127.0.0.1:0", "LOCALHOST:80": "127.0.0.1:80", "127.0.0.1:5": "127.0.0.1:5", "[::1]:5": "[::1]:5", "0.0.0.0:5": "0.0.0.0:5"} {
		b, err := ParseBind(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := b.ListenAddress(); got != want {
			t.Errorf("ListenAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoopbackAddr(t *testing.T) {
	for _, tc := range []struct {
		a    net.Addr
		want bool
	}{
		{&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}, true},
		{&net.TCPAddr{IP: net.ParseIP("::1"), Port: 1}, true},
		{&net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 1}, false},
		{&net.TCPAddr{IP: net.ParseIP("::"), Port: 1}, false},
		{&net.TCPAddr{IP: net.ParseIP("192.168.1.2"), Port: 1}, false},
		{&net.UnixAddr{Name: "/tmp/x", Net: "unix"}, false},
	} {
		if got := LoopbackAddr(tc.a); got != tc.want {
			t.Errorf("LoopbackAddr(%v) = %v, want %v", tc.a, got, tc.want)
		}
	}
}

// On port 80 a browser-style bare Host (no port) is the same address.
func TestGuardBareHostOnPort80(t *testing.T) {
	b, _ := ParseBind("127.0.0.1:80")
	h := guarded(GuardConfig{Bind: b, Listen: "127.0.0.1:80", AllowedHosts: []string{"alias.test", "other:8080"}, Token: []byte(tok)})
	auth := map[string]string{"Authorization": "Bearer " + tok}
	for host, want := range map[string]int{
		"localhost":      200,
		"127.0.0.1":      200,
		"[::1]":          200,
		"localhost:80":   200,
		"alias.test":     200,
		"alias.test:80":  200,
		"other":          403, // allowed only on its own port
		"evil.test":      403,
		"localhost:8080": 403,
		"[::1]:80":       200,
		"localhost:0080": 403, // exact spelling only
	} {
		if w := do(h, host, auth, ""); w.Code != want {
			t.Errorf("Host %q on port 80: status %d, want %d", host, w.Code, want)
		}
	}
}

// guardServer is a real server (deadlines need a real connection) whose
// handler reads the body, then sleeps for pause and reports whether the
// request context is still alive.
func guardServer(t *testing.T, bodyTimeout, pause time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	b, _ := ParseBind("127.0.0.1:" + port)
	h := Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusRequestTimeout)
			return
		}
		time.Sleep(pause)
		if r.Context().Err() != nil {
			http.Error(w, "cancelled", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}), GuardConfig{Bind: b, Listen: ln.Addr().String(), Token: []byte(tok), BodyTimeout: bodyTimeout})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// A body that trickles in is cut off at BodyTimeout.
func TestGuardBodyReadDeadline(t *testing.T) {
	addr := guardServer(t, 300*time.Millisecond, 0)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "POST /mcp HTTP/1.1\r\nHost: "+addr+"\r\nAuthorization: Bearer "+tok+"\r\nContent-Length: 100\r\n\r\nabc")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the stalled body held the request for %v", took)
	}
	if n > 0 && strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
		t.Errorf("a stalled body was served: %q", buf[:n])
	}
}

// The deadline covers reading the body only: a handler that works longer
// afterwards keeps its request alive.
func TestGuardBodyDeadlineEndsWithTheBody(t *testing.T) {
	addr := guardServer(t, 200*time.Millisecond, 600*time.Millisecond)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status %d (%s): the body deadline outlived the body", resp.StatusCode, body)
	}
}

func TestBodyTimeoutDefault(t *testing.T) {
	if DefaultBodyTimeout != 30*time.Second {
		t.Errorf("DefaultBodyTimeout = %v, want 30s", DefaultBodyTimeout)
	}
}
