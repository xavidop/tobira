package chroma

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// maxMessage caps the server message kept in an error.
const maxMessage = 500

// mapError classifies err as a *driver.Error. A Chroma reply is classified
// by its HTTP status (Error.Code is Chroma's error name, or the status).
// When ctx is done, our own cancellation caused the failure: the error
// carries the ctx error (errors.Is works) and no kind. Any other transport
// failure is ErrConnection. A *driver.Error passes through.
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
	if !errors.As(err, &ae) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	code := ae.name
	if code == "" {
		code = strconv.Itoa(ae.status)
	}
	msg := ae.message
	if len(msg) > maxMessage {
		cut := maxMessage
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + "…"
	}
	return &driver.Error{Kind: kindForStatus(ae.status, ae.name), Driver: Name, Op: op, Code: code, Err: errors.New(msg)}
}

// kindForStatus maps a Chroma reply to a driver error kind, or nil.
func kindForStatus(status int, name string) error {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return driver.ErrSyntax
	case http.StatusUnauthorized:
		return driver.ErrAuth
	case http.StatusForbidden:
		return driver.ErrPermission
	case http.StatusNotFound:
		return driver.ErrNotFound
	case http.StatusConflict:
		return driver.ErrConflict
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return driver.ErrConnection
	}
	if name == "InvalidArgumentError" {
		return driver.ErrSyntax
	}
	return nil
}

// fail maps err and masks the token in its message, keeping the kind,
// code, op and context identity.
func (c *conn) fail(ctx context.Context, op string, err error) error {
	err = mapError(ctx, op, err)
	if err == nil || c.token == "" {
		return err
	}
	var de *driver.Error
	if !errors.As(err, &de) {
		return err
	}
	out := *de
	out.Err = errors.New(maskToken(de.Err.Error(), c.token))
	if ctxErr := contextCause(de.Err); ctxErr != nil {
		out.Err = errors.Join(out.Err, ctxErr)
	}
	return &out
}
