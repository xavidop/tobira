package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. Context errors keep their
// identity (errors.Is still matches) and carry no kind. A PostgreSQL error is
// classified by SQLSTATE, which is also stored in Error.Code. A connection
// failure that wraps a server error (for example 28P01 during startup) takes
// the server error's classification.
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
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		if pe.Code == "57014" && ctxErr != nil { // query_canceled caused by our own ctx
			return &driver.Error{Driver: Name, Op: op, Code: pe.Code, Err: errors.Join(err, ctxErr)}
		}
		return &driver.Error{Kind: kindForSQLState(pe.Code), Driver: Name, Op: op, Code: pe.Code, Err: err}
	}
	var ce *pgconn.ConnectError
	var ne net.Error
	if errors.As(err, &ce) || errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		if ctxErr != nil {
			return &driver.Error{Driver: Name, Op: op, Err: errors.Join(err, ctxErr)}
		}
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	var pce *pgconn.ParseConfigError
	if errors.As(err, &pce) {
		return driver.Wrap(driver.ErrSyntax, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForSQLState maps a SQLSTATE to a driver error kind, or nil.
func kindForSQLState(code string) error {
	switch code {
	case "25006": // read_only_sql_transaction
		return driver.ErrReadOnly
	case "42501": // insufficient_privilege
		return driver.ErrPermission
	case "42P01", "42703", "42704", "3F000", "3D000": // undefined table/column/object, invalid schema/catalog
		return driver.ErrNotFound
	case "42601", "42000": // syntax_error, syntax_error_or_access_rule_violation
		return driver.ErrSyntax
	case "22P02", "22007", "22008", "22P05", "22023", "22004":
		// invalid_text_representation, invalid_datetime_format,
		// datetime_field_overflow, untranslatable_character,
		// invalid_parameter_value, null_value_not_allowed: the input is
		// malformed. Other class 22 errors (22012 division by zero, 22003
		// out of range) are runtime data errors and get no kind.
		return driver.ErrSyntax
	case "40001", "40P01": // serialization_failure, deadlock_detected
		return driver.ErrConflict
	case "53300": // too_many_connections
		return driver.ErrConnection
	}
	switch {
	case strings.HasPrefix(code, "28"): // invalid_authorization_specification, invalid_password
		return driver.ErrAuth
	case strings.HasPrefix(code, "23"): // integrity_constraint_violation
		return driver.ErrConstraint
	case strings.HasPrefix(code, "08"): // connection_exception
		return driver.ErrConnection
	}
	return nil
}

// named returns err with the Driver of its *driver.Error set to the name of
// the driver that opened c, so a flavored driver's errors carry its own name
// (redshift, cockroachdb, greenplum) rather than "postgres". Errors are built
// per call, so changing the one in the chain is safe.
func (c *conn) named(err error) error {
	var de *driver.Error
	if c == nil || c.name == "" || c.name == Name || !errors.As(err, &de) || de.Driver != Name {
		return err
	}
	de.Driver = c.name
	return err
}
