package d1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/xavidop/tobira/driverutil/netpolicy"
)

// errResponseTooLarge is returned (as driver.ErrUnsupported) for a body over
// maxResponseBytes.
var errResponseTooLarge = fmt.Errorf("the server's response exceeds %d MiB: add a LIMIT to the query", maxResponseBytes>>20)

// errNoResult is returned when a successful response holds no statement
// result, or more than the one statement sent.
var errNoResult = errors.New("the server did not return exactly one statement result")

// client sends statements to one D1 database's /raw endpoint. The HTTP
// transport is its own, so shutdown can cancel requests in flight and drop
// idle connections and their goroutines.
type client struct {
	http      *http.Client
	transport *http.Transport
	url       string // …/accounts/{account}/d1/database/{database}/raw
	token     string
	maxBytes  int64 // response cap, maxResponseBytes outside tests

	closeCtx    context.Context
	closeCancel context.CancelFunc
}

func newClient(url, token string) *client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	closeCtx, closeCancel := context.WithCancel(context.Background())
	return &client{
		transport: tr,
		http: &http.Client{
			Transport: tr,
			// A redirect is never followed, so the token cannot be carried to
			// another endpoint.
			CheckRedirect: netpolicy.RefuseRedirects,
		},
		url:         url,
		token:       token,
		maxBytes:    maxResponseBytes,
		closeCtx:    closeCtx,
		closeCancel: closeCancel,
	}
}

// shutdown cancels every request in flight, then drops idle connections.
func (c *client) shutdown() {
	c.closeCancel()
	c.transport.CloseIdleConnections()
}

// rawRequest is the /raw (and /query) request body.
type rawRequest struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
}

// exec sends one statement with its parameters and returns its result. A
// D1 or API error is an *apiError.
func (c *client) exec(ctx context.Context, sql string, params []any) (*stmtResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.closeCtx, cancel)
	defer stop()
	body, err := json.Marshal(rawRequest{SQL: sql, Params: params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) && c.closeCtx.Err() != nil {
			return nil, errClosed
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength > c.maxBytes {
		return nil, errResponseTooLarge
	}
	var buf bytes.Buffer
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength) + 1) // one allocation instead of ReadAll's doubling
	}
	if _, err := buf.ReadFrom(io.LimitReader(resp.Body, c.maxBytes+1)); err != nil {
		return nil, err
	}
	data := buf.Bytes()
	if int64(len(data)) > c.maxBytes {
		return nil, errResponseTooLarge
	}
	out, err := decodeResponse(data, newBudget(resultBudgetBytes, resultBudgetCells))
	if err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, c.masked(httpError(resp.StatusCode, data))
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || !out.Success || len(out.Errors) > 0 {
		e := &apiError{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		if len(out.Errors) > 0 {
			e.Code, e.Message = out.Errors[0].Code, out.Errors[0].Message
		}
		if e.Status == http.StatusOK && e.Message == http.StatusText(http.StatusOK) {
			e.Message = "the request did not succeed"
		}
		return nil, c.masked(e)
	}
	if len(out.Results) != 1 {
		return nil, errNoResult
	}
	return out.Results[0], nil
}

// httpError describes a non-200 response whose body is not a D1 envelope
// (a proxy's HTML page, say), keeping at most maxErrorBytes of it.
func httpError(status int, body []byte) error {
	msg := truncateText(strings.TrimSpace(string(body[:min(len(body), 4*maxErrorBytes)])), maxErrorBytes)
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &apiError{Status: status, Message: msg}
}

// masked replaces the token in an API error's message: a proxy or a stand-in
// at an endpoint override might echo the Authorization header.
func (c *client) masked(err error) error {
	var e *apiError
	if c.token != "" && errors.As(err, &e) {
		for _, form := range []string{c.token, url.QueryEscape(c.token), url.PathEscape(c.token)} {
			e.Message = strings.ReplaceAll(e.Message, form, "xxxxx")
		}
	}
	return err
}
