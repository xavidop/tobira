package libsql

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
// identity (errors.Is still matches) and are wrapped without a kind. A
// server error carries its Hrana code (SQLITE_CONSTRAINT, SQL_PARSE_ERROR,
// …) or, for a refused HTTP request, its status in Error.Code.
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
	var he *hranaError
	if errors.As(err, &he) {
		code := he.Code
		if code == "" && he.Status != 0 {
			code = strconv.Itoa(he.Status)
		}
		return &driver.Error{Kind: hranaKind(he), Driver: Name, Op: op, Code: code, Err: err}
	}
	var kind error
	var ue *url.Error
	var ne net.Error
	switch {
	case errors.Is(err, errStreamClosed), errors.As(err, &ue), errors.As(err, &ne):
		kind = driver.ErrConnection
	case errors.Is(err, errResponseTooLarge), errors.Is(err, driver.ErrLimit):
		kind = driver.ErrLimit
	}
	return &driver.Error{Kind: kind, Driver: Name, Op: op, Err: err}
}

// hranaKind classifies a server error by HTTP status, then Hrana code, then
// the SQLite message.
func hranaKind(e *hranaError) error {
	switch {
	case e.Status == 401:
		return driver.ErrAuth
	case e.Status == 403:
		return driver.ErrPermission
	case e.Status == 404:
		return driver.ErrNotFound
	case e.Status >= 500:
		return driver.ErrConnection
	}
	code := e.Code
	switch {
	case strings.HasPrefix(code, "SQLITE_CONSTRAINT"):
		return driver.ErrConstraint
	case strings.HasPrefix(code, "SQLITE_READONLY"):
		return driver.ErrReadOnly
	case strings.HasPrefix(code, "SQLITE_BUSY"), strings.HasPrefix(code, "SQLITE_LOCKED"):
		return driver.ErrConflict
	case code == "SQLITE_AUTH", code == "SQLITE_PERM":
		return driver.ErrPermission
	case code == "STREAM_EXPIRED", code == "STREAM_CLOSED", code == "STREAM_NOT_FOUND":
		return driver.ErrConnection
	case code == "SQL_PARSE_ERROR":
		if strings.Contains(e.Message, "unsupported statement") {
			return driver.ErrUnsupported
		}
		return driver.ErrSyntax
	case code == "SQL_MANY_STATEMENTS", code == "SQL_NO_STATEMENT", code == "ARGS_INVALID", code == "ARGS_BOTH_POSITIONAL_AND_NAMED":
		return driver.ErrSyntax
	}
	msg := strings.ToLower(e.Message)
	switch {
	case strings.Contains(msg, "no such table"), strings.Contains(msg, "no such column"),
		strings.Contains(msg, "no such index"), strings.Contains(msg, "no such database"),
		strings.Contains(msg, "unknown database"):
		return driver.ErrNotFound
	case strings.Contains(msg, "syntax error"), strings.Contains(msg, "incomplete input"),
		strings.Contains(msg, "unrecognized token"):
		return driver.ErrSyntax
	case strings.Contains(msg, "constraint failed"):
		return driver.ErrConstraint
	case strings.Contains(msg, "readonly database"):
		return driver.ErrReadOnly
	case strings.Contains(msg, "database is locked"):
		return driver.ErrConflict
	case strings.Contains(msg, "not authorized"):
		return driver.ErrPermission
	case strings.Contains(msg, "invalid baton"), strings.Contains(msg, "stream has expired"):
		return driver.ErrConnection
	}
	return nil
}

func errNoCollection(ns, name string) error {
	return errors.New("no such table or view: " + ns + "." + name)
}
