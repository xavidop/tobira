package neo4j

import (
	"context"
	"errors"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/db"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. A *driver.Error passes
// through. Context errors keep their identity (errors.Is still matches)
// and carry no kind; so does any error while ctx is done, since our own
// cancellation caused it. A server error is classified by its status code,
// which is kept in Error.Code; the Go driver's connectivity errors by
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
	var ne *db.Neo4jError
	if errors.As(err, &ne) {
		return &driver.Error{Kind: kindForCode(ne.Code, ne.Msg), Driver: Name, Op: op, Code: ne.Code, Err: err}
	}
	var ue *neo4j.UsageError
	switch {
	case errors.As(err, &ue):
		return driver.Wrap(driver.ErrSyntax, Name, op, err)
	case neo4j.IsConnectivityError(err):
		if strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
			return driver.Wrap(driver.ErrAuth, Name, op, err)
		}
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForCode maps a Neo4j status code (Neo.ClientError.Statement.SyntaxError
// and so on) to a driver error kind, or nil.
func kindForCode(code, msg string) error {
	switch {
	case strings.HasPrefix(code, "Neo.ClientError.Security.Unauthorized"),
		strings.HasPrefix(code, "Neo.ClientError.Security.AuthenticationRateLimit"),
		strings.HasPrefix(code, "Neo.ClientError.Security.CredentialsExpired"):
		return driver.ErrAuth
	case strings.HasPrefix(code, "Neo.ClientError.Security."):
		return driver.ErrPermission
	case code == "Neo.ClientError.Statement.AccessMode",
		code == "Neo.ClientError.Transaction.ForbiddenDueToTransactionType",
		code == "Neo.ClientError.General.ForbiddenReadOnlyDatabase",
		code == "Neo.ClientError.General.ReadOnly",
		code == "Neo.ClientError.General.ForbiddenOnReadOnlyDatabase",
		strings.Contains(msg, "read access mode"),
		strings.Contains(msg, "read transaction"):
		return driver.ErrReadOnly
	case code == "Neo.ClientError.Database.DatabaseNotFound",
		code == "Neo.ClientError.Statement.EntityNotFound",
		code == "Neo.ClientError.Procedure.ProcedureNotFound",
		code == "Neo.ClientError.Schema.IndexNotFound",
		code == "Neo.ClientError.Schema.ConstraintNotFound":
		return driver.ErrNotFound
	case strings.HasPrefix(code, "Neo.ClientError.Schema.Constraint"):
		return driver.ErrConstraint
	case strings.HasPrefix(code, "Neo.ClientError.Statement."),
		strings.HasPrefix(code, "Neo.ClientError.Procedure."):
		return driver.ErrSyntax
	case code == "Neo.TransientError.Transaction.DeadlockDetected",
		code == "Neo.TransientError.Transaction.LockClientStopped",
		code == "Neo.ClientError.Transaction.Terminated":
		return driver.ErrConflict
	case strings.HasPrefix(code, "Neo.TransientError.General."),
		strings.HasPrefix(code, "Neo.TransientError.Cluster."),
		strings.HasPrefix(code, "Neo.ClientError.Cluster."):
		return driver.ErrConnection
	}
	return nil
}
