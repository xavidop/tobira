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
	"time"

	"github.com/xavidop/tobira/driver"
)

// apiError is a non-2xx answer of the API.
type apiError struct {
	Status  int
	Message string // the API's message, with its validation errors appended
	// RateLimited: the answer is a primary (remaining 0) or secondary rate
	// limit. Reset is when the primary limit resets, RetryAfter how long a
	// secondary limit asks to wait; either may be zero.
	RateLimited bool
	Reset       time.Time
	RetryAfter  time.Duration
}

func (e *apiError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.RateLimited {
		switch {
		case !e.Reset.IsZero():
			msg += fmt.Sprintf(" (rate limit; resets in %s, at %s)", roundUp(e.Reset.Sub(now())), e.Reset.UTC().Format(time.RFC3339))
		case e.RetryAfter > 0:
			msg += fmt.Sprintf(" (secondary rate limit; retry after %s)", e.RetryAfter)
		default:
			msg += " (rate limit)"
		}
	}
	return msg
}

// roundUp rounds d up to whole seconds, never below one second.
func roundUp(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	return ((d + time.Second - 1) / time.Second) * time.Second
}

// errResponseTooLarge is returned (as driver.ErrLimit) for a body over
// maxResponseBytes.
var errResponseTooLarge = fmt.Errorf("the server response is too large (over %d MiB)", maxResponseBytes>>20)

// mapError classifies err as a *driver.Error. An *apiError is classified by
// its status, kept in Error.Code. When ctx is done, our own cancellation
// caused the failure: the error carries the ctx error (errors.Is works) and
// no kind. A *driver.Error passes through.
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
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return driver.Wrap(nil, Name, op, err)
	}
	var ae *apiError
	if errors.As(err, &ae) {
		kind := kindForHTTP(ae.Status)
		if ae.RateLimited {
			kind = driver.ErrLimit
		}
		return &driver.Error{Kind: kind, Driver: Name, Op: op, Code: strconv.Itoa(ae.Status), Err: errors.New(ae.Error())}
	}
	if errors.Is(err, errResponseTooLarge) {
		return driver.Wrap(driver.ErrLimit, Name, op, err)
	}
	if errors.Is(err, errClosed) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	if s := err.Error(); strings.Contains(s, "connection refused") || strings.Contains(s, "no such host") {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForHTTP maps an HTTP status to a driver error kind, or nil.
func kindForHTTP(code int) error {
	switch code {
	case http.StatusUnauthorized:
		return driver.ErrAuth
	case http.StatusForbidden:
		return driver.ErrPermission
	case http.StatusNotFound, http.StatusGone:
		return driver.ErrNotFound
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return driver.ErrSyntax
	case http.StatusConflict:
		return driver.ErrConflict
	case http.StatusNotImplemented:
		return driver.ErrUnsupported
	case http.StatusTooManyRequests:
		return driver.ErrLimit
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return driver.ErrConnection
	}
	return nil
}

// fail maps err and masks the DSN, the password and the token in its
// message, so no error a conn returns can carry the token.
func (c *conn) fail(ctx context.Context, op string, err error) error {
	return redact(mapError(ctx, op, err), c.cfg, c.client.token)
}

// redact masks the DSN, the password and the token in err's message,
// keeping its kind, code, op and context identity.
func redact(err error, cfg driver.ConnConfig, token string) error {
	if err == nil {
		return nil
	}
	var de *driver.Error
	if !errors.As(err, &de) || de.Err == nil {
		return err
	}
	msg := driver.RedactMessage(de.Err.Error(), cfg)
	if token != "" {
		for _, form := range []string{token, url.QueryEscape(token), url.PathEscape(token)} {
			msg = strings.ReplaceAll(msg, form, "xxxxx")
		}
	}
	if msg == de.Err.Error() {
		return err
	}
	out := *de
	out.Err = errors.New(msg)
	if ctxErr := contextCause(de.Err); ctxErr != nil {
		out.Err = errors.Join(out.Err, ctxErr)
	}
	return &out
}

// contextCause returns context.Canceled or context.DeadlineExceeded when err
// wraps one.
func contextCause(err error) error {
	for _, c := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, c) {
			return c
		}
	}
	return nil
}

// syntaxErr is an ErrSyntax for op.
func syntaxErr(op, format string, args ...any) error {
	return driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf(format, args...))
}

// unsupportedErr is an ErrUnsupported for op.
func unsupportedErr(op, format string, args ...any) error {
	return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf(format, args...))
}

// notFoundErr is an ErrNotFound for op.
func notFoundErr(op, format string, args ...any) error {
	return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf(format, args...))
}

// readOnlyErr is an ErrReadOnly for op.
func readOnlyErr(op, format string, args ...any) error {
	return driver.Wrap(driver.ErrReadOnly, Name, op, fmt.Errorf(format, args...))
}
