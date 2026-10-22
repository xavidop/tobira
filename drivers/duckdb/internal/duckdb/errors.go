package duckdb

import (
	"context"
	"errors"
	"strconv"
	"strings"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. Context errors keep their
// identity (errors.Is still matches).
func mapError(op string, err error) error {
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
	msg := strings.ToLower(err.Error())
	var kind error
	code := ""
	var e *duckdb.Error
	if errors.As(err, &e) {
		code = strconv.Itoa(int(e.Type))
		switch e.Type {
		case duckdb.ErrorTypeCatalog:
			kind = driver.ErrNotFound
			if strings.Contains(msg, "already exists") {
				kind = driver.ErrConflict
			}
		case duckdb.ErrorTypeParser, duckdb.ErrorTypeSyntax:
			kind = driver.ErrSyntax
		case duckdb.ErrorTypeBinder:
			kind = driver.ErrSyntax
			if strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") {
				kind = driver.ErrNotFound
			}
		case duckdb.ErrorTypeConstraint:
			kind = driver.ErrConstraint
		case duckdb.ErrorTypePermission:
			kind = driver.ErrPermission
		case duckdb.ErrorTypeTransaction:
			kind = driver.ErrConflict
		case duckdb.ErrorTypeConnection, duckdb.ErrorTypeIO:
			kind = driver.ErrConnection
		case duckdb.ErrorTypeInterrupt:
			kind = driver.ErrConnection
		}
	}
	switch {
	case strings.Contains(msg, "read-only mode"), strings.Contains(msg, "read only mode"),
		strings.Contains(msg, "attached in read-only"), strings.Contains(msg, "cannot write to database"),
		strings.Contains(msg, "configuration has been locked"), strings.Contains(msg, "file system operations are disabled"):
		kind = driver.ErrReadOnly
	case strings.Contains(msg, "could not set lock on file"):
		kind = driver.ErrConflict
	case kind == nil && strings.Contains(msg, "does not exist"):
		kind = driver.ErrNotFound
	case kind == nil && (strings.Contains(msg, "syntax error") || strings.Contains(msg, "parser error")):
		kind = driver.ErrSyntax
	case kind == nil && strings.Contains(msg, "constraint"):
		kind = driver.ErrConstraint
	case kind == nil && strings.Contains(msg, "could not connect"):
		kind = driver.ErrConnection
	}
	return &driver.Error{Kind: kind, Driver: Name, Op: op, Code: code, Err: err}
}
