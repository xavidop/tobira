package snowflake

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"

	sf "github.com/snowflakedb/gosnowflake/v2"

	"github.com/xavidop/tobira/driver"
)

// Snowflake error numbers the driver classifies. Server errors are the
// documented ones; 26xxxx are gosnowflake's own client errors.
var kindByNumber = map[int]error{
	390100: driver.ErrAuth, // incorrect username or password
	390101: driver.ErrAuth, // user is disabled
	390102: driver.ErrAuth, // user is locked
	390103: driver.ErrAuth, // session token not found
	390104: driver.ErrAuth, // must log in again
	390144: driver.ErrAuth, // JWT token is invalid
	390186: driver.ErrAuth, // role not granted to the user
	390189: driver.ErrAuth, // role does not exist or is not authorized
	390195: driver.ErrAuth, // user cannot log in through this authenticator
	390303: driver.ErrAuth, // invalid OAuth access token
	390318: driver.ErrAuth, // OAuth access token expired
	390422: driver.ErrAuth, // incorrect authentication credentials
	394400: driver.ErrAuth, // programmatic access token is invalid
	260001: driver.ErrAuth, // client: user is empty
	260002: driver.ErrAuth, // client: password is empty
	2003:   driver.ErrNotFound,
	2043:   driver.ErrNotFound,
	904:    driver.ErrNotFound, // invalid identifier (a column)
	1003:   driver.ErrSyntax,
	1007:   driver.ErrSyntax,
	3001:   driver.ErrPermission,
	100072: driver.ErrConstraint, // NULL into a NOT NULL column
	2:      driver.ErrUnsupported,
}

// errorCancelled is SQL execution canceled.
const errorCancelled = 604

// mapError classifies err as a *driver.Error. Context errors keep their
// identity (errors.Is still matches) and carry no kind. A Snowflake error
// is classified by its number, then its SQLSTATE, which is also stored in
// Error.Code (the number when there is no SQLSTATE), so hosts recognise
// data exceptions (classes 22 and 23); an unknown error keeps its message
// and gets no kind. A lost connection is ErrConnection unless ctx is done,
// in which case our own cancellation caused it and the ctx error is joined
// instead.
func mapError(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var de *driver.Error
	if errors.As(err, &de) {
		return err
	}
	var ctxErr error
	if ctx != nil {
		ctxErr = ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if ctxErr != nil && !errors.Is(err, ctxErr) {
			err = errors.Join(err, ctxErr)
		}
		return driver.Wrap(nil, Name, op, err)
	}
	var se *sf.SnowflakeError
	if errors.As(err, &se) {
		code := se.SQLState
		if code == "" {
			code = strconv.Itoa(se.Number)
		}
		kind := kindForError(se)
		if ctxErr != nil && (se.Number == errorCancelled || kind == driver.ErrConnection || kind == nil) {
			return &driver.Error{Driver: Name, Op: op, Code: code, Err: errors.Join(err, ctxErr)}
		}
		return &driver.Error{Kind: kind, Driver: Name, Op: op, Code: code, Err: err}
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		if ctxErr != nil {
			return &driver.Error{Driver: Name, Op: op, Err: errors.Join(err, ctxErr)}
		}
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	if ctxErr != nil {
		return &driver.Error{Driver: Name, Op: op, Err: errors.Join(err, ctxErr)}
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForError maps a Snowflake error to a driver error kind, or nil.
func kindForError(se *sf.SnowflakeError) error {
	if k, ok := kindByNumber[se.Number]; ok {
		return k
	}
	switch st := se.SQLState; {
	case st == "28000" || st == "08004":
		return driver.ErrAuth
	case st == "42501":
		return driver.ErrPermission
	case st == "42S02":
		return driver.ErrNotFound
	case st == "0A000":
		return driver.ErrUnsupported
	case strings.HasPrefix(st, "23"):
		return driver.ErrConstraint
	case strings.HasPrefix(st, "08"):
		return driver.ErrConnection
	}
	return nil
}

// scrubError masks every secret of cfg (the password, private key or
// token, and anything password-like in the DSN) in the message of a
// *driver.Error, keeping its kind, code and the original error (through
// Unwrap).
func scrubError(err error, cfg driver.ConnConfig) error {
	var de *driver.Error
	if !errors.As(err, &de) || de.Err == nil {
		return err
	}
	msg := de.Err.Error()
	if clean := driver.RedactMessage(msg, cfg); clean != msg {
		de.Err = maskedError{msg: clean, err: de.Err}
	}
	return err
}

// maskedError shows a masked message but keeps the original error reachable.
type maskedError struct {
	msg string
	err error
}

func (m maskedError) Error() string { return m.msg }
func (m maskedError) Unwrap() error { return m.err }
