package github

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// offlineClient is a client that never sends anything: the tests here only
// use its parsing and masking.
func offlineClient(t *testing.T, token string) *client {
	t.Helper()
	c := newClient("http://127.0.0.1:1", "http://127.0.0.1:1/graphql", token, 0)
	t.Cleanup(c.shutdown)
	return c
}

func TestETagCache(t *testing.T) {
	t.Parallel()
	entry := func(body string) *cacheEntry {
		return &cacheEntry{etag: `"` + body + `"`, status: http.StatusOK, body: []byte(body)}
	}
	t.Run("put and get", func(t *testing.T) {
		t.Parallel()
		k := newETagCache(2, 100)
		e := entry("aaaa")
		k.put("u1", e)
		if got := k.get("u1"); got != e || got.url != "u1" {
			t.Errorf("get = %+v, want the stored entry", got)
		}
		if got := k.get("missing"); got != nil {
			t.Errorf("get missing = %+v", got)
		}
		if k.bytes != 4 || k.order.Len() != 1 {
			t.Errorf("bytes %d, entries %d; want 4, 1", k.bytes, k.order.Len())
		}
	})
	t.Run("replacing an entry", func(t *testing.T) {
		t.Parallel()
		k := newETagCache(2, 100)
		k.put("u1", entry("aaaa"))
		e := entry("bbbbbb")
		k.put("u1", e)
		if got := k.get("u1"); got != e {
			t.Errorf("get = %+v, want the newer entry", got)
		}
		if k.bytes != 6 || k.order.Len() != 1 || len(k.entries) != 1 {
			t.Errorf("bytes %d, entries %d/%d; want 6, 1/1", k.bytes, k.order.Len(), len(k.entries))
		}
	})
	t.Run("least recently used leaves first", func(t *testing.T) {
		t.Parallel()
		k := newETagCache(2, 100)
		k.put("u1", entry("a"))
		k.put("u2", entry("b"))
		k.get("u1") // u1 is now the most recent
		k.put("u3", entry("c"))
		if k.get("u2") != nil {
			t.Error("u2 should have been evicted")
		}
		if k.get("u1") == nil || k.get("u3") == nil {
			t.Error("u1 and u3 should still be cached")
		}
		if k.order.Len() != 2 || len(k.entries) != 2 || k.bytes != 2 {
			t.Errorf("entries %d/%d, bytes %d; want 2/2, 2", k.order.Len(), len(k.entries), k.bytes)
		}
	})
	t.Run("bytes bound", func(t *testing.T) {
		t.Parallel()
		k := newETagCache(10, 10)
		k.put("u1", entry("aaaaaa"))
		k.put("u2", entry("bbbbbb"))
		if k.get("u1") != nil {
			t.Error("u1 should have been evicted to stay under the byte bound")
		}
		if k.get("u2") == nil || k.bytes != 6 || k.order.Len() != 1 {
			t.Errorf("u2 missing, or bytes %d, entries %d; want 6, 1", k.bytes, k.order.Len())
		}
		// A body of exactly the bound fits.
		k.put("u3", entry("cccccccccc"))
		if k.get("u3") == nil || k.get("u2") != nil || k.bytes != 10 {
			t.Errorf("exact-size body: u3 cached %v, u2 cached %v, bytes %d", k.get("u3") != nil, k.get("u2") != nil, k.bytes)
		}
	})
	t.Run("oversized body is not stored", func(t *testing.T) {
		t.Parallel()
		k := newETagCache(10, 10)
		k.put("u1", entry("aaaa"))
		k.put("big", entry("ccccccccccc"))
		if k.get("big") != nil {
			t.Error("an oversized body was cached")
		}
		if k.get("u1") == nil || k.bytes != 4 || k.order.Len() != 1 {
			t.Errorf("the oversized body disturbed the cache: bytes %d, entries %d", k.bytes, k.order.Len())
		}
	})
}

func TestCheckPath(t *testing.T) {
	t.Parallel()
	// A trailing slash is an ordinary path, not an empty segment.
	for _, p := range []string{"/", "/rate_limit", "/repos/octo/widgets/issues", "/repos/octo/widgets.js/git/refs/heads/main", "/a.b/c-d_e/f%20g", "/repos/o/r/.github", "/repos/octo/"} {
		if err := checkPath(p); err != nil {
			t.Errorf("checkPath(%q) = %v, want accepted", p, err)
		}
	}
	refused := map[string]string{
		"":                             `must start with "/"`,
		"rate_limit":                   `must start with "/"`,
		"https://api.github.com/x":     `must start with "/"`,
		"/x/https://evil.example.com":  "no scheme, host, query or fragment",
		"/x?per_page=1":                "no scheme, host, query or fragment",
		"/x#frag":                      "no scheme, host, query or fragment",
		"//evil.example.com/x":         "empty, . or .. segments",
		"/repos//octo":                 "empty, . or .. segments",
		"/repos/../admin":              "empty, . or .. segments",
		"/..":                          "empty, . or .. segments",
		"/repos/..":                    "empty, . or .. segments",
		"/repos/./octo":                "empty, . or .. segments",
		"/repos/.":                     "empty, . or .. segments",
		"/repos/o ctopus":              "whitespace",
		"/repos/octo\twidgets":         "whitespace",
		"/repos/octo\n":                "whitespace",
		"/repos/octo\r\nX-Injected: 1": "whitespace",
	}
	for p, hint := range refused {
		err := checkPath(p)
		if err == nil {
			t.Errorf("checkPath(%q) accepted, want refused", p)
			continue
		}
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("checkPath(%q) = %v, want %q", p, err, hint)
		}
	}
}

func TestHasNext(t *testing.T) {
	t.Parallel()
	link := func(values ...string) *response { return &response{header: http.Header{"Link": values}} }
	tests := []struct {
		name string
		r    *response
		want bool
	}{
		{"no header", &response{header: http.Header{}}, false},
		{"nil header", &response{}, false},
		{"next and last", link(`<https://api.github.com/repositories/1/issues?page=2>; rel="next", <https://api.github.com/repositories/1/issues?page=5>; rel="last"`), true},
		{"last only", link(`<https://api.github.com/repositories/1/issues?page=5>; rel="last"`), false},
		{"prev and first", link(`<https://api.github.com/x?page=1>; rel="prev", <https://api.github.com/x?page=1>; rel="first"`), false},
		{"next in a second header", link(`<https://api.github.com/x?page=1>; rel="prev"`, `<https://api.github.com/x?page=3>; rel="next"`), true},
		{"next in the URL only", link(`<https://api.github.com/x?rel=next>; rel="last"`), false},
		{"unquoted rel", link(`<https://api.github.com/x?page=2>; rel=next`), false},
	}
	for _, tc := range tests {
		if got := tc.r.hasNext(); got != tc.want {
			t.Errorf("%s: hasNext = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDecodeItems(t *testing.T) {
	t.Parallel()
	items, err := decodeItems([]byte(`[{"a":1},{"b":2}]`), "")
	if err != nil || len(items) != 2 || string(items[0]) != `{"a":1}` {
		t.Errorf("bare array = %v, %v", items, err)
	}
	items, err = decodeItems([]byte(`[]`), "")
	if err != nil || len(items) != 0 {
		t.Errorf("empty array = %v, %v", items, err)
	}
	items, err = decodeItems([]byte(`{"total_count": 1, "workflow_runs": [{"id": 1}]}`), "workflow_runs")
	if err != nil || len(items) != 1 || string(items[0]) != `{"id": 1}` {
		t.Errorf("envelope = %v, %v", items, err)
	}
	for _, tc := range []struct {
		body, envelope, hint string
	}{
		{`{"a":1}`, "", "not a JSON array"},
		{`"s"`, "", "not a JSON array"},
		{`null`, "", ""},
		{`[1,`, "", "not a JSON array"},
		{`[{"id":1}]`, "workflow_runs", "not a JSON object"},
		{`{"total_count": 1}`, "workflow_runs", `no "workflow_runs" member`},
		{`{"workflow_runs": {"id": 1}}`, "workflow_runs", "not a JSON array"},
		{`{"workflow_runs": null}`, "workflow_runs", ""},
	} {
		items, err := decodeItems([]byte(tc.body), tc.envelope)
		if tc.hint == "" {
			// A JSON null decodes as no items.
			if err != nil || len(items) != 0 {
				t.Errorf("decodeItems(%s, %q) = %v, %v; want no items", tc.body, tc.envelope, items, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.hint) {
			t.Errorf("decodeItems(%s, %q) = %v, want %q", tc.body, tc.envelope, err, tc.hint)
		}
	}
}

func TestAPIError(t *testing.T) {
	t.Parallel()
	c := offlineClient(t, "ghp_t0k3n")
	resp := func(status int, header map[string]string) *http.Response {
		r := &http.Response{StatusCode: status, Header: http.Header{}}
		for k, v := range header {
			r.Header.Set(k, v)
		}
		return r
	}
	asAPI := func(t *testing.T, err error) *apiError {
		t.Helper()
		var ae *apiError
		if !errors.As(err, &ae) {
			t.Fatalf("apiError returned %T", err)
		}
		return ae
	}
	t.Run("validation errors join the field and code", func(t *testing.T) {
		t.Parallel()
		body := `{"message":"Validation Failed","errors":[{"resource":"Issue","field":"title","code":"missing_field"},{"message":"custom text"},{"code":"custom"}],"documentation_url":"https://docs.github.com"}`
		e := asAPI(t, c.apiError(resp(422, nil), []byte(body)))
		if e.Status != 422 || e.Message != "Validation Failed; Issue title missing_field; custom text; custom" || e.RateLimited {
			t.Errorf("apiError = %+v", e)
		}
	})
	t.Run("short text body", func(t *testing.T) {
		t.Parallel()
		e := asAPI(t, c.apiError(resp(502, nil), []byte("  Bad gateway from the proxy\n")))
		if e.Message != "Bad gateway from the proxy" || e.Error() != "Bad gateway from the proxy" {
			t.Errorf("apiError = %+v", e)
		}
	})
	t.Run("html body is dropped", func(t *testing.T) {
		t.Parallel()
		e := asAPI(t, c.apiError(resp(502, nil), []byte("<html><body>oops</body></html>")))
		if e.Message != "" || e.Error() != "Bad Gateway" {
			t.Errorf("apiError = %+v, text %q", e, e.Error())
		}
	})
	t.Run("long body is dropped", func(t *testing.T) {
		t.Parallel()
		e := asAPI(t, c.apiError(resp(500, nil), []byte(strings.Repeat("x", 201))))
		if e.Message != "" || e.Error() != "Internal Server Error" {
			t.Errorf("apiError = %+v", e)
		}
	})
	t.Run("empty body", func(t *testing.T) {
		t.Parallel()
		e := asAPI(t, c.apiError(resp(404, nil), nil))
		if e.Message != "" || e.Error() != "Not Found" || e.RateLimited {
			t.Errorf("apiError = %+v", e)
		}
	})
	t.Run("json without a message is shown as text", func(t *testing.T) {
		t.Parallel()
		e := asAPI(t, c.apiError(resp(400, nil), []byte(`{"error":"bad"}`)))
		if e.Message != `{"error":"bad"}` {
			t.Errorf("apiError = %+v", e)
		}
	})
	t.Run("primary rate limit", func(t *testing.T) {
		t.Parallel()
		h := map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1700000000", "X-RateLimit-Limit": "60"}
		e := asAPI(t, c.apiError(resp(403, h), []byte(`{"message":"API rate limit exceeded for 1.2.3.4."}`)))
		if !e.RateLimited || !e.Reset.Equal(time.Unix(1700000000, 0)) || e.RetryAfter != 0 {
			t.Errorf("apiError = %+v", e)
		}
		if !strings.HasPrefix(e.Message, "API rate limit exceeded") {
			t.Errorf("message = %q", e.Message)
		}
	})
	t.Run("primary rate limit without a reset", func(t *testing.T) {
		t.Parallel()
		e := asAPI(t, c.apiError(resp(403, map[string]string{"X-RateLimit-Remaining": "0"}), []byte(`{"message":"API rate limit exceeded"}`)))
		if !e.RateLimited || !e.Reset.IsZero() || e.RetryAfter != 0 || e.Error() != "API rate limit exceeded (rate limit)" {
			t.Errorf("apiError = %+v, text %q", e, e.Error())
		}
	})
	t.Run("secondary rate limit", func(t *testing.T) {
		t.Parallel()
		e := asAPI(t, c.apiError(resp(429, map[string]string{"Retry-After": "60"}), []byte(`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)))
		if !e.RateLimited || e.RetryAfter != time.Minute || !e.Reset.IsZero() {
			t.Errorf("apiError = %+v", e)
		}
		if !strings.Contains(e.Error(), "(secondary rate limit; retry after 1m0s)") {
			t.Errorf("text = %q", e.Error())
		}
	})
	t.Run("rate limit by message only", func(t *testing.T) {
		t.Parallel()
		h := map[string]string{"X-RateLimit-Remaining": "5", "X-RateLimit-Reset": "1700000000", "Retry-After": "later"}
		e := asAPI(t, c.apiError(resp(403, h), []byte(`{"message":"You have triggered an abuse detection rate limit."}`)))
		if !e.RateLimited || !e.Reset.IsZero() || e.RetryAfter != 0 {
			t.Errorf("apiError = %+v", e)
		}
	})
	t.Run("forbidden without a rate limit", func(t *testing.T) {
		t.Parallel()
		e := asAPI(t, c.apiError(resp(403, nil), []byte(`{"message":"Resource not accessible by integration"}`)))
		if e.RateLimited || e.Message != "Resource not accessible by integration" {
			t.Errorf("apiError = %+v", e)
		}
	})
	t.Run("rate headers on another status are ignored", func(t *testing.T) {
		t.Parallel()
		e := asAPI(t, c.apiError(resp(404, map[string]string{"X-RateLimit-Remaining": "0"}), []byte(`{"message":"Not Found"}`)))
		if e.RateLimited {
			t.Errorf("apiError = %+v", e)
		}
	})
	t.Run("token is masked", func(t *testing.T) {
		t.Parallel()
		e := asAPI(t, c.apiError(resp(401, nil), []byte(`{"message":"Bad credentials for Bearer ghp_t0k3n","errors":[{"message":"token ghp_t0k3n rejected"}]}`)))
		if e.Message != "Bad credentials for Bearer xxxxx; token xxxxx rejected" {
			t.Errorf("message = %q", e.Message)
		}
		e = asAPI(t, c.apiError(resp(502, nil), []byte("proxy saw ghp_t0k3n")))
		if e.Message != "proxy saw xxxxx" {
			t.Errorf("message = %q", e.Message)
		}
	})
	t.Run("escaped token forms are masked", func(t *testing.T) {
		t.Parallel()
		const token = "s3cr+t/x"
		esc := offlineClient(t, token)
		body := fmt.Sprintf(`{"message":"plain %s query %s path %s"}`, token, url.QueryEscape(token), url.PathEscape(token))
		e := asAPI(t, esc.apiError(resp(401, nil), []byte(body)))
		if e.Message != "plain xxxxx query xxxxx path xxxxx" {
			t.Errorf("message = %q", e.Message)
		}
		if esc.masked("nothing here") != "nothing here" {
			t.Error("a message without the token changed")
		}
	})
	t.Run("without a token nothing is masked", func(t *testing.T) {
		t.Parallel()
		anon := offlineClient(t, "")
		if got := anon.masked("xxxxx ghp_t0k3n"); got != "xxxxx ghp_t0k3n" {
			t.Errorf("masked = %q", got)
		}
	})
}

// TestAPIErrorText pins the clock, so it does not run beside tests that read it.
func TestAPIErrorText(t *testing.T) {
	now = func() time.Time { return time.Unix(1700000000, 0) }
	t.Cleanup(func() { now = time.Now })
	tests := []struct {
		name string
		e    apiError
		want string
	}{
		{"message only", apiError{Status: 422, Message: "Validation Failed"}, "Validation Failed"},
		{"status text", apiError{Status: 404}, "Not Found"},
		{"unknown status", apiError{Status: 599}, ""},
		{"reset ahead", apiError{Status: 403, Message: "API rate limit exceeded", RateLimited: true, Reset: time.Unix(1700000090, 0)}, "API rate limit exceeded (rate limit; resets in 1m30s, at 2023-11-14T22:14:50Z)"},
		{"reset rounds up", apiError{Status: 403, Message: "m", RateLimited: true, Reset: time.Unix(1700000000, 500_000_000)}, "m (rate limit; resets in 1s, at 2023-11-14T22:13:20Z)"},
		{"reset just passed", apiError{Status: 403, Message: "m", RateLimited: true, Reset: time.Unix(1699999990, 0)}, "m (rate limit; resets in 1s, at 2023-11-14T22:13:10Z)"},
		{"retry after", apiError{Status: 429, Message: "slow down", RateLimited: true, RetryAfter: 90 * time.Second}, "slow down (secondary rate limit; retry after 1m30s)"},
		{"reset wins over retry after", apiError{Status: 403, Message: "m", RateLimited: true, Reset: time.Unix(1700000060, 0), RetryAfter: time.Second}, "m (rate limit; resets in 1m0s, at 2023-11-14T22:14:20Z)"},
		{"rate limited without details", apiError{Status: 403, RateLimited: true}, "Forbidden (rate limit)"},
	}
	for _, tc := range tests {
		e := tc.e
		if got := e.Error(); got != tc.want {
			t.Errorf("%s: Error() = %q, want %q", tc.name, got, tc.want)
		}
	}
	for d, want := range map[time.Duration]time.Duration{
		-time.Minute: time.Second, 0: time.Second, time.Millisecond: time.Second, time.Second: time.Second,
		1001 * time.Millisecond: 2 * time.Second, 90 * time.Second: 90 * time.Second,
	} {
		if got := roundUp(d); got != want {
			t.Errorf("roundUp(%s) = %s, want %s", d, got, want)
		}
	}
}

func TestMapError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if err := mapError(ctx, "query", nil); err != nil {
		t.Errorf("nil = %v", err)
	}
	// A classified error passes through untouched.
	already := driver.Wrap(driver.ErrNotFound, Name, "describe", errors.New("gone"))
	if got := mapError(ctx, "query", fmt.Errorf("wrapped: %w", already)); !errors.Is(got, driver.ErrNotFound) || driver.KindOf(got) != driver.ErrNotFound {
		t.Errorf("wrapped driver error = %v", got)
	}
	if got := mapError(ctx, "query", already); got != already {
		t.Errorf("driver error = %v, want the same error", got)
	}
	t.Run("http status", func(t *testing.T) {
		t.Parallel()
		kinds := map[int]error{
			400: driver.ErrSyntax, 401: driver.ErrAuth, 403: driver.ErrPermission, 404: driver.ErrNotFound, 409: driver.ErrConflict,
			410: driver.ErrNotFound, 422: driver.ErrSyntax, 429: driver.ErrLimit, 501: driver.ErrUnsupported, 502: driver.ErrConnection,
			503: driver.ErrConnection, 504: driver.ErrConnection, 500: nil, 418: nil,
		}
		for status, kind := range kinds {
			err := mapError(ctx, "query", &apiError{Status: status, Message: "m"})
			var de *driver.Error
			if !errors.As(err, &de) {
				t.Fatalf("%d: %T", status, err)
			}
			if driver.KindOf(err) != kind || de.Code != strconv.Itoa(status) || de.Op != "query" || de.Driver != Name || !strings.Contains(err.Error(), "m") {
				t.Errorf("%d: %+v (kind %v), want kind %v, code %d", status, de, driver.KindOf(err), kind, status)
			}
		}
		// A rate-limited answer is ErrLimit whatever its status.
		for _, status := range []int{403, 429} {
			if err := mapError(ctx, "query", &apiError{Status: status, RateLimited: true}); !errors.Is(err, driver.ErrLimit) {
				t.Errorf("rate limited %d = %v, want ErrLimit", status, err)
			}
		}
		// The message is rendered with the rate-limit note.
		err := mapError(ctx, "query", &apiError{Status: 429, Message: "slow", RateLimited: true, RetryAfter: time.Minute})
		if !strings.Contains(err.Error(), "retry after 1m0s") || !strings.Contains(err.Error(), "(429)") {
			t.Errorf("rate limited message = %q", err)
		}
	})
	t.Run("done context", func(t *testing.T) {
		t.Parallel()
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		for _, in := range []error{context.Canceled, fmt.Errorf("Get \"http://x\": %w", context.Canceled), errors.New("read tcp: use of closed network connection"), &apiError{Status: 500}} {
			err := mapError(cancelled, "query", in)
			if !errors.Is(err, context.Canceled) || driver.KindOf(err) != nil {
				t.Errorf("cancelled %v = %v (kind %v), want Canceled and no kind", in, err, driver.KindOf(err))
			}
			var de *driver.Error
			if !errors.As(err, &de) || de.Op != "query" || de.Driver != Name {
				t.Errorf("cancelled %v attribution = %+v", in, de)
			}
		}
		expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel2()
		if err := mapError(expired, "query", errors.New("dial tcp: i/o timeout")); !errors.Is(err, context.DeadlineExceeded) || driver.KindOf(err) != nil || !strings.Contains(err.Error(), "i/o timeout") {
			t.Errorf("expired = %v", err)
		}
		// The request's own timeout, with the caller's context still live.
		if err := mapError(ctx, "query", fmt.Errorf("Get \"http://x\": %w", context.DeadlineExceeded)); !errors.Is(err, context.DeadlineExceeded) || driver.KindOf(err) != nil {
			t.Errorf("request timeout = %v", err)
		}
		if err := mapError(ctx, "query", context.Canceled); !errors.Is(err, context.Canceled) || driver.KindOf(err) != nil {
			t.Errorf("cancelled error on a live context = %v", err)
		}
	})
	t.Run("other errors", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			err  error
			kind error
		}{
			{"response too large", errResponseTooLarge, driver.ErrLimit},
			{"wrapped response too large", fmt.Errorf("reading: %w", errResponseTooLarge), driver.ErrLimit},
			{"closed", errClosed, driver.ErrConnection},
			{"net.OpError", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, driver.ErrConnection},
			{"url.Error", &url.Error{Op: "Get", URL: "http://127.0.0.1:1/x", Err: errors.New("boom")}, driver.ErrConnection},
			{"connection refused text", errors.New("dial tcp 127.0.0.1:1: connection refused"), driver.ErrConnection},
			{"no such host text", errors.New("lookup ghe.example.invalid: no such host"), driver.ErrConnection},
			{"unclassified", errors.New("the server answer is not JSON"), nil},
		}
		for _, tc := range tests {
			err := mapError(ctx, "query", tc.err)
			var de *driver.Error
			if !errors.As(err, &de) {
				t.Errorf("%s: %T", tc.name, err)
				continue
			}
			if driver.KindOf(err) != tc.kind || de.Op != "query" || de.Driver != Name || !errors.Is(err, tc.err) {
				t.Errorf("%s: %+v (kind %v), want kind %v wrapping the cause", tc.name, de, driver.KindOf(err), tc.kind)
			}
		}
	})
}

func TestRedact(t *testing.T) {
	t.Parallel()
	const token = "s3cr+t/x"
	cfg := driver.ConnConfig{DSN: "github://:dsnp4ss@octo/widgets", Password: driver.NewSecret("p4ssword")}
	if redact(nil, cfg, token) != nil {
		t.Error("redact(nil) is not nil")
	}
	plain := errors.New("plain " + token)
	if got := redact(plain, cfg, token); got != plain {
		t.Errorf("an unclassified error = %v, want it unchanged", got)
	}
	clean := &driver.Error{Kind: driver.ErrNotFound, Driver: Name, Op: "get", Code: "404", Err: errors.New("no issue 7")}
	if got := redact(clean, cfg, token); got != clean {
		t.Errorf("an error without secrets = %v, want the same error", got)
	}
	in := &driver.Error{Kind: driver.ErrAuth, Driver: Name, Op: "ping", Code: "401", Err: fmt.Errorf("bad %s, %s, %s, %s, %s: %w", token, url.QueryEscape(token), url.PathEscape(token), cfg.DSN, "p4ssword", context.Canceled)}
	out := redact(in, cfg, token)
	var de *driver.Error
	if !errors.As(out, &de) || de == in {
		t.Fatalf("redact = %v (%T), want a new *driver.Error", out, out)
	}
	if de.Kind != driver.ErrAuth || de.Driver != Name || de.Op != "ping" || de.Code != "401" {
		t.Errorf("redact lost the classification: %+v", de)
	}
	if !errors.Is(out, driver.ErrAuth) || !errors.Is(out, context.Canceled) {
		t.Errorf("redact lost the kind or the context cause: %v", out)
	}
	msg := out.Error()
	for _, secret := range []string{token, url.QueryEscape(token), url.PathEscape(token), "dsnp4ss", "p4ssword"} {
		if strings.Contains(msg, secret) {
			t.Errorf("message %q carries %q", msg, secret)
		}
	}
	if !strings.Contains(msg, "bad xxxxx, xxxxx, xxxxx") || !strings.Contains(msg, "(401)") {
		t.Errorf("message = %q", msg)
	}
	// Without a token the DSN and password are still masked.
	out = redact(driver.Wrap(driver.ErrSyntax, Name, "open", errors.New("dsn github://:dsnp4ss@octo/widgets and p4ssword")), cfg, "")
	if s := out.Error(); strings.Contains(s, "dsnp4ss") || strings.Contains(s, "p4ssword") || !errors.Is(out, driver.ErrSyntax) {
		t.Errorf("redact without a token = %q", s)
	}
}

// TestRateNotice pins the clock, so it does not run beside tests that read it.
func TestRateNotice(t *testing.T) {
	now = func() time.Time { return time.Unix(1700000000, 0) }
	t.Cleanup(func() { now = time.Now })
	c := offlineClient(t, "")
	headers := func(limit, remaining, reset, resource string) http.Header {
		h := http.Header{}
		for k, v := range map[string]string{"X-RateLimit-Limit": limit, "X-RateLimit-Remaining": remaining, "X-RateLimit-Reset": reset, "X-RateLimit-Resource": resource} {
			if v != "" {
				h.Set(k, v)
			}
		}
		return h
	}
	if got := c.rateNotice(); got != "" {
		t.Errorf("before any response = %q", got)
	}
	tests := []struct {
		name                              string
		limit, remaining, reset, resource string
		want                              string
	}{
		{"plenty left", "5000", "4000", "", "", ""},
		{"just above a tenth", "5000", "501", "", "", ""},
		{"exactly a tenth", "5000", "500", "", "", "GitHub rate limit (core): 500 of 5000 requests left"},
		{"below a tenth", "5000", "499", "", "", "GitHub rate limit (core): 499 of 5000 requests left"},
		{"exhausted", "60", "0", "", "", "GitHub rate limit (core): 0 of 60 requests left"},
		{"with a reset", "5000", "10", "1700000090", "", "GitHub rate limit (core): 10 of 5000 requests left, resets in 1m30s"},
		{"reset in the past", "5000", "10", "1699999000", "", "GitHub rate limit (core): 10 of 5000 requests left, resets in 1s"},
		{"named resource", "30", "2", "", "search", "GitHub rate limit (search): 2 of 30 requests left"},
		{"zero limit", "0", "0", "", "", ""},
	}
	for _, tc := range tests {
		c.noteRate(headers(tc.limit, tc.remaining, tc.reset, tc.resource))
		if got := c.rateNotice(); got != tc.want {
			t.Errorf("%s: rateNotice = %q, want %q", tc.name, got, tc.want)
		}
	}
	// Headers without the counters leave the last reading in place.
	c.noteRate(headers("30", "2", "", "search"))
	c.noteRate(http.Header{})
	c.noteRate(headers("", "2", "", ""))
	c.noteRate(headers("x", "y", "", ""))
	if got := c.rateNotice(); got != "GitHub rate limit (search): 2 of 30 requests left" {
		t.Errorf("after headers without counters = %q", got)
	}
}
