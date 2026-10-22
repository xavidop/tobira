package vertica

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	vertigo "github.com/vertica/vertica-sql-go"

	"github.com/xavidop/tobira/driver"
)

// kindByState maps the SQLSTATEs the driver classifies. Vertica reports
// PostgreSQL-style SQLSTATEs, plus a few of its own (42V01 for a missing
// table).
var kindByState = map[string]error{
	"28000": driver.ErrAuth, "28P01": driver.ErrAuth, "08004": driver.ErrAuth,
	"42V01": driver.ErrNotFound, "42P01": driver.ErrNotFound, "42703": driver.ErrNotFound,
	"3F000": driver.ErrNotFound, "42883": driver.ErrNotFound, "42704": driver.ErrNotFound,
	"42601": driver.ErrSyntax,
	"42501": driver.ErrPermission,
	"25006": driver.ErrReadOnly, // a write in a READ ONLY transaction
	"0A000": driver.ErrUnsupported,
}

// errorReadOnly is Vertica's "Cannot issue this command in a read-only
// transaction", which it reports with SQLSTATE 26000 or 25006.
const errorReadOnly = "2448"

// stateCancelled is SQLSTATE query_canceled.
const stateCancelled = "57014"

// mapError classifies err as a *driver.Error. Context errors keep their
// identity (errors.Is still matches) and carry no kind. A Vertica error is
// classified by its SQLSTATE, which is also stored in Error.Code, so hosts
// recognise data exceptions (classes 22 and 23); an unknown error keeps
// its message and gets no kind. A lost connection is ErrConnection unless
// ctx is done, in which case our own cancellation caused it and the ctx
// error is joined instead.
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
	var ve *vertigo.VError
	if errors.As(err, &ve) {
		kind := kindForState(ve.SQLState)
		if ve.ErrorCode == errorReadOnly {
			kind = driver.ErrReadOnly
		}
		if ctxErr != nil && (ve.SQLState == stateCancelled || kind == driver.ErrConnection || kind == nil) {
			return &driver.Error{Driver: Name, Op: op, Code: ve.SQLState, Err: errors.Join(err, ctxErr)}
		}
		return &driver.Error{Kind: kind, Driver: Name, Op: op, Code: ve.SQLState, Err: err}
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || dialFailure(err) {
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

// dialFailure reports whether err is vertica-sql-go's "no host answered"
// error, a plain fmt error that wraps nothing.
func dialFailure(err error) bool {
	return strings.HasPrefix(err.Error(), "Failed to establish a connection")
}

// kindForState maps a SQLSTATE to a driver error kind, or nil.
func kindForState(st string) error {
	if k, ok := kindByState[st]; ok {
		return k
	}
	switch {
	case strings.HasPrefix(st, "23"):
		return driver.ErrConstraint
	case strings.HasPrefix(st, "08"):
		return driver.ErrConnection
	case strings.HasPrefix(st, "28"):
		return driver.ErrAuth
	case strings.HasPrefix(st, "53"), strings.HasPrefix(st, "54"):
		// Insufficient resources (out of memory) or a program limit.
		return driver.ErrLimit
	}
	return nil
}

// scrubError masks every secret of cfg (the password or token, and
// anything password-like in the DSN) in the message of a *driver.Error,
// keeping its kind, code and the original error (through Unwrap).
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
