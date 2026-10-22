package mssql

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"io"
	"net"
	"strconv"

	mssqldb "github.com/microsoft/go-mssqldb"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. Context errors keep their
// identity (errors.Is still matches) and carry no kind. A server error is
// classified by its error number, which is also stored in Error.Code; an
// unknown number keeps its message and gets no kind. A lost or unusable
// connection is ErrConnection unless ctx is done, in which case our own
// cancellation caused it and the ctx error is joined instead.
func mapError(ctx context.Context, op string, err error) error {
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
	var ctxErr error
	if ctx != nil {
		ctxErr = ctx.Err()
	}
	var me mssqldb.Error
	if errors.As(err, &me) {
		code := strconv.Itoa(int(me.Number))
		if ctxErr != nil && me.Number == 3617 { // the attention our cancellation sent
			return &driver.Error{Driver: Name, Op: op, Code: code, Err: errors.Join(err, ctxErr)}
		}
		return &driver.Error{Kind: kindForCode(me.Number), Driver: Name, Op: op, Code: code, Err: err}
	}
	if ctxErr != nil {
		return &driver.Error{Driver: Name, Op: op, Err: errors.Join(err, ctxErr)}
	}
	var ne net.Error
	if errors.Is(err, sqldriver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) || errors.As(err, &ne) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	var se mssqldb.ServerError
	var st mssqldb.StreamError
	if errors.As(err, &se) || errors.As(err, &st) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForCode maps a SQL Server error number to a driver error kind, or nil.
func kindForCode(n int32) error {
	switch n {
	case 208, 2812, 911, 4060, 15151: // invalid object, no such procedure, no such database, cannot open database, cannot find object
		return driver.ErrNotFound
	case 18456, 18452, 18470, 18486, 18487, 18488: // login failed, untrusted domain, disabled, locked out, expired, must change
		return driver.ErrAuth
	case 102, 105, 156, 170, 319, 1038: // incorrect syntax, unclosed quotation, near keyword, …
		return driver.ErrSyntax
	case 2627, 2601, 547, 515: // unique/primary key, unique index, foreign key or check, NULL into NOT NULL
		return driver.ErrConstraint
	case 229, 230, 262, 297, 300, 916: // permission denied (object, column, statement), not a valid user
		return driver.ErrPermission
	case 1205, 1222, 3960: // deadlock victim, lock timeout, snapshot update conflict
		return driver.ErrConflict
	case 3906, 3908: // database is read-only
		return driver.ErrReadOnly
	}
	return nil
}
