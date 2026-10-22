package d1

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. Context errors keep their
// identity (errors.Is still matches) and are wrapped without a kind. A D1
// or API error carries its Cloudflare error code (or, without one, its HTTP
// status) in Error.Code.
func mapError(op string, err error) error {
	if err == nil {
		return nil
	}
	var de *driver.Error
	if errors.As(err, &de) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return driver.Wrap(nil, Name, op, err)
	}
	var ae *apiError
	if errors.As(err, &ae) {
		code := ""
		switch {
		case ae.Code != 0:
			code = strconv.Itoa(ae.Code)
		case ae.Status != 0:
			code = strconv.Itoa(ae.Status)
		}
		return &driver.Error{Kind: apiKind(ae), Driver: Name, Op: op, Code: code, Err: err}
	}
	var kind error
	var ue *url.Error
	var ne net.Error
	switch {
	case errors.Is(err, errClosed), errors.Is(err, errNoResult), errors.Is(err, errBadResponse), errors.Is(err, errBadValue),
		errors.As(err, &ue), errors.As(err, &ne):
		kind = driver.ErrConnection
	case errors.Is(err, errResponseTooLarge), errors.Is(err, driver.ErrLimit):
		kind = driver.ErrLimit
	}
	return &driver.Error{Kind: kind, Driver: Name, Op: op, Err: err}
}

// apiKind classifies an API error by HTTP status and Cloudflare code, then
// by the SQLite message D1 passes on ("<message>: SQLITE_<CODE>").
func apiKind(e *apiError) error {
	switch {
	case e.Status == 401,
		e.Code == 10000, e.Code == 1000, // "Authentication error", "Invalid API Token"
		e.Code == 6003, e.Code == 6100, e.Code == 6111: // malformed Authorization header
		return driver.ErrAuth
	case e.Code == 7403, e.Code == 9109: // account not authorised for D1; token lacks the permission
		return driver.ErrPermission
	case e.Code == 7404, e.Code == 7003, e.Code == 7000: // database not found; no route for the URI
		return driver.ErrNotFound
	case e.Code == 7400: // malformed request
		return driver.ErrSyntax
	case e.Code == 971: // rate limited
		return driver.ErrConnection
	case e.Status == 403:
		return driver.ErrPermission
	case e.Status == 404:
		return driver.ErrNotFound
	case e.Status == 429, e.Status >= 500, e.Status >= 300 && e.Status < 400:
		return driver.ErrConnection
	}
	msg := strings.ToLower(e.Message)
	switch {
	case strings.Contains(msg, "no such table"), strings.Contains(msg, "no such column"),
		strings.Contains(msg, "no such index"), strings.Contains(msg, "no such view"):
		return driver.ErrNotFound
	case strings.Contains(msg, "sqlite_constraint"), strings.Contains(msg, "constraint failed"):
		return driver.ErrConstraint
	case strings.Contains(msg, "sqlite_readonly"), strings.Contains(msg, "readonly database"):
		return driver.ErrReadOnly
	case strings.Contains(msg, "sqlite_auth"), strings.Contains(msg, "not authorized"):
		return driver.ErrPermission
	case strings.Contains(msg, "sqlite_busy"), strings.Contains(msg, "database is locked"):
		return driver.ErrConflict
	case strings.Contains(msg, "syntax error"), strings.Contains(msg, "incomplete input"),
		strings.Contains(msg, "unrecognized token"):
		return driver.ErrSyntax
	case strings.Contains(msg, "overloaded"), strings.Contains(msg, "timed out"), strings.Contains(msg, "network connection lost"):
		return driver.ErrConnection
	}
	return nil
}

func errNoCollection(ns, name string) error {
	return errors.New("no such table or view: " + ns + "." + name)
}
