package mysql

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"io"
	"net"
	"strconv"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/xavidop/tobira/driver"
)

// mysqlQueryInterrupted is ER_QUERY_INTERRUPTED, the server's answer to a
// statement killed while it ran.
const mysqlQueryInterrupted = 1317

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
	var me *gomysql.MySQLError
	if errors.As(err, &me) {
		code := strconv.Itoa(int(me.Number))
		if me.Number == mysqlQueryInterrupted && ctxErr != nil {
			return &driver.Error{Driver: Name, Op: op, Code: code, Err: errors.Join(err, ctxErr)}
		}
		return &driver.Error{Kind: kindForCode(me.Number), Driver: Name, Op: op, Code: code, Err: err}
	}
	var ne net.Error
	if errors.Is(err, gomysql.ErrInvalidConn) || errors.Is(err, sqldriver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) ||
		errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		if ctxErr != nil {
			return &driver.Error{Driver: Name, Op: op, Err: errors.Join(err, ctxErr)}
		}
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForCode maps a MySQL error number to a driver error kind, or nil.
func kindForCode(n uint16) error {
	switch n {
	case 1792: // ER_CANT_EXECUTE_IN_READ_ONLY_TRANSACTION
		return driver.ErrReadOnly
	case 1045: // ER_ACCESS_DENIED_ERROR
		return driver.ErrAuth
	case 1146, 1049: // ER_NO_SUCH_TABLE, ER_BAD_DB_ERROR
		return driver.ErrNotFound
	case 1064: // ER_PARSE_ERROR
		return driver.ErrSyntax
	case 1062, 1451, 1452: // ER_DUP_ENTRY, ER_ROW_IS_REFERENCED_2, ER_NO_REFERENCED_ROW_2
		return driver.ErrConstraint
	case 1142, 1044: // ER_TABLEACCESS_DENIED_ERROR, ER_DBACCESS_DENIED_ERROR
		return driver.ErrPermission
	case 1213, 1205: // ER_LOCK_DEADLOCK, ER_LOCK_WAIT_TIMEOUT
		return driver.ErrConflict
	case 2002, 2003, 2013: // CR_CONNECTION_ERROR, CR_CONN_HOST_ERROR, CR_SERVER_LOST
		return driver.ErrConnection
	}
	return nil
}

// named returns err with the Driver of its *driver.Error set to the name of
// the driver that opened c, so the mariadb and singlestore drivers' errors
// carry their own name rather than "mysql". Errors are built per call, so
// changing the one in the chain is safe.
func (c *conn) named(err error) error {
	var de *driver.Error
	if c == nil || c.name == "" || c.name == Name || !errors.As(err, &de) || de.Driver != Name {
		return err
	}
	de.Driver = c.name
	return err
}
