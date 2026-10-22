package sqlite

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"modernc.org/sqlite"

	"github.com/xavidop/tobira/driver"
)

// SQLite primary result codes.
const (
	codeAuth       = 23
	codeBusy       = 5
	codeLocked     = 6
	codeReadOnly   = 8
	codeCantOpen   = 14
	codeConstraint = 19
	codePerm       = 3
	codeNotADB     = 26
)

// mapError classifies err as a *driver.Error. Context errors keep their
// identity (errors.Is still matches) and are wrapped without a kind.
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
	code := ""
	primary := -1
	var se *sqlite.Error
	if errors.As(err, &se) {
		code = strconv.Itoa(se.Code())
		primary = se.Code() & 0xff
	}
	msg := strings.ToLower(err.Error())
	var kind error
	switch primary {
	case codeReadOnly:
		kind = driver.ErrReadOnly
	case codeConstraint:
		kind = driver.ErrConstraint
	case codeAuth, codePerm:
		kind = driver.ErrPermission
	case codeBusy, codeLocked:
		kind = driver.ErrConflict
	case codeCantOpen, codeNotADB:
		kind = driver.ErrConnection
	case -1: // not a sqlite error: classify by message only
		switch {
		case strings.Contains(msg, "readonly database"):
			kind = driver.ErrReadOnly
		case strings.Contains(msg, "constraint failed"):
			kind = driver.ErrConstraint
		case strings.Contains(msg, "not authorized"):
			kind = driver.ErrPermission
		case strings.Contains(msg, "database is locked"):
			kind = driver.ErrConflict
		case strings.Contains(msg, "unable to open database"), strings.Contains(msg, "database is closed"):
			kind = driver.ErrConnection
		}
	}
	if kind == nil { // generic SQLITE_ERROR and friends: the message decides
		switch {
		case strings.Contains(msg, "no such table"), strings.Contains(msg, "no such column"),
			strings.Contains(msg, "no such index"), strings.Contains(msg, "no such database"),
			strings.Contains(msg, "unknown database"):
			kind = driver.ErrNotFound
		case strings.Contains(msg, "syntax error"), strings.Contains(msg, "incomplete input"),
			strings.Contains(msg, "unrecognized token"):
			kind = driver.ErrSyntax
		}
	}
	if code != "" { // drop modernc's trailing " (N)": Code carries it
		err = trimmedError{msg: strings.TrimSuffix(err.Error(), " ("+code+")"), err: err}
	}
	e := &driver.Error{Kind: kind, Driver: Name, Op: op, Code: code, Err: err}
	return e
}

func errNoCollection(ns, name string) error {
	return errors.New("no such table or view: " + ns + "." + name)
}

// trimmedError keeps the wrapped error reachable while overriding its text.
type trimmedError struct {
	msg string
	err error
}

func (t trimmedError) Error() string { return t.msg }
func (t trimmedError) Unwrap() error { return t.err }
