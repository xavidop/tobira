package oracle

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/sijms/go-ora/v2/network"

	"github.com/xavidop/tobira/driver"
)

// oraCancelled is ORA-01013, the server's answer to a call broken off by the
// client (go-ora sends a break when the call's context ends).
const oraCancelled = 1013

// oraCode renders an Oracle error number the way Oracle prints it.
func oraCode(n int) string { return fmt.Sprintf("ORA-%05d", n) }

// mapError classifies err as a *driver.Error. Context errors keep their
// identity (errors.Is still matches) and carry no kind. A server error is
// classified by its ORA- number, which is also stored in Error.Code; an
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
	var oe *network.OracleError
	if errors.As(err, &oe) {
		code := oraCode(oe.ErrCode)
		if ctxErr != nil && (oe.ErrCode == oraCancelled || kindForCode(oe.ErrCode) == driver.ErrConnection) {
			return &driver.Error{Driver: Name, Op: op, Code: code, Err: errors.Join(err, ctxErr)}
		}
		return &driver.Error{Kind: kindForCode(oe.ErrCode), Driver: Name, Op: op, Code: code, Err: err}
	}
	var ne net.Error
	if errors.Is(err, sqldriver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) || errors.As(err, &ne) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
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

// kindForCode maps an Oracle error number to a driver error kind, or nil.
func kindForCode(n int) error {
	switch n {
	case 942, 4043, 904, 1918, 2289, 4080: // table/view, object, identifier, user, sequence, trigger does not exist
		return driver.ErrNotFound
	case 1017, 28000, 28001, 1005: // invalid credential, account locked, password expired, null password
		return driver.ErrAuth
	case 1031, 1045, 1749, 1720: // insufficient privileges, no CREATE SESSION, grant to self, grant option
		return driver.ErrPermission
	case 1456, 16000, 372: // write in a READ ONLY transaction, database open read-only, read-only tablespace
		return driver.ErrReadOnly
	case 1, 1400, 1407, 2290, 2291, 2292, 2293: // unique, NOT NULL, check, foreign key
		return driver.ErrConstraint
	case 60, 8177, 54, 30006, 1466: // deadlock, cannot serialize, resource busy (NOWAIT), wait timeout, table changed after the read-only snapshot
		return driver.ErrConflict
	case 3113, 3114, 3135, 12170, 12514, 12505, 12541, 12543, 12545, 12537, 12547:
		return driver.ErrConnection
	}
	if n >= 900 && n <= 999 && n != 942 && n != 904 { // ORA-009xx: parse errors
		return driver.ErrSyntax
	}
	return nil
}
