package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// answer makes a hook that answers one path with a status, headers and
// body, letting every other request through.
func answer(path string, status int, headers map[string]string, body string) func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != path {
			return false
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return true
	}
}

func asDriverError(t *testing.T, err error) *driver.Error {
	t.Helper()
	var de *driver.Error
	if !errors.As(err, &de) {
		t.Fatalf("%v is not a *driver.Error", err)
	}
	return de
}

// A 404 is ErrNotFound with the status as its code.
func TestNotFoundCarriesCode404(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"get","collection":"issues","number":9999}`})
	wantKind(t, "a missing issue", err, driver.ErrNotFound)
	if de := asDriverError(t, err); de.Code != "404" || de.Driver != Name || de.Op != "query" {
		t.Errorf("error = %+v, want code 404 from github/query", de)
	}
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"rest","path":"/repos/octo/widgets/nothing"}`})
	wantKind(t, "a rest GET of nothing", err, driver.ErrNotFound)
	if !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("the error lacks GitHub's message: %v", err)
	}
}

// A 422 is ErrSyntax carrying the validation detail and code 422.
func TestValidationFailureIsErrSyntaxWithTheField(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"rest","method":"POST","path":"/repos/octo/widgets/issues","body":{"body":"no title"}}`})
	wantKind(t, "a POST without a required field", err, driver.ErrSyntax)
	msg := err.Error()
	if !strings.Contains(msg, "Validation Failed") || !strings.Contains(msg, "title") || !strings.Contains(msg, "missing_field") {
		t.Errorf("the error lacks the field detail: %v", err)
	}
	if de := asDriverError(t, err); de.Code != "422" {
		t.Errorf("code = %q, want 422", de.Code)
	}
	f.setHook(answer("/repos/octo/widgets/labels", http.StatusUnprocessableEntity, nil,
		`{"message":"Validation Failed","errors":[{"resource":"Label","code":"custom","field":"name","message":"name is too long (maximum is 50 characters)"}]}`))
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"rest","method":"POST","path":"/repos/octo/widgets/labels","body":{"name":"x"}}`})
	wantKind(t, "a validation error with a message", err, driver.ErrSyntax)
	if !strings.Contains(err.Error(), "name is too long") {
		t.Errorf("the error lacks the validation message: %v", err)
	}
}

// A 403 with the primary rate limit exhausted is ErrLimit naming the reset.
func TestPrimaryRateLimitIsErrLimitNamingTheReset(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	fixed := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	saved := now
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = saved })
	reset := fixed.Add(90 * time.Second)
	f.setHook(answer("/repos/octo/widgets/labels", http.StatusForbidden, map[string]string{
		"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": itoa(reset.Unix()), "X-RateLimit-Resource": "core",
	}, `{"message":"API rate limit exceeded for user ID 1. If you reach out to GitHub Support for help, please include the request ID.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`))
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"labels"}`})
	wantKind(t, "an exhausted rate limit", err, driver.ErrLimit)
	msg := err.Error()
	if !strings.Contains(msg, "resets in 1m30s") || !strings.Contains(msg, reset.Format(time.RFC3339)) {
		t.Errorf("the error does not name the reset: %v", err)
	}
	if de := asDriverError(t, err); de.Code != "403" {
		t.Errorf("code = %q, want 403", de.Code)
	}
	// Once the limit is exhausted every result would warn: the headers are
	// remembered, and a later success carries the notice.
	f.setHook(nil)
	f.setRate(5000, 0)
	_, _, stats := queryText(t, c, `{"collection":"labels"}`)
	if len(stats.Notices) != 1 || !strings.Contains(stats.Notices[0], "0 of 5000") {
		t.Errorf("notices = %v", stats.Notices)
	}
}

// A 429 secondary rate limit is ErrLimit with the Retry-After delay.
func TestSecondaryRateLimitIsErrLimit(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.setHook(answer("/repos/octo/widgets/labels", http.StatusTooManyRequests, map[string]string{"Retry-After": "60", "X-RateLimit-Remaining": "4000"},
		`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`))
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"labels"}`})
	wantKind(t, "a secondary rate limit", err, driver.ErrLimit)
	if !strings.Contains(err.Error(), "retry after 1m0s") {
		t.Errorf("the error does not carry Retry-After: %v", err)
	}
	// The same 403 secondary limit (remaining left) is ErrLimit by its
	// message, not ErrPermission.
	f.setHook(answer("/repos/octo/widgets/labels", http.StatusForbidden, map[string]string{"Retry-After": "30", "X-RateLimit-Remaining": "4000"},
		`{"message":"You have exceeded a secondary rate limit and have been temporarily blocked from content creation."}`))
	_, err = c.Query(context.Background(), driver.Query{Text: `{"collection":"labels"}`})
	wantKind(t, "a 403 secondary rate limit", err, driver.ErrLimit)
}

// A plain 403 is ErrPermission; a 5xx is ErrConnection; a 409 ErrConflict.
func TestOtherStatusesMapToTheirKinds(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	for _, tc := range []struct {
		status int
		body   string
		kind   error
	}{
		{http.StatusForbidden, `{"message":"Resource not accessible by integration"}`, driver.ErrPermission},
		{http.StatusBadGateway, `<html>Bad gateway</html>`, driver.ErrConnection},
		{http.StatusServiceUnavailable, `{"message":"Service Unavailable"}`, driver.ErrConnection},
		{http.StatusConflict, `{"message":"Git Repository is empty."}`, driver.ErrConflict},
		{http.StatusGone, `{"message":"Gone"}`, driver.ErrNotFound},
		{http.StatusNotImplemented, `{"message":"Not Implemented"}`, driver.ErrUnsupported},
		{http.StatusBadRequest, `{"message":"Problems parsing JSON"}`, driver.ErrSyntax},
	} {
		f.setHook(answer("/repos/octo/widgets/labels", tc.status, nil, tc.body))
		_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"labels"}`})
		if !errors.Is(err, tc.kind) {
			t.Errorf("status %d: %v, want %v", tc.status, err, tc.kind)
		}
		if de := asDriverError(t, err); de.Code != itoa(int64(tc.status)) {
			t.Errorf("status %d: code %q", tc.status, de.Code)
		}
		if strings.HasPrefix(tc.body, "<") && strings.Contains(err.Error(), "<html>") {
			t.Errorf("an HTML body is shown as the message: %v", err)
		}
	}
}

// An error body that echoes the Authorization header never leaks the token.
func TestErrorMessagesNeverCarryTheToken(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/repos/octo/widgets/labels" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad header Authorization: ` + r.Header.Get("Authorization") + `"}`))
		return true
	})
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"labels"}`})
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), fakeToken) {
		t.Errorf("the error carries the token: %v", err)
	}
	if !strings.Contains(err.Error(), "xxxxx") {
		t.Errorf("the token is not masked: %v", err)
	}
	// A GraphQL error echoing it is masked too.
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/graphql" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"token ` + r.Header.Get("Authorization") + ` rejected"}]}`))
		return true
	})
	_, err = c.Query(context.Background(), driver.Query{Text: `{"op":"graphql","query":"{ viewer { login } }"}`})
	if err == nil || strings.Contains(err.Error(), fakeToken) || !strings.Contains(err.Error(), "xxxxx") {
		t.Errorf("GraphQL error = %v", err)
	}
}

// A body over the response cap is ErrLimit instead of filling memory.
func TestOversizedBodyIsErrLimit(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	c.client.maxBytes = 1 << 20
	const total = 5<<20 + 1
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/repos/octo/widgets/labels" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		chunk := []byte(strings.Repeat("a", 64<<10))
		_, _ = w.Write([]byte(`["`))
		for written := 2; written < total; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return true
			}
		}
		_, _ = w.Write([]byte(`"]`))
		return true
	})
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"labels"}`})
	wantKind(t, "an oversized body", err, driver.ErrLimit)
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v", err)
	}
	// With a Content-Length the cap is enforced before reading the body.
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/repos/octo/widgets/labels" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", itoa(total))
		w.WriteHeader(http.StatusOK)
		chunk := []byte(strings.Repeat("b", 64<<10))
		for written := 0; written < total; written += len(chunk) {
			if _, err := w.Write(chunk[:min(len(chunk), total-written)]); err != nil {
				return true
			}
		}
		return true
	})
	_, err = c.Query(context.Background(), driver.Query{Text: `{"collection":"labels"}`})
	wantKind(t, "an oversized Content-Length", err, driver.ErrLimit)
	f.setHook(nil)
	if _, rows, _ := queryText(t, c, `{"collection":"labels"}`); len(rows) != 7 {
		t.Errorf("the connection is unusable after an oversized answer: %d rows", len(rows))
	}
}

// An unchanged listing is read again with If-None-Match and the 304 is
// served from the cache: same rows, no body.
func TestUnchangedListingIsServedFromTheETagCache(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.resetLog()
	_, first, _ := queryText(t, c, `{"collection":"labels"}`)
	_, second, _ := queryText(t, c, `{"collection":"labels"}`)
	reqs := f.requestsTo("/repos/octo/widgets/labels")
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	if reqs[0].Header.Get("If-None-Match") != "" || reqs[0].Status != http.StatusOK {
		t.Errorf("first request = %+v, want a plain GET answered 200", reqs[0])
	}
	if reqs[1].Header.Get("If-None-Match") == "" || reqs[1].Status != http.StatusNotModified {
		t.Errorf("second request = If-None-Match %q answered %d, want the ETag and a 304", reqs[1].Header.Get("If-None-Match"), reqs[1].Status)
	}
	if len(first) != 7 || len(second) != len(first) {
		t.Fatalf("%d then %d rows", len(first), len(second))
	}
	for i := range first {
		if first[i][0] != second[i][0] {
			t.Errorf("row %d differs after the 304: %v vs %v", i, first[i], second[i])
		}
	}
	// After a change the ETag no longer matches and the new body is read.
	queryText(t, c, `{"op":"rest","method":"POST","path":"/repos/octo/widgets/labels","body":{"name":"fresh"}}`)
	_, third, _ := queryText(t, c, `{"collection":"labels"}`)
	reqs = f.requestsTo("/repos/octo/widgets/labels")
	if len(third) != 8 || reqs[len(reqs)-1].Status != http.StatusOK || reqs[len(reqs)-1].Header.Get("If-None-Match") == "" {
		t.Errorf("after a write: %d rows, last request %+v", len(third), reqs[len(reqs)-1])
	}
	// Writes are never answered from the cache.
	for _, r := range f.writes() {
		if r.Header.Get("If-None-Match") != "" {
			t.Errorf("a write carried If-None-Match: %+v", r)
		}
	}
}

// Under a tenth of the rate limit left, every result carries a notice.
func TestLowRateLimitAddsANotice(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, _, stats := queryText(t, c, `{"collection":"labels"}`)
	if len(stats.Notices) != 0 {
		t.Errorf("notices with plenty left = %v", stats.Notices)
	}
	f.setRate(5000, 100)
	_, _, stats = queryText(t, c, `{"collection":"labels"}`)
	if len(stats.Notices) != 1 || !strings.Contains(stats.Notices[0], "100 of 5000") || !strings.Contains(stats.Notices[0], "core") {
		t.Errorf("notices = %v, want the low rate limit", stats.Notices)
	}
	rs, err := c.Browse(context.Background(), driver.CollectionRef{Name: "labels"}, driver.BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, bstats, _ := driver.Collect(context.Background(), rs, 0)
	if len(bstats.Notices) != 1 {
		t.Errorf("Browse notices = %v", bstats.Notices)
	}
	f.setRate(5000, 4000)
	_, _, stats = queryText(t, c, `{"op":"rate_limit"}`)
	if len(stats.Notices) != 0 {
		t.Errorf("notices after the limit recovered = %v", stats.Notices)
	}
}

// A redirect is not followed: the token must not travel to another host.
func TestRedirectsAreNotFollowed(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/repos/octo/widgets/labels" {
			return false
		}
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusMovedPermanently)
		return true
	})
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"labels"}`})
	if err == nil {
		t.Fatal("a redirected listing succeeded")
	}
	for _, r := range f.log() {
		if r.Path == "/elsewhere" {
			t.Error("the redirect was followed")
		}
	}
}
