package clickhouse

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/xavidop/tobira/driver"
)

// ClickHouse server error codes the driver classifies
// (src/Common/ErrorCodes.cpp).
const (
	codeQueryWasCancelled = 394
)

// mapError classifies err as a *driver.Error. Context errors keep their
// identity (errors.Is still matches) and carry no kind. A server exception
// is classified by its code, which is also stored in Error.Code; an unknown
// code keeps its message and gets no kind. A lost connection is
// ErrConnection unless ctx is done, in which case our own cancellation
// caused it and the ctx error is joined instead.
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
		if ctxErr != nil && !errors.Is(err, ctxErr) { // a detached call ended by ctx's deadline
			err = errors.Join(err, ctxErr)
		}
		return driver.Wrap(nil, Name, op, err)
	}
	var ex *ch.Exception
	if errors.As(err, &ex) {
		code := strconv.Itoa(int(ex.Code))
		if ctxErr != nil && (ex.Code == codeQueryWasCancelled || kindForCode(ex.Code) == driver.ErrConnection) {
			return &driver.Error{Driver: Name, Op: op, Code: code, Err: errors.Join(err, ctxErr)}
		}
		return &driver.Error{Kind: kindForCode(ex.Code), Driver: Name, Op: op, Code: code, Err: err}
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

// kindForCode maps a ClickHouse error code to a driver error kind, or nil.
func kindForCode(code int32) error {
	switch code {
	case 164: // READONLY
		return driver.ErrReadOnly
	case 516, 192, 193, 194: // AUTHENTICATION_FAILED, UNKNOWN_USER, WRONG_PASSWORD, REQUIRED_PASSWORD
		return driver.ErrAuth
	case 60, 81, 16: // UNKNOWN_TABLE, UNKNOWN_DATABASE, NO_SUCH_COLUMN_IN_TABLE
		return driver.ErrNotFound
	case 62: // SYNTAX_ERROR
		return driver.ErrSyntax
	case 497, 291: // ACCESS_DENIED, DATABASE_ACCESS_DENIED
		return driver.ErrPermission
	case 469: // VIOLATED_CONSTRAINT
		return driver.ErrConstraint
	case 1, 48: // UNSUPPORTED_METHOD, NOT_IMPLEMENTED
		return driver.ErrUnsupported
	case 209, 210: // SOCKET_TIMEOUT, NETWORK_ERROR
		return driver.ErrConnection
	}
	return nil
}
