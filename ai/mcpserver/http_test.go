package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/mcphttp"
)

// listenLine is what ServeHTTP prints once it listens.
var listenLine = regexp.MustCompile(`MCP HTTP server listening on http://(\S+)/mcp\n`)

// stderrWatch is a stderr that hands over the bound address once printed.
type stderrWatch struct {
	lockedBuffer
	once sync.Once
	addr chan string
}

func newStderrWatch() *stderrWatch { return &stderrWatch{addr: make(chan string, 1)} }

func (w *stderrWatch) Write(p []byte) (int, error) {
	n, err := w.lockedBuffer.Write(p)
	if m := listenLine.FindStringSubmatch(w.String()); m != nil {
		w.once.Do(func() { w.addr <- m[1] })
	}
	return n, err
}

// httpRun is a running ServeHTTP.
type httpRun struct {
	s      *srv
	addr   string // host:port bound
	url    string // the MCP endpoint
	stderr *stderrWatch
	cancel context.CancelFunc
	done   chan error

	stopped bool
}

// stop cancels the server and returns what ServeHTTP returned and how long
// it took.
func (r *httpRun) stop(t *testing.T) (time.Duration, error) {
	t.Helper()
	r.stopped = true
	start := time.Now()
	r.cancel()
	select {
	case err := <-r.done:
		return time.Since(start), err
	case <-time.After(15 * time.Second):
		t.Fatal("ServeHTTP did not return after cancel")
		return 0, nil
	}
}

// serveHTTP runs the HTTP transport on h (Addr defaults to 127.0.0.1:0)
// and waits until it listens. At cleanup it stops the server, which must
// return nil.
func (e *env) serveHTTP(t *testing.T, pol config.MCP, h HTTP) *httpRun {
	t.Helper()
	return e.serveHTTPWith(t, pol, h, nil)
}

// serveHTTPWith is serveHTTP with prep applied to the server first.
func (e *env) serveHTTPWith(t *testing.T, pol config.MCP, h HTTP, prep func(*srv)) *httpRun {
	t.Helper()
	if h.Addr == "" {
		h.Addr = "127.0.0.1:0"
	}
	r := &httpRun{stderr: newStderrWatch(), done: make(chan error, 1)}
	h.Stderr = r.stderr
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	s, err := newServer(ctx, Options{Manager: e.mgr, History: e.hist, Policy: pol, Version: "test"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	r.s = s
	if prep != nil {
		prep(s)
	}
	b, err := mcphttp.ParseBind(h.Addr)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { r.done <- s.listenHTTP(ctx, b, h) }()
	select {
	case r.addr = <-r.stderr.addr:
	case err := <-r.done:
		t.Fatalf("ServeHTTP returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP did not listen")
	}
	r.url = "http://" + r.addr + "/mcp"
	t.Cleanup(func() {
		if !r.stopped {
			if _, err := r.stop(t); err != nil {
				t.Errorf("ServeHTTP after cancel: %v", err)
			}
		}
	})
	return r
}

// httpClient is an initialized mcp-go Streamable HTTP client named name.
func httpClient(t *testing.T, url, name string, opts ...transport.StreamableHTTPCOption) (*client.Client, error) {
	t.Helper()
	c, err := client.NewStreamableHttpClient(url, opts...)
	if err != nil {
		t.Fatal(err)
	}
	// Start's context bounds the client's GET stream, if it opens one.
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: name, Version: "2.0"}
	if _, err := c.Initialize(ctx, init); err != nil {
		_ = c.Close()
		return nil, err
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, nil
}

func bearerHeader(tok string) transport.StreamableHTTPCOption {
	return transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + tok})
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"raw","version":"1"},"capabilities":{}}}`

// post sends an initialize request with the given Host (empty: the URL's)
// and headers.
func post(t *testing.T, url, host string, hdr map[string]string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

const cfgToken = "cfg-token-7d1e"

func TestHTTPServesWithTheToken(t *testing.T) {
	e := newEnv(t)
	r := e.serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)})
	c, err := httpClient(t, r.url, "http-client", bearerHeader(cfgToken))
	if err != nil {
		t.Fatalf("initialize with the token: %v", err)
	}
	var out struct {
		Rows [][]any `json:"rows"`
	}
	okJSON(t, call(t, c, "query", map[string]any{"connection": "lite", "query": "select id from orders order by id"}), &out)
	if len(out.Rows) != 2 {
		t.Errorf("rows = %v", out.Rows)
	}
	res := call(t, c, "query", map[string]any{"connection": "hidden", "query": "select 1"})
	if !res.IsError || !strings.Contains(text(t, res), "unknown connection") {
		t.Errorf("non-exposed over HTTP = %s", text(t, res))
	}
	// History has the calls with the session's client, as over stdio.
	got := historyMCP(t, e)
	if len(got) != 2 {
		t.Fatalf("history = %+v, want 2 entries", got)
	}
	for _, h := range got {
		if h.Source != "mcp" || h.Client != "http-client/2.0" {
			t.Errorf("entry %+v, want source mcp and client http-client/2.0", h)
		}
	}
	// A token from config is never printed.
	if s := r.stderr.String(); strings.Contains(s, cfgToken) || strings.Contains(s, "MCP HTTP token") {
		t.Errorf("stderr shows the configured token: %q", s)
	}
}

// Two sessions keep their own client names.
func TestHTTPClientNamePerSession(t *testing.T) {
	e := newEnv(t)
	r := e.serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)})
	a, err := httpClient(t, r.url, "alpha", bearerHeader(cfgToken))
	if err != nil {
		t.Fatal(err)
	}
	b, err := httpClient(t, r.url, "beta", bearerHeader(cfgToken))
	if err != nil {
		t.Fatal(err)
	}
	call(t, a, "list_connections", nil)
	call(t, b, "list_connections", nil)
	call(t, a, "list_connections", nil)
	var clients []string
	for _, h := range historyMCP(t, e) {
		clients = append(clients, h.Client)
	}
	if strings.Join(clients, ",") != "alpha/2.0,beta/2.0,alpha/2.0" {
		t.Errorf("clients = %v", clients)
	}
}

// A closed session (DELETE) drops its client name. (mcp-go's client sends
// its DELETE without the configured headers, so it is sent here by hand.)
func TestHTTPSessionEndForgetsTheClient(t *testing.T) {
	e := newEnv(t)
	r := e.serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)})
	auth := map[string]string{"Authorization": "Bearer " + cfgToken}
	resp := post(t, r.url, "", auth, initBody)
	id := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != http.StatusOK || id == "" {
		t.Fatalf("initialize: status %d, session %q", resp.StatusCode, id)
	}
	if n := countClients(r.s); n != 1 {
		t.Fatalf("clients = %d after initialize, want 1", n)
	}
	del := func(hdr map[string]string) int {
		req, err := http.NewRequest(http.MethodDelete, r.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if code := del(map[string]string{"Mcp-Session-Id": id}); code != http.StatusUnauthorized {
		t.Errorf("DELETE without the token: status %d, want 401", code)
	}
	if n := countClients(r.s); n != 1 {
		t.Errorf("clients = %d after an unauthorized DELETE, want 1", n)
	}
	if code := del(map[string]string{"Mcp-Session-Id": id, "Authorization": "Bearer " + cfgToken}); code != http.StatusOK {
		t.Errorf("DELETE: status %d", code)
	}
	if n := countClients(r.s); n != 0 {
		t.Errorf("clients = %d after the session closed, want 0", n)
	}
	// The ended session is gone: its ID is no longer accepted.
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	if resp := post(t, r.url, "", map[string]string{"Authorization": "Bearer " + cfgToken, "Mcp-Session-Id": id}, call); resp.StatusCode == http.StatusOK {
		t.Error("an ended session still works")
	}
	// So is one this server never issued.
	if resp := post(t, r.url, "", map[string]string{"Authorization": "Bearer " + cfgToken, "Mcp-Session-Id": "mcp-session-00000000-0000-0000-0000-000000000000"}, call); resp.StatusCode == http.StatusOK {
		t.Error("an unknown session ID works")
	}
}

func countClients(s *srv) int {
	n := 0
	s.clients.Range(func(any, any) bool { n++; return true })
	return n
}

var tokenLine = regexp.MustCompile(`MCP HTTP token: ([0-9a-f]+) \(pass as Authorization: Bearer …\)\n`)

func TestHTTPGeneratedTokenIsPrintedOnce(t *testing.T) {
	r := newEnv(t).serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{})
	ms := tokenLine.FindAllStringSubmatch(r.stderr.String(), -1)
	if len(ms) != 1 {
		t.Fatalf("stderr has %d token lines, want 1: %q", len(ms), r.stderr.String())
	}
	tok := ms[0][1]
	if len(tok) != 64 {
		t.Errorf("token %q is not 32 bytes of hex", tok)
	}
	if strings.Count(r.stderr.String(), tok) != 1 {
		t.Errorf("the token appears more than once: %q", r.stderr.String())
	}
	if _, err := httpClient(t, r.url, "gen", bearerHeader(tok)); err != nil {
		t.Errorf("the printed token does not work: %v", err)
	}
	if _, err := httpClient(t, r.url, "gen", bearerHeader(strings.Repeat("0", 64))); err == nil {
		t.Error("another token works")
	}
	// Each run gets its own token.
	r2 := newEnv(t).serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{})
	if m := tokenLine.FindStringSubmatch(r2.stderr.String()); m == nil || m[1] == tok {
		t.Errorf("second run's token = %v, want a fresh one", m)
	}
}

func TestHTTPRefusesWithoutTheToken(t *testing.T) {
	r := newEnv(t).serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)})
	for name, hdr := range map[string]map[string]string{
		"no token":    nil,
		"wrong token": {"Authorization": "Bearer nope"},
	} {
		resp := post(t, r.url, "", hdr, initBody)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: status %d, WWW-Authenticate %q; want 401 Bearer", name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	if _, err := httpClient(t, r.url, "anon"); err == nil {
		t.Error("the mcp-go client initialized without a token")
	}
	if _, err := httpClient(t, r.url, "anon", bearerHeader("wrong")); err == nil {
		t.Error("the mcp-go client initialized with a wrong token")
	}
	if resp := post(t, r.url, "", map[string]string{"Authorization": "Bearer " + cfgToken}, initBody); resp.StatusCode != http.StatusOK {
		t.Errorf("with the token: status %d", resp.StatusCode)
	}
}

func TestHTTPRejectsAForeignHostOrOrigin(t *testing.T) {
	r := newEnv(t).serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)})
	_, port, _ := net.SplitHostPort(r.addr)
	auth := map[string]string{"Authorization": "Bearer " + cfgToken}
	if resp := post(t, r.url, "evil.test:"+port, auth, initBody); resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host: status %d, want 403", resp.StatusCode)
	}
	if resp := post(t, r.url, "localhost:"+port, auth, initBody); resp.StatusCode != http.StatusOK {
		t.Errorf("localhost Host: status %d, want 200", resp.StatusCode)
	}
	evil := map[string]string{"Authorization": "Bearer " + cfgToken, "Origin": "https://evil.test"}
	if resp := post(t, r.url, "", evil, initBody); resp.StatusCode != http.StatusForbidden {
		t.Errorf("evil Origin: status %d, want 403", resp.StatusCode)
	}
	if _, err := httpClient(t, r.url, "page", transport.WithHTTPHeaders(evil)); err == nil {
		t.Error("the mcp-go client initialized with an evil Origin")
	}
	local := map[string]string{"Authorization": "Bearer " + cfgToken, "Origin": "http://localhost:3000"}
	if _, err := httpClient(t, r.url, "page", transport.WithHTTPHeaders(local)); err != nil {
		t.Errorf("a loopback Origin was refused: %v", err)
	}
}

func TestHTTPAllowedHost(t *testing.T) {
	r := newEnv(t).serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken), AllowedHosts: []string{"tobira.test"}})
	_, port, _ := net.SplitHostPort(r.addr)
	auth := map[string]string{"Authorization": "Bearer " + cfgToken}
	if resp := post(t, r.url, "tobira.test:"+port, auth, initBody); resp.StatusCode != http.StatusOK {
		t.Errorf("allowed host: status %d, want 200", resp.StatusCode)
	}
	if resp := post(t, r.url, "other.test:"+port, auth, initBody); resp.StatusCode != http.StatusForbidden {
		t.Errorf("other host: status %d, want 403", resp.StatusCode)
	}
}

func TestHTTPRefusesANonLoopbackBind(t *testing.T) {
	e := newEnv(t)
	var stderr lockedBuffer
	err := ServeHTTP(context.Background(), Options{Manager: e.mgr, Policy: config.MCP{Expose: []string{"lite"}}},
		HTTP{Addr: "0.0.0.0:0", Stderr: &stderr})
	if !errors.Is(err, mcphttp.ErrInsecureBind) {
		t.Fatalf("err = %v, want ErrInsecureBind", err)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q: it listened or made a token", stderr.String())
	}
	if err := ServeHTTP(context.Background(), Options{Manager: e.mgr, Policy: config.MCP{Expose: []string{"lite"}}},
		HTTP{Addr: "nonsense", Stderr: &stderr}); err == nil || errors.Is(err, mcphttp.ErrInsecureBind) {
		t.Errorf("a bad address = %v, want a parse error", err)
	}
}

func TestHTTPListenFailureIsAnError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	e := newEnv(t)
	err = ServeHTTP(context.Background(), Options{Manager: e.mgr, Policy: config.MCP{Expose: []string{"lite"}}},
		HTTP{Addr: ln.Addr().String(), Stderr: io.Discard})
	if err == nil {
		t.Fatal("ServeHTTP on a taken port returned nil")
	}
}

// Cancel shuts the server down within the 5s grace, even with a client
// holding a GET stream open, and the port is free afterwards.
func TestHTTPShutdownReleasesThePort(t *testing.T) {
	r := newEnv(t).serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)})
	c, err := httpClient(t, r.url, "listener", bearerHeader(cfgToken), transport.WithContinuousListening())
	if err != nil {
		t.Fatal(err)
	}
	call(t, c, "list_connections", nil)
	time.Sleep(200 * time.Millisecond) // let the GET stream open
	took, err := r.stop(t)
	if err != nil {
		t.Errorf("ServeHTTP after cancel = %v, want nil", err)
	}
	if took > 4*time.Second {
		t.Errorf("shutdown took %v: the open stream held it", took)
	}
	ln, err := net.Listen("tcp", r.addr)
	if err != nil {
		t.Fatalf("the port is still taken after shutdown: %v", err)
	}
	_ = ln.Close()
}

func TestHTTPLimitsTheBody(t *testing.T) {
	e := newEnv(t)
	r := e.serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)})
	auth := map[string]string{"Authorization": "Bearer " + cfgToken}
	big := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"pad":%q}}`, strings.Repeat("a", mcphttp.MaxBody))
	if resp := post(t, r.url, "", auth, big); resp.StatusCode == http.StatusOK {
		t.Error("a body over 4 MiB was served")
	}
}

// A connection that never sends a request does not hold shutdown either.
func TestHTTPShutdownIgnoresSilentConnections(t *testing.T) {
	r := newEnv(t).serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)})
	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	time.Sleep(100 * time.Millisecond) // let the server accept it
	took, err := r.stop(t)
	if err != nil {
		t.Errorf("ServeHTTP after cancel = %v, want nil", err)
	}
	if took > 4*time.Second {
		t.Errorf("shutdown took %v: the silent connection held it", took)
	}
}

// A localhost bind listens on 127.0.0.1, whatever the resolver says.
func TestHTTPLocalhostBindListensOnLoopbackIP(t *testing.T) {
	r := newEnv(t).serveHTTP(t, config.MCP{Expose: []string{"lite"}}, HTTP{Addr: "localhost:0", Token: driver.NewSecret(cfgToken)})
	if !strings.HasPrefix(r.addr, "127.0.0.1:") {
		t.Errorf("localhost:0 listened on %s, want 127.0.0.1", r.addr)
	}
	_, port, _ := net.SplitHostPort(r.addr)
	if resp := post(t, r.url, "localhost:"+port, map[string]string{"Authorization": "Bearer " + cfgToken}, initBody); resp.StatusCode != http.StatusOK {
		t.Errorf("Host localhost: status %d", resp.StatusCode)
	}
}

// A DELETE that mcp-go refused leaves the session's client name in place.
func TestEndSessionsOnlyWhenMCPGoAgreed(t *testing.T) {
	e := newEnv(t)
	s, err := newServer(context.Background(), Options{Manager: e.mgr, Policy: config.MCP{Expose: []string{"lite"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.mcp.RegisterSession(context.Background(), fakeSession{}); err != nil {
		t.Fatal(err)
	}
	s.clients.Store(fakeSession{}.SessionID(), "kept/1")
	del := func(code int) {
		h := s.endSessions(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
		req.Header.Set("Mcp-Session-Id", fakeSession{}.SessionID())
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	for _, code := range []int{http.StatusMethodNotAllowed, http.StatusInternalServerError, http.StatusBadRequest} {
		del(code)
		if n := countClients(s); n != 1 {
			t.Fatalf("after a DELETE answered %d: clients = %d, want 1", code, n)
		}
	}
	del(http.StatusOK)
	if n := countClients(s); n != 0 {
		t.Errorf("after a DELETE answered 200: clients = %d, want 0", n)
	}
}

// stuckTool adds a raw tool that ignores cancellation: it signals started,
// waits for release (or d), records a history entry as a finished call
// does, and sets done.
func stuckTool(s *srv, started chan<- struct{}, release <-chan struct{}, d time.Duration, done *atomic.Bool) {
	s.mcp.AddTool(mcp.NewTool("stuck"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-time.After(d):
		}
		s.record(ctx, "", "stuck call finished", true, d)
		done.Store(true)
		return mcp.NewToolResultText("late"), nil
	})
}

func callAsync(t *testing.T, url string) {
	t.Helper()
	c, err := httpClient(t, url, "late", bearerHeader(cfgToken))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		var req mcp.CallToolRequest
		req.Params.Name = "stuck"
		_, _ = c.CallTool(context.Background(), req) // the server goes away under it
	}()
}

// When the grace ends with a call still running, ServeHTTP waits for it
// (up to the drain bound) before returning, so the caller does not close
// history and the connections under it: the call keeps its history entry.
func TestHTTPShutdownWaitsForInFlightCalls(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{}, 1)
	var done atomic.Bool
	r := e.serveHTTPWith(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)}, func(s *srv) {
		s.grace, s.drain = 200*time.Millisecond, 3*time.Second
		stuckTool(s, started, nil, 900*time.Millisecond, &done)
	})
	callAsync(t, r.url)
	<-started
	took, err := r.stop(t)
	if err != nil {
		t.Errorf("ServeHTTP = %v", err)
	}
	if !done.Load() {
		t.Fatalf("ServeHTTP returned after %v with the call still running", took)
	}
	var found bool
	for _, h := range historyMCP(t, e) {
		found = found || h.Query == "stuck call finished"
	}
	if !found {
		t.Error("the call in flight lost its history entry")
	}
}

// The wait for calls in flight is bounded.
func TestHTTPShutdownDrainIsBounded(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	var done atomic.Bool
	r := e.serveHTTPWith(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)}, func(s *srv) {
		s.grace, s.drain = 200*time.Millisecond, 300*time.Millisecond
		stuckTool(s, started, release, time.Minute, &done)
	})
	callAsync(t, r.url)
	<-started
	took, err := r.stop(t)
	if err != nil {
		t.Errorf("ServeHTTP = %v", err)
	}
	if took > 2*time.Second || done.Load() {
		t.Errorf("ServeHTTP took %v (call done %v): the drain is not bounded", took, done.Load())
	}
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// A session idle for 30 minutes is dropped with its client name; activity
// keeps it alive.
func TestHTTPIdleSessionsExpire(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	r := newEnv(t).serveHTTPWith(t, config.MCP{Expose: []string{"lite"}}, HTTP{Token: driver.NewSecret(cfgToken)}, func(s *srv) { s.now = clock.Now })
	auth := map[string]string{"Authorization": "Bearer " + cfgToken}
	initialize := func() string {
		t.Helper()
		resp := post(t, r.url, "", auth, initBody)
		id := resp.Header.Get("Mcp-Session-Id")
		if resp.StatusCode != http.StatusOK || id == "" {
			t.Fatalf("initialize: status %d, session %q", resp.StatusCode, id)
		}
		return id
	}
	list := func(id string) int {
		t.Helper()
		return post(t, r.url, "", map[string]string{"Authorization": "Bearer " + cfgToken, "Mcp-Session-Id": id}, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`).StatusCode
	}
	old := initialize()
	for range 2 {
		clock.Advance(20 * time.Minute)
		if code := list(old); code != http.StatusOK {
			t.Fatalf("an active session (20 minutes idle): status %d", code)
		}
	}
	idle := initialize()
	clock.Advance(31 * time.Minute)
	r.s.sessions.sweep() // what the periodic sweep does
	if n := countClients(r.s); n != 0 {
		t.Errorf("clients = %d after 31 idle minutes, want 0", n)
	}
	for _, id := range []string{old, idle} {
		if code := list(id); code != http.StatusNotFound {
			t.Errorf("an expired session: status %d, want 404 (re-initialize)", code)
		}
	}
	// A request after expiry, without a sweep, also expires the session.
	fresh := initialize()
	clock.Advance(31 * time.Minute)
	if code := list(fresh); code != http.StatusNotFound {
		t.Errorf("an expired session without a sweep: status %d, want 404", code)
	}
	if n := countClients(r.s); n != 0 {
		t.Errorf("clients = %d, want 0", n)
	}
	if sessionIdle != 30*time.Minute {
		t.Errorf("sessionIdle = %v, want 30 minutes", sessionIdle)
	}
}
