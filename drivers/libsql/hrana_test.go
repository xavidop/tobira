package libsql

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// fakeServer is a scripted Hrana endpoint: reply builds the response for
// each pipeline request, and every request is recorded.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	reqs     []pipelineRequest
	auth     []string
	paths    []string
	reply    func(r pipelineRequest) (int, any)
	override http.HandlerFunc
}

// setReply and setHandler swap the behaviour under the lock, so a test can
// change it while the server goroutines read it.
func (f *fakeServer) setReply(fn func(r pipelineRequest) (int, any)) {
	f.mu.Lock()
	f.reply = fn
	f.mu.Unlock()
}

func (f *fakeServer) setHandler(h http.HandlerFunc) {
	f.mu.Lock()
	f.override = h
	f.mu.Unlock()
}

func newFake(t *testing.T, reply func(r pipelineRequest) (int, any)) *fakeServer {
	t.Helper()
	f := &fakeServer{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		h := f.override
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v2/pipeline") || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req pipelineRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.URL.Path)
		reply := f.reply
		f.mu.Unlock()
		status, out := reply(req)
		w.WriteHeader(status)
		if s, ok := out.(string); ok {
			_, _ = io.WriteString(w, s)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) requests() []pipelineRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pipelineRequest(nil), f.reqs...)
}

// okResults answers every request of r: execute with a "SELECT 1"-shaped
// result, batch with one empty result per step, close with close.
func okResults(r pipelineRequest) []map[string]any {
	out := make([]map[string]any, len(r.Requests))
	for i, q := range r.Requests {
		switch q.Type {
		case "execute":
			out[i] = map[string]any{"type": "ok", "response": map[string]any{"type": "execute", "result": map[string]any{
				"cols": []any{map[string]any{"name": "1", "decltype": nil}},
				"rows": []any{[]any{map[string]any{"type": "integer", "value": "1"}}},
			}}}
		case "batch":
			res := make([]any, len(q.Batch.Steps))
			errs := make([]any, len(q.Batch.Steps))
			for j := range res {
				res[j] = map[string]any{"cols": []any{}, "rows": []any{}}
			}
			out[i] = map[string]any{"type": "ok", "response": map[string]any{"type": "batch", "result": map[string]any{"step_results": res, "step_errors": errs}}}
		default:
			out[i] = map[string]any{"type": "ok", "response": map[string]any{"type": q.Type}}
		}
	}
	return out
}

func okReply(r pipelineRequest) (int, any) {
	return http.StatusOK, map[string]any{"baton": "b1", "base_url": nil, "results": okResults(r)}
}

func openFake(t *testing.T, f *fakeServer, cfg driver.ConnConfig) *conn {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = f.URL
	}
	cfg.Driver = Name
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

func TestTokenSentAsBearer(t *testing.T) {
	f := newFake(t, okReply)
	openFake(t, f, driver.ConnConfig{Password: driver.NewSecret("tok")})
	if got := f.auth[0]; got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	g := newFake(t, okReply)
	openFake(t, g, driver.ConnConfig{})
	if got := g.auth[0]; got != "" {
		t.Errorf("Authorization without a token = %q", got)
	}
}

func TestOpenBadTokenIsAuthError(t *testing.T) {
	f := newFake(t, func(pipelineRequest) (int, any) {
		return http.StatusUnauthorized, map[string]any{"error": "Unauthorized: `The JWT is invalid`"}
	})
	_, err := New().Open(context.Background(), driver.ConnConfig{Driver: Name, DSN: f.URL, Password: driver.NewSecret("wrong-tok")})
	if !errors.Is(err, driver.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
	if strings.Contains(err.Error(), "wrong-tok") {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestForbiddenIsPermissionError(t *testing.T) {
	f := newFake(t, okReply)
	c := openFake(t, f, driver.ConnConfig{})
	f.setReply(func(pipelineRequest) (int, any) {
		return http.StatusForbidden, map[string]any{"error": "Authorization forbidden: Current session doesn't not have Write permission to namespace default"}
	})
	_, err := c.Query(context.Background(), driver.Query{Text: "delete from t"})
	if !errors.Is(err, driver.ErrPermission) {
		t.Fatalf("got %v, want ErrPermission", err)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect target was contacted (Authorization %q)", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	f := newFake(t, func(pipelineRequest) (int, any) { return http.StatusOK, "" })
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v2/pipeline", http.StatusTemporaryRedirect)
	})
	_, err := New().Open(context.Background(), driver.ConnConfig{Driver: Name, DSN: f.URL, Password: driver.NewSecret("tok")})
	if err == nil {
		t.Fatal("open through a redirect succeeded")
	}
}

func TestBaseURLOnlySameOrigin(t *testing.T) {
	cl, err := newClient("https://db.example:8443", "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.close()
	for _, bad := range []string{"https://evil.example", "http://db.example:8443", "https://db.example:9999",
		"https://user@db.example:8443", "https://db.example:8443/x?y=1", "::not a url"} {
		if got := cl.adoptBaseURL(cl.base, bad); got != cl.base {
			t.Fatalf("after base_url %q the base is %q", bad, got)
		}
	}
	if got := cl.adoptBaseURL(cl.base, "https://db.example:8443/sticky/"); got != "https://db.example:8443/sticky" {
		t.Errorf("same-origin base_url not adopted: %q", got)
	}
}

func TestResponseCap(t *testing.T) {
	f := newFake(t, okReply)
	cl, err := newClient(f.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.close()
	cl.maxBytes = 64
	_, _, _, err = cl.pipeline(context.Background(), cl.base, "", []streamRequest{executeReq(hranaStmt{SQL: "select 1", WantRows: true})})
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("got %v, want errResponseTooLarge", err)
	}
}

func TestStreamFollowsBaton(t *testing.T) {
	n := 0
	f := newFake(t, func(r pipelineRequest) (int, any) {
		n++
		baton := any("b" + string(rune('0'+n)))
		if n == 3 {
			baton = nil // the server closes the stream
		}
		return http.StatusOK, map[string]any{"baton": baton, "results": okResults(r)}
	})
	cl, err := newClient(f.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.close()
	s := &stream{c: cl}
	ctx := context.Background()
	for range 3 {
		if _, err := s.send(ctx, executeReq(hranaStmt{SQL: "select 1"})); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.send(ctx, executeReq(hranaStmt{SQL: "select 1"})); !errors.Is(err, errStreamClosed) {
		t.Fatalf("send on a closed stream: %v", err)
	}
	reqs := f.requests()
	if reqs[0].Baton != nil || *reqs[1].Baton != "b1" || *reqs[2].Baton != "b2" {
		t.Errorf("batons sent: %v %v %v", reqs[0].Baton, reqs[1].Baton, reqs[2].Baton)
	}
}

func TestValueRoundTrip(t *testing.T) {
	for _, v := range []any{nil, int64(-42), int64(math.MaxInt64), 1.25, "héllo", []byte{0, 1, 2, 255}} {
		hv, err := encodeArg(v)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(hv)
		var back hranaValue
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		got, err := back.decode()
		if err != nil {
			t.Fatalf("%v: %v", v, err)
		}
		if b, ok := v.([]byte); ok {
			if string(got.([]byte)) != string(b) {
				t.Errorf("blob round trip: %v", got)
			}
			continue
		}
		if got != v {
			t.Errorf("round trip %#v -> %s -> %#v", v, data, got)
		}
	}
	if _, err := encodeArg(math.NaN()); err == nil {
		t.Error("NaN encoded")
	}
	// sqld sends blobs without padding.
	got, err := hranaValue{Type: "blob", Base64: "AAEC"}.decode()
	if err != nil || string(got.([]byte)) != "\x00\x01\x02" {
		t.Errorf("unpadded blob: %v %v", got, err)
	}
	if _, err := (hranaValue{Type: "integer", Value: "x"}).decode(); err == nil {
		t.Error("bad integer decoded")
	}
}

// The read-only path wraps the statement in BEGIN … ROLLBACK inside one
// batch, the statement conditional on BEGIN succeeding, on a fresh stream
// that is closed in the same pipeline.
func TestReadOnlyQueryShape(t *testing.T) {
	f := newFake(t, okReply)
	c := openFake(t, f, driver.ConnConfig{ReadOnly: true})
	rs, err := c.Query(context.Background(), driver.Query{Text: "select 1"})
	if err != nil {
		t.Fatal(err)
	}
	_ = rs.Close()
	reqs := f.requests()
	last := reqs[len(reqs)-1]
	if last.Baton != nil || len(last.Requests) != 2 || last.Requests[0].Type != "batch" || last.Requests[1].Type != "close" {
		t.Fatalf("pipeline = %+v", last)
	}
	steps := last.Requests[0].Batch.Steps
	if len(steps) != 3 || steps[0].Stmt.SQL != "BEGIN" || steps[1].Stmt.SQL != "select 1" || steps[2].Stmt.SQL != "ROLLBACK" {
		t.Fatalf("steps = %+v", steps)
	}
	if steps[0].Condition != nil || *steps[1].Condition != (hranaCond{Type: "ok", Step: 0}) || *steps[2].Condition != (hranaCond{Type: "ok", Step: 0}) {
		t.Errorf("conditions = %+v %+v %+v", steps[0].Condition, steps[1].Condition, steps[2].Condition)
	}
}

// When BEGIN fails, the statement is skipped and the query fails closed.
func TestReadOnlyQueryFailsClosedWhenBeginFails(t *testing.T) {
	f := newFake(t, okReply)
	c := openFake(t, f, driver.ConnConfig{ReadOnly: true})
	f.setReply(func(r pipelineRequest) (int, any) {
		res := okResults(r)
		res[0] = map[string]any{"type": "ok", "response": map[string]any{"type": "batch", "result": map[string]any{
			"step_results": []any{nil, nil, nil},
			"step_errors":  []any{map[string]any{"message": "SQLite error: no", "code": "SQLITE_UNKNOWN"}, nil, nil},
		}}}
		return http.StatusOK, map[string]any{"baton": nil, "results": res}
	})
	if _, err := c.Query(context.Background(), driver.Query{Text: "select 1"}); err == nil {
		t.Fatal("query succeeded although BEGIN failed")
	}
	f.setReply(func(r pipelineRequest) (int, any) { // statement skipped without an error
		res := okResults(r)
		res[0] = map[string]any{"type": "ok", "response": map[string]any{"type": "batch", "result": map[string]any{
			"step_results": []any{map[string]any{"cols": []any{}, "rows": []any{}}, nil, nil},
			"step_errors":  []any{nil, nil, nil},
		}}}
		return http.StatusOK, map[string]any{"baton": nil, "results": res}
	})
	if _, err := c.Query(context.Background(), driver.Query{Text: "select 1"}); err == nil {
		t.Fatal("query succeeded although the statement never ran")
	}
}

func TestQueryCancelledMidRequest(t *testing.T) {
	block := make(chan struct{})
	f := newFake(t, okReply)
	c := openFake(t, f, driver.ConnConfig{})
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Query(ctx, driver.Query{Text: "select 1"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("cancellation took %v", time.Since(start))
	}
}

func TestClosedConn(t *testing.T) {
	f := newFake(t, okReply)
	c := openFake(t, f, driver.ConnConfig{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := c.Query(context.Background(), driver.Query{Text: "select 1"}); !errors.Is(err, driver.ErrConnection) {
		t.Fatalf("query after close: %v", err)
	}
	if err := c.Ping(context.Background()); !errors.Is(err, driver.ErrConnection) {
		t.Fatalf("ping after close: %v", err)
	}
}
