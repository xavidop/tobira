package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	mssqldb "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

const (
	urlScheme   = "sqlserver"
	aliasScheme = "mssql"
	odbcScheme  = "odbc"
	// sessionSQL reads the database and schema of a fresh session: the
	// login database is the one whose schemas are listed by bare name.
	sessionSQL = "SELECT DB_NAME(), SCHEMA_NAME()"
)

// Open builds a connector from cfg and pings once, so a bad DSN or bad
// credentials fail here rather than on the first query.
func (mssqlDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	p, err := buildConfig(cfg)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	db := sql.OpenDB(mssqldb.NewConnectorConfig(p))
	c := &conn{db: db, readOnly: cfg.ReadOnly}
	err = db.PingContext(ctx)
	if err == nil {
		var dbName, schema sql.NullString
		err = db.QueryRowContext(ctx, sessionSQL).Scan(&dbName, &schema)
		c.loginDB, c.defaultSchema = dbName.String, schema.String
	}
	if err != nil {
		_ = db.Close()
		return nil, scrubError(mapError(ctx, "open", err), cfg)
	}
	if c.defaultSchema == "" {
		c.defaultSchema = "dbo"
	}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	return c, nil
}

// buildConfig turns cfg into a go-mssqldb config without connecting. It
// accepts go-mssqldb's own forms:
//
//   - sqlserver://user:password@host[:port][/instance]?database=db&…
//     (mssql:// is the same URL under tobira's alias scheme);
//   - ADO: server=host;user id=user;password=…;database=db;…
//   - ODBC: odbc:server=host;uid=user;pwd=…;database=db;…
//
// and then enforces what tobira relies on:
//
//   - go-mssqldb never logs (it would write to the terminal);
//   - date and time values decode in UTC, and GUIDs keep SQL Server's
//     byte order, which the driver formats itself;
//   - a read-only connection asks for ApplicationIntent=ReadOnly when the
//     DSN names a database (go-mssqldb requires one for it). It only
//     routes the connection to a readable secondary of an availability
//     group; it does not make a primary read-only, which is why the
//     read-only guard does not rely on it;
//   - a non-empty ConnConfig.Password replaces the DSN's password.
func buildConfig(cfg driver.ConnConfig) (msdsn.Config, error) {
	fail := func(kind error, err error) (msdsn.Config, error) {
		return msdsn.Config{}, driver.Wrap(kind, Name, "open", err)
	}
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return fail(driver.ErrUnsupported, fmt.Errorf("unsupported option %q: the mssql driver takes its settings from the DSN", k))
	}
	dsn := strings.TrimSpace(cfg.DSN)
	switch scheme := driver.Scheme(dsn); {
	case scheme == urlScheme || scheme == aliasScheme:
		if !strings.HasPrefix(dsn[len(scheme):], "://") {
			return fail(driver.ErrSyntax, fmt.Errorf("invalid DSN: want %s://host", scheme))
		}
		dsn = urlScheme + dsn[len(scheme):]
	case scheme == odbcScheme:
		dsn = odbcScheme + dsn[len(scheme):]
	case scheme != "":
		return fail(driver.ErrSyntax, fmt.Errorf("unsupported DSN scheme %q: use sqlserver://, mssql://, odbc: or an ADO connection string", scheme))
	case !strings.Contains(dsn, "="):
		return fail(driver.ErrSyntax, errors.New("invalid DSN: use sqlserver://user@host?database=db or an ADO connection string (server=…;database=…)"))
	}
	p, err := msdsn.Parse(dsn)
	if err != nil {
		return fail(driver.ErrSyntax, fmt.Errorf("invalid DSN: %w", err))
	}
	if p.Host == "" {
		return fail(driver.ErrSyntax, errors.New("invalid DSN: no server host"))
	}
	if !cfg.Password.IsZero() {
		p.Password = cfg.Password.Reveal()
	}
	p.LogFlags = 0
	p.Encoding.Timezone = time.UTC
	p.Encoding.GuidConversion = false
	p.ReadOnlyIntent = p.ReadOnlyIntent || cfg.ReadOnly && p.Database != ""
	return p, nil
}

// scrubError masks every password of cfg in the message of a *driver.Error,
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
