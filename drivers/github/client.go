package github

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/driverutil/netpolicy"
)

const (
	// maxResponseBytes caps one response body.
	maxResponseBytes = 8 << 20
	// maxPerPage is GitHub's largest page.
	maxPerPage = 100
	// apiVersion is the REST API version the requests pin.
	apiVersion = "2022-11-28"
	// Bounds of the ETag cache: a 304 answer serves the cached body and
	// costs no rate limit.
	cacheEntries = 128
	cacheBytes   = 16 << 20
	// lowRateShare is the share of the rate limit below which a result
	// carries a notice.
	lowRateShare = 0.1
)

// client sends requests to one API endpoint. The HTTP transport is its own,
// so shutdown can cancel requests in flight and drop idle connections.
type client struct {
	http      *http.Client
	transport *http.Transport
	base      string // https://api.github.com, or an Enterprise Server's /api/v3, no trailing slash
	graphql   string // the GraphQL endpoint
	token     string
	userAgent string
	maxBytes  int64 // response cap, maxResponseBytes outside tests
	timeout   time.Duration

	closeCtx    context.Context
	closeCancel context.CancelFunc

	mu    sync.Mutex
	cache *etagCache
	rate  rateInfo // the last rate-limit headers seen
}

// rateInfo is what the last response said about the primary rate limit.
type rateInfo struct {
	limit, remaining int
	reset            time.Time
	resource         string
	seen             bool
}

func newClient(base, graphql, token string, timeout time.Duration) *client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	closeCtx, closeCancel := context.WithCancel(context.Background())
	return &client{
		transport: tr,
		http: &http.Client{
			Transport: tr,
			// A redirect is never followed, so the token cannot be carried to
			// another host.
			CheckRedirect: netpolicy.RefuseRedirects,
		},
		base:        strings.TrimRight(base, "/"),
		graphql:     graphql,
		token:       token,
		userAgent:   "tobira-github-driver/" + version,
		maxBytes:    maxResponseBytes,
		timeout:     timeout,
		closeCtx:    closeCtx,
		closeCancel: closeCancel,
		cache:       newETagCache(cacheEntries, cacheBytes),
	}
}

// shutdown cancels every request in flight, then drops idle connections.
func (c *client) shutdown() {
	c.closeCancel()
	c.transport.CloseIdleConnections()
}

// response is a decoded answer: the status, the headers and the body.
type response struct {
	status int
	header http.Header
	body   []byte
}

// hasNext reports a Link header with a rel="next" page.
func (r *response) hasNext() bool {
	for _, link := range r.header.Values("Link") {
		for _, part := range strings.Split(link, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}

// checkPath accepts an API-relative path: it starts with "/", holds no
// scheme, query, fragment or parent segment, so a request can only reach
// the configured endpoint.
func checkPath(p string) error {
	switch {
	case p == "" || p[0] != '/':
		return errors.New(`the path must start with "/" (it is relative to the API endpoint)`)
	case strings.Contains(p, "://"), strings.ContainsAny(p, "?#"):
		return errors.New("the path takes no scheme, host, query or fragment: give parameters in params")
	case strings.Contains(p, "//"), strings.Contains(p+"/", "/../"), strings.Contains(p+"/", "/./"):
		return errors.New("the path must not hold empty, . or .. segments")
	case strings.ContainsAny(p, " \t\r\n"):
		return errors.New("the path must not hold whitespace")
	}
	return nil
}

// url builds the request URL for an API path and its query.
func (c *client) url(path string, query url.Values) string {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// do sends one request. body, when not nil, is JSON-encoded. A GET is sent
// with If-None-Match when the cache holds the URL; a 304 answers from the
// cache. A status of 400 or more is an *apiError.
func (c *client) do(ctx context.Context, method, rawURL string, body any) (*response, error) {
	if c.closeCtx.Err() != nil {
		return nil, errClosed
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.closeCtx, cancel)
	defer stop()
	if c.timeout > 0 {
		var tcancel context.CancelFunc
		ctx, tcancel = context.WithTimeout(ctx, c.timeout)
		defer tcancel()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	var cached *cacheEntry
	if method == http.MethodGet {
		if cached = c.cache.get(rawURL); cached != nil {
			req.Header.Set("If-None-Match", cached.etag)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) && c.closeCtx.Err() != nil {
			return nil, errClosed
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	c.noteRate(resp.Header)
	if resp.StatusCode == http.StatusNotModified && cached != nil {
		return &response{status: cached.status, header: resp.Header, body: cached.body}, nil
	}
	if resp.ContentLength > c.maxBytes {
		return nil, errResponseTooLarge
	}
	var buf bytes.Buffer
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength) + 1)
	}
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, c.maxBytes+1)); err != nil {
		return nil, err
	}
	data := buf.Bytes()
	if int64(len(data)) > c.maxBytes {
		return nil, errResponseTooLarge
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, c.apiError(resp, data)
	}
	if method == http.MethodGet && resp.StatusCode == http.StatusOK {
		if etag := resp.Header.Get("ETag"); etag != "" {
			c.cache.put(rawURL, &cacheEntry{etag: etag, status: resp.StatusCode, body: data})
		}
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

// apiError builds the error for a non-2xx answer from its JSON body
// ({"message": …, "errors": [{"resource", "field", "code", "message"}]}),
// or from the status alone when the body is something else (a proxy page).
func (c *client) apiError(resp *http.Response, data []byte) error {
	e := &apiError{Status: resp.StatusCode}
	var body struct {
		Message string `json:"message"`
		Errors  []struct {
			Resource string `json:"resource"`
			Field    string `json:"field"`
			Code     string `json:"code"`
			Message  string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &body) == nil && body.Message != "" {
		e.Message = body.Message
		for _, ve := range body.Errors {
			detail := ve.Message
			if detail == "" {
				detail = strings.TrimSpace(strings.Join([]string{ve.Resource, ve.Field, ve.Code}, " "))
			}
			if detail != "" {
				e.Message += "; " + detail
			}
		}
	} else if text := strings.TrimSpace(string(data)); text != "" && len(text) <= 200 && !strings.HasPrefix(text, "<") {
		e.Message = text
	}
	e.Message = c.masked(e.Message)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		lower := strings.ToLower(e.Message)
		if resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("X-RateLimit-Remaining") == "0" || strings.Contains(lower, "rate limit") {
			e.RateLimited = true
			if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && resp.Header.Get("X-RateLimit-Remaining") == "0" {
				e.Reset = time.Unix(n, 0)
			}
			if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
				e.RetryAfter = time.Duration(n) * time.Second
			}
		}
	}
	return e
}

// masked replaces the token in a message: a proxy or a stand-in at an
// endpoint override might echo the Authorization header.
func (c *client) masked(msg string) string {
	if c.token == "" {
		return msg
	}
	for _, form := range []string{c.token, url.QueryEscape(c.token), url.PathEscape(c.token)} {
		msg = strings.ReplaceAll(msg, form, "xxxxx")
	}
	return msg
}

// noteRate records the rate-limit headers of a response.
func (c *client) noteRate(h http.Header) {
	limit, err1 := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	remaining, err2 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err1 != nil || err2 != nil {
		return
	}
	info := rateInfo{limit: limit, remaining: remaining, resource: h.Get("X-RateLimit-Resource"), seen: true}
	if n, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		info.reset = time.Unix(n, 0)
	}
	c.mu.Lock()
	c.rate = info
	c.mu.Unlock()
}

// rateNotice is a notice for a result when the rate limit runs low, or "".
func (c *client) rateNotice() string {
	c.mu.Lock()
	r := c.rate
	c.mu.Unlock()
	if !r.seen || r.limit <= 0 || float64(r.remaining) > lowRateShare*float64(r.limit) {
		return ""
	}
	res := r.resource
	if res == "" {
		res = "core"
	}
	msg := fmt.Sprintf("GitHub rate limit (%s): %d of %d requests left", res, r.remaining, r.limit)
	if !r.reset.IsZero() {
		msg += fmt.Sprintf(", resets in %s", roundUp(r.reset.Sub(now())))
	}
	return msg
}

// getJSON reads one API path and returns the raw JSON body.
func (c *client) getJSON(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	resp, err := c.do(ctx, http.MethodGet, c.url(path, query), nil)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(resp.body), nil
}

// page is one page of a list endpoint: its items and whether a next page
// exists. envelope names the member holding the items when the endpoint
// wraps them ({"total_count": …, "workflow_runs": […]}); "" is a bare array.
func (c *client) page(ctx context.Context, path string, query url.Values, envelope string) (items []json.RawMessage, next bool, err error) {
	resp, err := c.do(ctx, http.MethodGet, c.url(path, query), nil)
	if err != nil {
		return nil, false, err
	}
	items, err = decodeItems(resp.body, envelope)
	if err != nil {
		return nil, false, err
	}
	return items, resp.hasNext(), nil
}

// decodeItems reads the items of a list answer.
func decodeItems(body []byte, envelope string) ([]json.RawMessage, error) {
	data := json.RawMessage(body)
	if envelope != "" {
		var env map[string]json.RawMessage
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("the server answer is not a JSON object: %w", err)
		}
		var ok bool
		if data, ok = env[envelope]; !ok {
			return nil, fmt.Errorf("the server answer has no %q member", envelope)
		}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("the server answer is not a JSON array: %w", err)
	}
	return items, nil
}

// etagCache keeps the last answers of GET URLs with their ETags, bounded in
// entries and bytes, dropping the least recently used.
type etagCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	order      *list.List // front = most recent
	bytes      int64
	maxEntries int
	maxBytes   int64
}

type cacheEntry struct {
	url    string
	etag   string
	status int
	body   []byte
}

func newETagCache(maxEntries int, maxBytes int64) *etagCache {
	return &etagCache{entries: map[string]*list.Element{}, order: list.New(), maxEntries: maxEntries, maxBytes: maxBytes}
}

func (k *etagCache) get(url string) *cacheEntry {
	k.mu.Lock()
	defer k.mu.Unlock()
	el, ok := k.entries[url]
	if !ok {
		return nil
	}
	k.order.MoveToFront(el)
	return el.Value.(*cacheEntry)
}

func (k *etagCache) put(url string, e *cacheEntry) {
	if int64(len(e.body)) > k.maxBytes {
		return
	}
	e.url = url
	k.mu.Lock()
	defer k.mu.Unlock()
	if el, ok := k.entries[url]; ok {
		k.bytes -= int64(len(el.Value.(*cacheEntry).body))
		k.order.Remove(el)
		delete(k.entries, url)
	}
	k.entries[url] = k.order.PushFront(e)
	k.bytes += int64(len(e.body))
	for k.order.Len() > k.maxEntries || k.bytes > k.maxBytes {
		last := k.order.Back()
		old := last.Value.(*cacheEntry)
		k.bytes -= int64(len(old.body))
		k.order.Remove(last)
		delete(k.entries, old.url)
	}
}
