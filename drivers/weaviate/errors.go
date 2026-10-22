package weaviate

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/weaviate/weaviate-go-client/v5/weaviate/fault"

	"github.com/xavidop/tobira/driver"
)

// maxMessage caps a server message quoted in an error.
const maxMessage = 500

// mapError classifies err as a *driver.Error. A status reply is classified
// by its HTTP code, which is also stored in Error.Code ("404"). When ctx is
// done, our own cancellation caused the failure: the error carries the ctx
// error (errors.Is works) and no kind. A *driver.Error passes through.
func mapError(ctx context.Context, op string, err error) error {
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
				return driver.Wrap(nil, Name, op, err)
			}
			return driver.Wrap(nil, Name, op, errors.Join(err, ctxErr))
		}
	}
	var we *fault.WeaviateClientError
	if errors.As(err, &we) {
		if we.StatusCode > 0 {
			return statusError(op, we.StatusCode, []byte(we.Msg))
		}
		if we.DerivedFromError != nil {
			err = we.DerivedFromError
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return driver.Wrap(nil, Name, op, err)
	}
	if errors.Is(err, errTooBig) {
		return driver.Wrap(driver.ErrLimit, Name, op, errTooBig)
	}
	var netErr net.Error
	var urlErr *url.Error
	if errors.As(err, &netErr) || errors.As(err, &urlErr) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// statusError builds the error for a non-2xx reply: the server's message,
// classified by the status code.
func statusError(op string, code int, body []byte) error {
	msg := serverMessage(body)
	if msg == "" {
		msg = strings.ToLower(http.StatusText(code))
	}
	var kind error
	switch code {
	case http.StatusUnauthorized:
		kind = driver.ErrAuth
	case http.StatusForbidden:
		kind = driver.ErrPermission
	case http.StatusNotFound:
		kind = driver.ErrNotFound
	case http.StatusConflict:
		kind = driver.ErrConflict
	case http.StatusUnprocessableEntity:
		kind = driver.ErrSyntax
		if strings.Contains(strings.ToLower(msg), "already exists") {
			kind = driver.ErrConflict
		}
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		kind = driver.ErrSyntax
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		kind = driver.ErrConnection
	case http.StatusInternalServerError:
		// Weaviate answers a vector of the wrong size with a 500.
		if strings.Contains(strings.ToLower(msg), "dimensions do not match") {
			kind = driver.ErrSyntax
		}
	}
	return &driver.Error{Kind: kind, Driver: Name, Op: op, Code: strconv.Itoa(code), Err: errors.New(msg)}
}

// serverMessage extracts the message of a Weaviate error body:
// {"error":[{"message":…}]}, {"message":…} or plain text.
func serverMessage(body []byte) string {
	var e struct {
		Error []struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil {
		var parts []string
		for _, m := range e.Error {
			if m.Message != "" {
				parts = append(parts, m.Message)
			}
		}
		if len(parts) > 0 {
			return clip(strings.Join(parts, "; "))
		}
		if e.Message != "" {
			return clip(e.Message)
		}
	}
	return clip(strings.TrimSpace(string(body)))
}

func clip(s string) string {
	if len(s) > maxMessage {
		return s[:maxMessage] + "…"
	}
	return s
}

// graphQLError builds the error for a GraphQL reply carrying errors. A
// request the server could not read (an unknown field or argument, a type
// mismatch in a filter) is ErrSyntax.
func graphQLError(op string, msgs []string) error {
	msg := clip(strings.Join(msgs, "; "))
	low := strings.ToLower(msg)
	var kind error
	switch {
	case strings.Contains(low, "unauthorized") || strings.Contains(low, "anonymous access not enabled"):
		kind = driver.ErrAuth
	case strings.Contains(low, "forbidden"):
		kind = driver.ErrPermission
	default:
		for _, hint := range []string{"cannot query field", "syntax error", "unknown argument", "invalid", "expected", "must be", "cannot be set", "unknown type", "don't match", "dimension", "argument"} {
			if strings.Contains(low, hint) {
				kind = driver.ErrSyntax
				break
			}
		}
	}
	return &driver.Error{Kind: kind, Driver: Name, Op: op, Code: "graphql", Err: errors.New(msg)}
}
