package mssql

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// rollbackTimeout bounds the statements that end a call (post-check,
// ROLLBACK, cleanup), which run after the caller's ctx may be gone.
const rollbackTimeout = 5 * time.Second

// beginReadOnlySQL opens the read-only call's transaction, the guard's
// second layer: SQL Server has no read-only transaction, so the statement
// runs inside one that is always rolled back. When the database allows
// snapshot isolation the transaction uses it, so the reads see one
// consistent snapshot without blocking writers; otherwise it keeps the
// session's level (READ COMMITTED). A query that reaches another database
// under SNAPSHOT needs snapshot isolation there too (error 3952).
// IMPLICIT_TRANSACTIONS is turned off first, whatever the server's user
// options say, so the snapshot check does not open a transaction of its own.
const beginReadOnlySQL = "SET IMPLICIT_TRANSACTIONS OFF; SET NOCOUNT OFF; " +
	"IF EXISTS (SELECT 1 FROM sys.databases WHERE database_id = DB_ID() AND snapshot_isolation_state = 1) " +
	"SET TRANSACTION ISOLATION LEVEL SNAPSHOT; BEGIN TRANSACTION"

// endReadOnlySQL is the read-only call's post-check and end, in one batch:
// it reports @@TRANCOUNT, which must still be 1 (the statement did not end
// or nest the transaction), then rolls everything back and restores the
// default isolation level.
const endReadOnlySQL = "SELECT @@TRANCOUNT; IF @@TRANCOUNT > 0 ROLLBACK; SET TRANSACTION ISOLATION LEVEL READ COMMITTED"

// beginWritableSQL prepares a writable call's session: autocommit, and row
// counts on (Exec reports them), whatever the server's user options say.
const beginWritableSQL = "SET IMPLICIT_TRANSACTIONS OFF; SET NOCOUNT OFF"

// endWritableSQL rolls back a transaction a writable statement left open
// (BEGIN TRANSACTION without COMMIT): each Query is independent, and an
// open transaction would hold its locks while the connection sits in the
// pool. Use Begin for a transaction across statements.
const endWritableSQL = "IF @@TRANCOUNT > 0 ROLLBACK"

// useSQL is the USE statement of a namespace's database, or "" for the
// login database (a fresh session's).
func (c *conn) useSQL(ns string) (string, error) {
	if ns == "" {
		return "", nil
	}
	db, _, err := c.resolveNS(ns)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(db, c.loginDB) {
		return "", nil
	}
	return "USE " + quoteIdent(db), nil
}

// prefixSQL joins the statements that prepare a call's session.
func prefixSQL(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}

// endReadOnly is the read-only call's post-check, run once the statement's
// rows are closed: @@TRANCOUNT must still be 1, then everything is rolled
// back. A failed proof is ErrReadOnly. When the check cannot run, the
// connection is discarded (the server rolls back when it goes).
func (r *resultSet) endReadOnly() error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), rollbackTimeout)
	defer cancel()
	var n int64
	if err := r.sc.QueryRowContext(ctx, endReadOnlySQL).Scan(&n); err != nil {
		r.discard = true
		return nil
	}
	if n != 1 {
		r.discard = true
		return driver.Wrap(driver.ErrReadOnly, Name, "query",
			fmt.Errorf("the statement ended the read-only transaction (@@TRANCOUNT %d); the connection was discarded", n))
	}
	return nil
}

// endWritable cleans up after a writable statement on its own connection.
func (r *resultSet) endWritable() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), rollbackTimeout)
	defer cancel()
	if _, err := r.sc.ExecContext(ctx, endWritableSQL); err != nil {
		r.discard = true
	}
}

// discardConn makes database/sql close the connection instead of returning
// it to the pool, for a connection whose state the driver cannot vouch for.
func discardConn(sc *sql.Conn) {
	_ = sc.Raw(func(any) error { return sqldriver.ErrBadConn })
}

// errEnded is what a post-check reports when a statement left its
// transaction.
var errEnded = errors.New("a statement ended the transaction; roll back")
