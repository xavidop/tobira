package mysql

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/xavidop/tobira/driver"
)

// rollbackTimeout bounds the statements that end a read-only call (post-check
// and ROLLBACK), which run after the caller's ctx may be gone.
const rollbackTimeout = 5 * time.Second

// errCantChangeTx is ER_CANT_CHANGE_TX_CHARACTERISTICS: SET TRANSACTION
// without GLOBAL or SESSION fails this way only inside an active transaction,
// which makes it the probe for "still in our transaction".
const errCantChangeTx = 1568

// sessionCheckSQL reads back the session's read-only state in the post-check.
const sessionCheckSQL = "SELECT @@SESSION.transaction_read_only"

// withReadOnlyTx runs fn inside START TRANSACTION READ ONLY and always ends
// with ROLLBACK. Catalog reads use it on read-only and writable connections
// alike: they only run the driver's own SQL, so the connection goes back to
// the pool afterwards.
func (c *conn) withReadOnlyTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// beginReadOnly opens the read-only call on the result set's dedicated
// connection: SET SESSION TRANSACTION READ ONLY (the session layer, which
// read-only connections also get from the DSN), then START TRANSACTION READ
// ONLY. With an allow-list (check), it then checks that the session's client
// charset is ASCII-transparent (the guard's lexer assumes a 0x5c byte is
// always a backslash) and, when text holds a backslash, re-runs the
// allow-list with the session's sql_mode, since NO_BACKSLASH_ESCAPES and
// ANSI_QUOTES change where quoted text ends. On MariaDB the sql_mode must
// also pass mariaDBModeRefusal, whatever the text: MSSQL mode reads [ ] as
// identifier quotes, which the lexer does not know.
func (r *resultSet) beginReadOnly(text string, check checker) error {
	for _, stmt := range []string{"SET SESSION TRANSACTION READ ONLY", "START TRANSACTION READ ONLY"} {
		if _, err := r.sc.ExecContext(r.qctx, stmt); err != nil {
			return err
		}
	}
	r.inTx = true
	if check == nil {
		return nil
	}
	var sqlMode, charset string
	if err := r.sc.QueryRowContext(r.qctx, "SELECT @@SESSION.sql_mode, @@SESSION.character_set_client").Scan(&sqlMode, &charset); err != nil {
		return err
	}
	if !asciiSafeCharsets[strings.ToLower(charset)] {
		return driver.Wrap(driver.ErrUnsupported, Name, "query",
			fmt.Errorf("client charset %q is not supported on a read-only path: the guard needs an ASCII-transparent charset", charset))
	}
	if r.c.onMariaDB() {
		if err := mariaDBModeRefusal(sqlMode); err != nil {
			return driver.Wrap(driver.ErrUnsupported, Name, "query", err)
		}
	}
	if mode := modeFromSQLMode(sqlMode); mode != (lexMode{}) && strings.ContainsRune(text, '\\') {
		return check(text, mode)
	}
	return nil
}

// asciiSafeCharsets are the client charsets in which a 0x5c byte is always a
// backslash, as the guard's lexer assumes.
var asciiSafeCharsets = map[string]bool{"utf8mb4": true, "utf8mb3": true, "utf8": true, "ascii": true, "latin1": true}

// endReadOnly is the read-only call's post-check, run once the statement's
// rows are closed on a live connection. It proves the statement is still
// inside the READ ONLY transaction (SET TRANSACTION must fail with 1568) and
// that the session is still read-only, then rolls back. A failed proof is
// ErrReadOnly. The connection is discarded afterwards either way.
func (r *resultSet) endReadOnly() error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), rollbackTimeout)
	defer cancel()
	var escaped error
	_, err := r.sc.ExecContext(ctx, "SET TRANSACTION READ ONLY")
	var me *gomysql.MySQLError
	switch {
	case err == nil:
		escaped = errors.New("the statement ended the read-only transaction")
	case !errors.As(err, &me) || me.Number != errCantChangeTx:
		return nil // the connection is unusable; it is discarded and nothing more runs on it
	}
	if escaped == nil {
		var ro int64
		check := r.c.sessionCheckSQL
		if check == "" {
			check = sessionCheckSQL
		}
		switch err := r.sc.QueryRowContext(ctx, check).Scan(&ro); {
		case err != nil:
			escaped = fmt.Errorf("cannot verify that the session is still read-only: %w", err)
		case ro != 1:
			escaped = errors.New("the statement switched the session to read-write")
		}
	}
	_, _ = r.sc.ExecContext(ctx, "ROLLBACK")
	if escaped != nil {
		return driver.Wrap(driver.ErrReadOnly, Name, "query", fmt.Errorf("%w; the connection was discarded", escaped))
	}
	return nil
}

// sessionLexMode reads the lexMode of the connection's sessions from a
// pooled connection. Every session gets its sql_mode from the DSN (user
// statements never return their connection to the pool), so it is the mode
// a call's own connection will have.
func (c *conn) sessionLexMode(ctx context.Context) (lexMode, error) {
	var sqlMode string
	if err := c.db.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&sqlMode); err != nil {
		return lexMode{}, mapError(ctx, "browse", err)
	}
	return modeFromSQLMode(sqlMode), nil
}

// discardConn makes database/sql close the dedicated connection instead of
// returning it to the pool. go-sql-driver's ResetSession only checks that a
// connection is alive, so this is how a call's session state (USE, user
// variables, GET_LOCK locks, SET SESSION, an open transaction …) is
// guaranteed not to reach the next caller. Every user statement's connection
// is discarded; only the driver's own statements (catalog reads) pool.
func discardConn(sc *sql.Conn) {
	_ = sc.Raw(func(any) error { return sqldriver.ErrBadConn })
}

// killConn closes the network connection under sc while a result is still
// streaming, so closing the rows does not read (drain) the rest of the
// result. The server rolls back an open transaction when the client goes.
// database/sql then finds the connection invalid and drops it.
func killConn(sc *sql.Conn) {
	_ = sc.Raw(func(dc any) error {
		if c, ok := dc.(io.Closer); ok {
			_ = c.Close()
		}
		return nil
	})
}

// quoteIdent quotes a MySQL identifier with backticks.
func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
