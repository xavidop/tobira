package cassandra

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. A *driver.Error passes
// through. Context errors keep their identity (errors.Is still matches)
// and carry no kind; so does any error while ctx is done, since our own
// cancellation caused it. A server error is classified by its code, which
// is stored in Error.Code as 0x2200 and so on; gocql's client errors by
// identity.
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
	if ctx != nil && ctx.Err() != nil {
		return &driver.Error{Driver: Name, Op: op, Err: errors.Join(err, ctx.Err())}
	}
	var re gocql.RequestError
	if errors.As(err, &re) {
		return &driver.Error{Kind: kindForCode(re.Code(), re.Message()), Driver: Name, Op: op, Code: fmt.Sprintf("0x%04X", re.Code()), Err: err}
	}
	var ne net.Error
	switch {
	case errors.Is(err, gocql.ErrNoConnections), errors.Is(err, gocql.ErrSessionClosed),
		errors.Is(err, gocql.ErrConnectionClosed), errors.Is(err, gocql.ErrTimeoutNoResponse),
		errors.Is(err, gocql.ErrNoStreams),
		errors.Is(err, gocql.ErrUnavailable), errors.Is(err, gocql.ErrNoHosts),
		errors.Is(err, gocql.ErrNoConnectionsStarted), errors.As(err, &ne):
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	case errors.Is(err, gocql.ErrUseStmt):
		return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("USE is not supported: qualify table names or pick the namespace"))
	case errors.Is(err, gocql.ErrKeyspaceDoesNotExist), errors.Is(err, gocql.ErrNotFound):
		return driver.Wrap(driver.ErrNotFound, Name, op, err)
	case strings.HasPrefix(err.Error(), "gocql: expected ") && strings.Contains(err.Error(), " values send got "):
		// The parameters do not match the placeholders (a top-level tuple
		// placeholder counts once per element).
		return driver.Wrap(driver.ErrSyntax, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForCode maps a native protocol error code to a driver error kind,
// or nil. An invalid query (0x2200) naming a missing keyspace or table is
// ErrNotFound; any other is ErrSyntax.
func kindForCode(code int, msg string) error {
	switch code {
	case gocql.ErrCodeCredentials:
		return driver.ErrAuth
	case gocql.ErrCodeUnauthorized:
		return driver.ErrPermission
	case gocql.ErrCodeSyntax:
		return driver.ErrSyntax
	case gocql.ErrCodeInvalid:
		m := strings.ToLower(msg)
		if strings.Contains(m, "does not exist") || strings.Contains(m, "unconfigured table") ||
			strings.HasPrefix(m, "keyspace") && strings.Contains(m, "doesn't exist") {
			return driver.ErrNotFound
		}
		return driver.ErrSyntax
	case gocql.ErrCodeAlreadyExists, gocql.ErrCodeCASWriteUnknown:
		return driver.ErrConflict
	case gocql.ErrCodeUnavailable, gocql.ErrCodeOverloaded, gocql.ErrCodeBootstrapping,
		gocql.ErrCodeWriteTimeout, gocql.ErrCodeReadTimeout, gocql.ErrCodeReadFailure, gocql.ErrCodeWriteFailure:
		return driver.ErrConnection
	}
	return nil
}
