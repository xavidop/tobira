package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/xavidop/tobira/driver"
)

// flavor is the server family behind the MySQL protocol. Open detects it, a
// mariadb:// or singlestore:// DSN or the flavor= DSN parameter forces it,
// and it selects the dialect differences: catalog SQL, EXPLAIN syntax,
// transaction statements and which read-only layers exist.
type flavor uint8

const (
	flavorMySQL       flavor = iota // MySQL 8 (Percona Server included)
	flavorMariaDB                   // VERSION() contains "MariaDB"
	flavorSingleStore               // @@memsql_version exists
)

// flavorParam is the DSN parameter that forces a flavor; it is removed
// before connecting, so it never becomes a session variable.
const flavorParam = "flavor"

func (f flavor) String() string {
	switch f {
	case flavorMariaDB:
		return "mariadb"
	case flavorSingleStore:
		return "singlestore"
	}
	return "mysql"
}

// parseFlavor reads a flavor= value, case-insensitively.
func parseFlavor(s string) (flavor, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "mysql":
		return flavorMySQL, nil
	case "mariadb":
		return flavorMariaDB, nil
	case "singlestore":
		return flavorSingleStore, nil
	}
	return flavorMySQL, fmt.Errorf("unknown flavor %q: use mysql, mariadb or singlestore", s)
}

// The variables Open probes. @@in_transaction is MariaDB's (MySQL and
// SingleStore answer 1193, unknown variable); @@memsql_version is
// SingleStore's.
const (
	mariaDBProbeVar = "in_transaction"
	memsqlProbeVar  = "memsql_version"
)

// flavorOf classifies a server from SELECT VERSION(), @@version_comment and
// whether the MariaDB-only and SingleStore-only variables exist. MariaDB
// usually says so in its version, but an administrator can override the
// version (to look like MySQL 8.0, say), so its comment and its own variable
// count too. SingleStore reports a MySQL version (5.7.32 or 8.0.x) but has
// @@memsql_version.
func flavorOf(version, comment string, mariaVar, memsql bool) flavor {
	switch {
	case strings.Contains(strings.ToLower(version), "mariadb"),
		strings.Contains(strings.ToLower(comment), "mariadb"),
		mariaVar:
		return flavorMariaDB
	case memsql:
		return flavorSingleStore
	}
	return flavorMySQL
}

// errUnknownSysVar is ER_UNKNOWN_SYSTEM_VARIABLE, the answer to SELECT @@x
// for a variable the server does not have.
const errUnknownSysVar = 1193

// detectFlavor reads the server version and flavor over db. A probe that
// fails other than with "unknown variable" fails a read-only Open, and makes
// a writable one fall back to MySQL with a debug log (see classifyFlavor).
func detectFlavor(ctx context.Context, db *sql.DB, readOnly bool, log *slog.Logger) (string, flavor, error) {
	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return "", flavorMySQL, err
	}
	var comment sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT @@version_comment").Scan(&comment); err != nil {
		comment.String = "" // the variable probes decide
	}
	f, err := classifyFlavor(ctx, version, comment.String, serverProbe(ctx, db), readOnly, log)
	if err != nil {
		return "", flavorMySQL, err
	}
	return version, f, nil
}

// serverProbe reports whether the server behind db has a system variable:
// SELECT @@variable succeeds, or fails with 1193 (unknown variable). variable
// is one of the driver's constants, never caller text.
func serverProbe(ctx context.Context, db *sql.DB) func(variable string) (bool, error) {
	return func(variable string) (bool, error) {
		var v sql.NullString
		err := db.QueryRowContext(ctx, "SELECT @@"+variable).Scan(&v)
		var me *gomysql.MySQLError
		switch {
		case err == nil:
			return true, nil
		case errors.As(err, &me) && me.Number == errUnknownSysVar:
			return false, nil
		}
		return false, err
	}
}

// classifyFlavor decides the flavor from the version, the comment and the
// variable probes (probe reports whether the server has a variable). When a
// probe fails other than with "unknown variable", for example behind a
// proxy, a read-only connection fails, since read-only must know the server
// family, and a writable one is treated as MySQL with a debug log.
func classifyFlavor(ctx context.Context, version, comment string, probe func(string) (bool, error), readOnly bool, log *slog.Logger) (flavor, error) {
	if f := flavorOf(version, comment, false, false); f == flavorMariaDB {
		return f, nil
	}
	// @@memsql_version first: it is the decisive signal, so a SingleStore is
	// never taken for MariaDB, whatever it answers for @@in_transaction.
	var has [2]bool // memsql, mariadb
	for i, variable := range []string{memsqlProbeVar, mariaDBProbeVar} {
		ok, err := probe(variable)
		if err != nil {
			if readOnly {
				return flavorMySQL, fmt.Errorf("cannot tell the server's flavor (SELECT @@%s failed), which read-only access needs: %w", variable, err)
			}
			log.DebugContext(ctx, "mysql: flavor probe failed; treating the server as MySQL", "variable", variable, "error", err.Error())
			return flavorMySQL, nil
		}
		if has[i] = ok; ok {
			break
		}
	}
	if has[0] {
		return flavorSingleStore, nil
	}
	return flavorOf(version, comment, has[1], false), nil
}

// readOnlyVarRefusal turns the server's refusal of the read-only session
// variable (MariaDB before 11.1 has no transaction_read_only) into a clear
// ErrUnsupported; any other error gives nil.
func readOnlyVarRefusal(err error) error {
	var me *gomysql.MySQLError
	if !errors.As(err, &me) || me.Number != errUnknownSysVar || !strings.Contains(strings.ToLower(me.Message), readOnlyVar) {
		return nil
	}
	return driver.Wrap(driver.ErrUnsupported, Name, "open",
		fmt.Errorf("read-only access needs a server with the transaction_read_only variable (MySQL 8.0 to 8.4, MariaDB 11.1 to 11.x): %w", err))
}

// mariaDBVerified reports whether the read-only guard was verified against
// this MariaDB version: 11.1 (the first with transaction_read_only) to 11.x.
// A "5.5.5-" replication prefix is ignored.
func mariaDBVerified(version string) bool {
	if !strings.Contains(strings.ToLower(version), "mariadb") {
		return false
	}
	v := strings.TrimPrefix(version, "5.5.5-")
	major, rest, ok := strings.Cut(v, ".")
	if !ok || major != "11" {
		return false
	}
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	minor, err := strconv.Atoi(rest[:end])
	return err == nil && minor >= 1
}

// readOnlyRefusal says why read-only access is refused on this server, or
// returns nil when the read-only guard was verified against it. detected is
// what Open found; f is the flavor in use (forced or detected). Forcing a
// flavor onto a server of another one turns read-only off: the guard is
// only trusted on the server family it was verified against.
func readOnlyRefusal(version string, detected, f flavor) error {
	switch {
	case detected != f:
		return fmt.Errorf("read-only access is off: flavor=%s was forced on a %s server (version %q); remove flavor= to use read-only", f, detected, version)
	case f == flavorSingleStore:
		return errors.New("read-only access is not supported on SingleStore: it has no read-only transactions, so the driver cannot hold a statement in; connect as a user with only SELECT grants instead")
	case f == flavorMariaDB:
		if !mariaDBVerified(version) {
			return fmt.Errorf("read-only access needs MariaDB 11.1 to 11.x: the read-only guard is not verified on server version %q", version)
		}
		return nil
	case !guardVerified(version):
		return unverifiedServer(version)
	}
	return nil
}

// readOnlyRefusal is readOnlyRefusal for this connection.
func (c *conn) readOnlyRefusal() error { return readOnlyRefusal(c.version, c.detected, c.flavor) }

// syntaxFlavor is the flavor whose syntax the server must accept: catalog
// SQL, EXPLAIN, transaction statements and keyset clauses all use it, so a
// forced flavor never sends SQL the server cannot parse. It is SingleStore when the server
// is, or is treated as, SingleStore (it has no READ WRITE transactions and no
// row-constructor comparison), else MariaDB likewise, else MySQL.
func (c *conn) syntaxFlavor() flavor {
	switch {
	case c.flavor == flavorSingleStore || c.detected == flavorSingleStore:
		return flavorSingleStore
	case c.flavor == flavorMariaDB || c.detected == flavorMariaDB:
		return flavorMariaDB
	}
	return flavorMySQL
}

// onMariaDB reports whether the server is, or is treated as, MariaDB.
func (c *conn) onMariaDB() bool { return c.flavor == flavorMariaDB || c.detected == flavorMariaDB }

// mariaDBSafeModes are the MariaDB sql_mode flags that leave the guard's
// lexer correct: none changes where a string, identifier or comment ends,
// except ANSI_QUOTES and NO_BACKSLASH_ESCAPES, which the lexer reads
// (modeFromSQLMode). ANSI is a combination of flags listed here.
var mariaDBSafeModes = map[string]bool{
	"ALLOW_INVALID_DATES": true, "ANSI": true, "ANSI_QUOTES": true, "EMPTY_STRING_IS_NULL": true,
	"ERROR_FOR_DIVISION_BY_ZERO": true, "HIGH_NOT_PRECEDENCE": true, "IGNORE_BAD_TABLE_OPTIONS": true,
	"IGNORE_SPACE": true, "NO_AUTO_CREATE_USER": true, "NO_AUTO_VALUE_ON_ZERO": true,
	"NO_BACKSLASH_ESCAPES": true, "NO_DIR_IN_CREATE": true, "NO_ENGINE_SUBSTITUTION": true,
	"NO_FIELD_OPTIONS": true, "NO_KEY_OPTIONS": true, "NO_TABLE_OPTIONS": true,
	"NO_UNSIGNED_SUBTRACTION": true, "NO_ZERO_DATE": true, "NO_ZERO_IN_DATE": true,
	"ONLY_FULL_GROUP_BY": true, "PAD_CHAR_TO_FULL_LENGTH": true, "PIPES_AS_CONCAT": true,
	"REAL_AS_FLOAT": true, "SIMULTANEOUS_ASSIGNMENT": true, "STRICT_ALL_TABLES": true,
	"STRICT_TRANS_TABLES": true, "TIME_ROUND_FRACTIONAL": true, "TRADITIONAL": true,
}

// mariaDBModeRefusal checks a MariaDB @@sql_mode against mariaDBSafeModes.
// It is an allow-list: MSSQL mode quotes identifiers with [ ], ORACLE mode
// changes the grammar, and an unknown flag may change the lexer too, so any
// flag outside the list is refused.
func mariaDBModeRefusal(sqlMode string) error {
	for f := range strings.SplitSeq(sqlMode, ",") {
		f = strings.ToUpper(strings.TrimSpace(f))
		if f != "" && !mariaDBSafeModes[f] {
			return fmt.Errorf("MariaDB sql_mode %s is not supported on a checked path: it changes how the server reads quoted text, which the statement check relies on", f)
		}
	}
	return nil
}
