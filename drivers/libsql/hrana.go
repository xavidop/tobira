package libsql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/xavidop/tobira/driverutil/netpolicy"
)

// This file is a minimal client for Hrana over HTTP, version 2 (the
// /v2/pipeline endpoint that sqld and Turso serve). It is stdlib only, so
// the driver needs neither libsql-client-go nor cgo.

// maxResponseBytes caps one pipeline response. Hrana v2 returns a whole
// result at once, so an unbounded SELECT would otherwise be read into memory
// entirely. The decoded result has its own budget (decode.go).
const maxResponseBytes = 64 << 20

// errResponseTooLarge reports a response over maxResponseBytes.
var errResponseTooLarge = fmt.Errorf("the server's response exceeds %d MiB: add a LIMIT to the query", maxResponseBytes>>20)

// hranaValue is a Hrana value: null, integer (decimal text), float (a JSON
// number), text or blob (base64 on the wire). A decoded value (see its
// UnmarshalJSON) holds a string, a float64 or, for a blob, the bytes in
// Value.
type hranaValue struct {
	Type   string `json:"type"`
	Value  any    `json:"value,omitempty"`
	Base64 string `json:"base64,omitempty"`
}

// hranaStmt is one SQL statement with positional arguments.
type hranaStmt struct {
	SQL      string       `json:"sql"`
	Args     []hranaValue `json:"args,omitempty"`
	WantRows bool         `json:"want_rows"`
}

// hranaCond is a batch step condition: ok/error on an earlier step.
type hranaCond struct {
	Type string `json:"type"`
	Step int    `json:"step"`
}

func condOK(step int) *hranaCond    { return &hranaCond{Type: "ok", Step: step} }
func condError(step int) *hranaCond { return &hranaCond{Type: "error", Step: step} }

type hranaStep struct {
	Condition *hranaCond `json:"condition,omitempty"`
	Stmt      hranaStmt  `json:"stmt"`
}

type hranaBatch struct {
	Steps []hranaStep `json:"steps"`
}

// streamRequest is one entry of a pipeline: execute, batch or close.
type streamRequest struct {
	Type  string      `json:"type"`
	Stmt  *hranaStmt  `json:"stmt,omitempty"`
	Batch *hranaBatch `json:"batch,omitempty"`
}

func executeReq(s hranaStmt) streamRequest { return streamRequest{Type: "execute", Stmt: &s} }
func batchReq(steps ...hranaStep) streamRequest {
	return streamRequest{Type: "batch", Batch: &hranaBatch{Steps: steps}}
}
func closeReq() streamRequest { return streamRequest{Type: "close"} }

type pipelineRequest struct {
	Baton    *string         `json:"baton"`
	Requests []streamRequest `json:"requests"`
}

type hranaCol struct {
	Name     *string `json:"name"`
	DeclType *string `json:"decltype"`
}

// stmtResult is the result of one statement.
type stmtResult struct {
	Cols             []hranaCol     `json:"cols"`
	Rows             [][]hranaValue `json:"rows"`
	AffectedRowCount int64          `json:"affected_row_count"`
}

type batchResult struct {
	StepResults []*stmtResult `json:"step_results"`
	StepErrors  []*hranaError `json:"step_errors"`
}

type streamResponse struct {
	Type   string          `json:"type"`
	Result json.RawMessage `json:"result"`

	// Filled by decodeResults.
	stmt  *stmtResult
	batch *batchResult
	err   error
}

type streamResult struct {
	Type     string          `json:"type"` // "ok" or "error"
	Response *streamResponse `json:"response"`
	Error    *hranaError     `json:"error"`
}

type pipelineResponse struct {
	Baton   *string        `json:"baton"`
	BaseURL *string        `json:"base_url"`
	Results []streamResult `json:"results"`
}

// hranaError is an error the server reported: a statement error inside a
// result (Code such as SQLITE_CONSTRAINT or SQL_PARSE_ERROR), or a whole
// request refused with an HTTP status (Status != 0).
type hranaError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Status  int    `json:"-"`
}

func (e *hranaError) Error() string { return e.Message }

// client sends pipelines to one server. The HTTP transport is its own, so
// Close can drop its idle connections and their goroutines; shutdown also
// cancels requests still in flight.
type client struct {
	http      *http.Client
	transport *http.Transport
	maxBytes  int64  // response cap, maxResponseBytes outside tests
	base      string // scheme://host[:port][/path] of the DSN, no trailing slash
	origin    string // scheme://host[:port] of the DSN
	token     string

	closeCtx    context.Context
	closeCancel context.CancelFunc
}

func newClient(baseURL, token string) (*client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	closeCtx, closeCancel := context.WithCancel(context.Background())
	return &client{
		transport: tr,
		http: &http.Client{
			Transport: tr,
			// A pipeline is a POST: a redirect is never followed, so the
			// token cannot be carried to another endpoint.
			CheckRedirect: netpolicy.RefuseRedirects,
		},
		base:        strings.TrimRight(baseURL, "/"),
		origin:      u.Scheme + "://" + u.Host,
		token:       token,
		maxBytes:    maxResponseBytes,
		closeCtx:    closeCtx,
		closeCancel: closeCancel,
	}, nil
}

// close drops idle connections.
func (c *client) close() { c.transport.CloseIdleConnections() }

// shutdown cancels every request in flight, then drops idle connections.
func (c *client) shutdown() {
	c.closeCancel()
	c.close()
}

// adoptBaseURL returns the base a stream continues on after the server sent
// base_url raw (sqld's sticky sessions): raw when it is on the DSN's origin
// (same scheme, host and port, no userinfo, query or fragment), otherwise
// current. The token is never sent anywhere else.
func (c *client) adoptBaseURL(current, raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme+"://"+u.Host != c.origin || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return current
	}
	return strings.TrimRight(raw, "/")
}

// pipeline sends one pipeline request to base. baton is "" for a new
// stream. It returns the results, the stream's next baton ("" once the
// server closed it) and the base its next request goes to.
func (c *client) pipeline(ctx context.Context, base, baton string, reqs []streamRequest) ([]streamResult, string, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.closeCtx, cancel)
	defer stop()
	msg := pipelineRequest{Requests: reqs}
	if baton != "" {
		msg.Baton = &baton
	}
	if msg.Requests == nil {
		msg.Requests = []streamRequest{}
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, "", base, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/pipeline", bytes.NewReader(body))
	if err != nil {
		return nil, "", base, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", base, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength > c.maxBytes {
		return nil, "", base, errResponseTooLarge
	}
	var buf bytes.Buffer
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength) + 1) // one allocation instead of ReadAll's doubling
	}
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, c.maxBytes+1)); err != nil {
		return nil, "", base, err
	}
	data := buf.Bytes()
	if int64(len(data)) > c.maxBytes {
		return nil, "", base, errResponseTooLarge
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", base, httpError(resp.StatusCode, data)
	}
	var out pipelineResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, "", base, errors.New("the server sent an invalid pipeline response")
	}
	if len(out.Results) != len(reqs) {
		return nil, "", base, fmt.Errorf("the server answered %d of %d requests", len(out.Results), len(reqs))
	}
	decodeResults(out.Results)
	next := base
	if out.BaseURL != nil && *out.BaseURL != "" {
		next = c.adoptBaseURL(base, *out.BaseURL)
	}
	baton = ""
	if out.Baton != nil {
		baton = *out.Baton
	}
	return out.Results, baton, next, nil
}

// oneShot runs reqs on a new stream that reqs end with a close request.
func (c *client) oneShot(ctx context.Context, reqs ...streamRequest) ([]streamResult, error) {
	res, _, _, err := c.pipeline(ctx, c.base, "", reqs)
	return res, err
}

// httpError turns a non-200 response into a hranaError. The body is either
// {"error": "..."}, a Hrana error object or plain text.
func httpError(status int, body []byte) error {
	e := &hranaError{Status: status}
	var plain struct {
		Error string `json:"error"`
	}
	var obj hranaError
	switch {
	case json.Unmarshal(body, &plain) == nil && plain.Error != "":
		e.Message = truncateText(plain.Error, maxErrorBytes)
	case json.Unmarshal(body, &obj) == nil && obj.Message != "":
		e.Message, e.Code = obj.Message, obj.Code // already truncated
	default:
		e.Message = truncateText(strings.ToValidUTF8(strings.TrimSpace(string(body[:min(len(body), 4*maxErrorBytes)])), "?"), maxErrorBytes)
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	e.Message = fmt.Sprintf("HTTP %d: %s", status, e.Message)
	return e
}

// stream is a Hrana stream: one server-side connection, addressed by a baton
// that changes with every request. Requests on a stream are serialised.
type stream struct {
	c *client

	base string // where the next request goes: the DSN's base or a base_url the server sent

	mu     sync.Mutex
	baton  string
	opened bool // a request has run, so baton names the stream
	closed bool // the server or close ended it
}

var errStreamClosed = errors.New("the server closed the stream")

// send runs reqs on the stream.
func (s *stream) send(ctx context.Context, reqs ...streamRequest) ([]streamResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendLocked(ctx, reqs)
}

func (s *stream) sendLocked(ctx context.Context, reqs []streamRequest) ([]streamResult, error) {
	if s.closed {
		return nil, errStreamClosed
	}
	if s.base == "" {
		s.base = s.c.base
	}
	res, next, base, err := s.c.pipeline(ctx, s.base, s.baton, reqs)
	if err != nil {
		// Whether the server applied the requests is unknown: the stream
		// cannot be trusted any more.
		s.closed = true
		return nil, err
	}
	s.baton, s.base, s.opened = next, base, true
	if next == "" {
		s.closed = true
	}
	return res, nil
}

// close ends the stream, which rolls back any open transaction on it.
func (s *stream) close(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if !s.opened {
		return
	}
	_, _, _, _ = s.c.pipeline(ctx, s.base, s.baton, []streamRequest{closeReq()})
}

// keepAlive sends an empty pipeline, which resets the server's idle timer.
func (s *stream) keepAlive(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.opened {
		return
	}
	_, _ = s.sendLocked(ctx, nil)
}

// executeResult decodes an execute response, or returns its error.
func executeResult(r streamResult) (*stmtResult, error) {
	if r.Type == "error" || r.Error != nil {
		if r.Error == nil {
			return nil, errors.New("the server reported an error without a message")
		}
		return nil, r.Error
	}
	if r.Response == nil || r.Response.Type != "execute" {
		return nil, errors.New("the server sent an unexpected response type")
	}
	if r.Response.err != nil {
		return nil, r.Response.err
	}
	if r.Response.stmt == nil {
		return nil, errBadResult
	}
	return r.Response.stmt, nil
}

// batchOutcome decodes a batch response, or returns its error.
func batchOutcome(r streamResult, steps int) (*batchResult, error) {
	if r.Type == "error" || r.Error != nil {
		if r.Error == nil {
			return nil, errors.New("the server reported an error without a message")
		}
		return nil, r.Error
	}
	if r.Response == nil || r.Response.Type != "batch" {
		return nil, errors.New("the server sent an unexpected response type")
	}
	if r.Response.err != nil {
		return nil, r.Response.err
	}
	out := r.Response.batch
	if out == nil {
		return nil, errBadResult
	}
	if len(out.StepResults) != steps || len(out.StepErrors) != steps {
		return nil, fmt.Errorf("the server answered %d of %d batch steps", len(out.StepResults), steps)
	}
	return out, nil
}

// step returns step i's result: its error, or nil, nil when its condition
// skipped it.
func (b *batchResult) step(i int) (*stmtResult, error) {
	if e := b.StepErrors[i]; e != nil {
		return nil, e
	}
	return b.StepResults[i], nil
}

// encodeArg converts a database/sql-style argument (nil, int64, float64,
// string, []byte) to a Hrana value.
func encodeArg(v any) (hranaValue, error) {
	switch x := v.(type) {
	case nil:
		return hranaValue{Type: "null"}, nil
	case int64:
		return hranaValue{Type: "integer", Value: strconv.FormatInt(x, 10)}, nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return hranaValue{}, errors.New("NaN and infinite floats cannot be sent to the server")
		}
		return hranaValue{Type: "float", Value: x}, nil
	case string:
		return hranaValue{Type: "text", Value: x}, nil
	case []byte:
		return hranaValue{Type: "blob", Base64: base64.StdEncoding.EncodeToString(x)}, nil
	}
	return hranaValue{}, fmt.Errorf("unsupported argument type %T", v)
}

// decode converts a Hrana value to nil, int64, float64, string or []byte.
func (v hranaValue) decode() (any, error) {
	switch v.Type {
	case "null":
		return nil, nil
	case "integer":
		s, ok := v.Value.(string)
		if !ok {
			return nil, errors.New("invalid integer value")
		}
		return strconv.ParseInt(s, 10, 64)
	case "float":
		switch x := v.Value.(type) {
		case float64:
			return x, nil
		}
		return nil, errors.New("invalid float value")
	case "text":
		s, ok := v.Value.(string)
		if !ok {
			return nil, errors.New("invalid text value")
		}
		return s, nil
	case "blob":
		if b, ok := v.Value.([]byte); ok { // decoded by UnmarshalJSON
			return b, nil
		}
		b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(v.Base64, "="))
		if err != nil {
			return nil, errors.New("invalid blob value")
		}
		return b, nil
	}
	return nil, fmt.Errorf("unknown value type %q", v.Type)
}
