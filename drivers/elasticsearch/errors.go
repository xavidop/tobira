package elasticsearch

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// unknownProductMsg is the start of go-elasticsearch's error for a server
// that does not send Elastic's product header (OpenSearch, a proxy).
const unknownProductMsg = "the client noticed that the server is not Elasticsearch"

// esError is the error object of an Elasticsearch or OpenSearch response.
type esError struct {
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	RootCause []esError `json:"root_cause"`
}

// httpError turns a non-2xx response into a *driver.Error classified by
// its status. The Code is the server's error type (the root cause's when
// there is one, which names the actual problem of a search failure).
func httpError(name, op string, status int, body []byte) error {
	code, msg := "", ""
	var env struct {
		Error  json.RawMessage `json:"error"`
		Result string          `json:"result"`
	}
	if json.Unmarshal(body, &env) == nil {
		var e esError
		var s string
		switch {
		case json.Unmarshal(env.Error, &e) == nil && e.Type != "":
			code, msg = e.Type, e.Reason
			if len(e.RootCause) > 0 && e.RootCause[0].Type != "" {
				code, msg = e.RootCause[0].Type, e.RootCause[0].Reason
			}
		case json.Unmarshal(env.Error, &s) == nil && s != "":
			msg = s
		case env.Result == "not_found":
			msg = "document not found"
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d %s", status, http.StatusText(status))
	}
	if code == "search_context_missing_exception" {
		msg += " (the point in time expired: reload to browse again)"
	}
	return &driver.Error{Kind: kindForStatus(status), Driver: name, Op: op, Code: code, Err: errors.New(msg)}
}

// kindForStatus maps an HTTP status to a driver error kind, or nil.
func kindForStatus(status int) error {
	switch status {
	case http.StatusBadRequest:
		return driver.ErrSyntax
	case http.StatusUnauthorized:
		return driver.ErrAuth
	case http.StatusForbidden:
		return driver.ErrPermission
	case http.StatusNotFound:
		return driver.ErrNotFound
	case http.StatusConflict:
		return driver.ErrConflict
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return driver.ErrUnsupported
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return driver.ErrConnection
	}
	return nil
}

// mapError classifies a transport error. When ctx is done, our own
// cancellation caused it: the error carries the ctx error and no kind. A
// *driver.Error passes through.
func mapError(ctx context.Context, name, op string, err error) error {
	if err == nil {
		return nil
	}
	var de *driver.Error
	if errors.As(err, &de) {
		return err
	}
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(err, ctxErr) {
				return driver.Wrap(nil, name, op, err)
			}
			return driver.Wrap(nil, name, op, errors.Join(err, ctxErr))
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return driver.Wrap(nil, name, op, err)
	}
	if strings.HasPrefix(err.Error(), unknownProductMsg) {
		return driver.Wrap(driver.ErrConnection, name, op, errors.New("the server is not Elasticsearch (it sends no X-Elastic-Product header); for OpenSearch use an opensearch:// DSN"))
	}
	var (
		netErr  net.Error
		opErr   *net.OpError
		certErr *x509.UnknownAuthorityError
		hostErr x509.HostnameError
	)
	if errors.As(err, &netErr) || errors.As(err, &opErr) || errors.As(err, &certErr) || errors.As(err, &hostErr) ||
		strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "certificate") {
		return driver.Wrap(driver.ErrConnection, name, op, err)
	}
	return driver.Wrap(nil, name, op, err)
}
