package mongodb

import (
	"context"
	"errors"
	"strconv"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/topology"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. A *driver.Error passes through.
// Context errors keep their identity (errors.Is still matches) and carry no
// kind; so does any error while ctx is done, since our own cancellation
// caused it. A server error is classified by its code, which is also stored
// in Error.Code; an unknown code keeps its message and gets no kind. Network
// and server-selection failures are ErrConnection.
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
	if code, ok := serverCode(err); ok {
		return &driver.Error{Kind: kindForCode(code), Driver: Name, Op: op, Code: strconv.Itoa(code), Err: err}
	}
	var sse topology.ServerSelectionError
	if errors.Is(err, mongo.ErrClientDisconnected) || mongo.IsNetworkError(err) || errors.As(err, &sse) ||
		errors.Is(err, topology.ErrTopologyClosed) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	var ce topology.ConnectionError
	if errors.As(err, &ce) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// serverCode returns the first error code a server error carries: the
// command's code, or that of its first write error.
func serverCode(err error) (int, bool) {
	var we mongo.WriteException
	if errors.As(err, &we) {
		if len(we.WriteErrors) > 0 {
			return we.WriteErrors[0].Code, true
		}
		if we.WriteConcernError != nil {
			return we.WriteConcernError.Code, true
		}
	}
	var se mongo.ServerError
	if errors.As(err, &se) {
		if codes := se.ErrorCodes(); len(codes) > 0 && codes[0] != 0 {
			return codes[0], true
		}
	}
	return 0, false
}

// kindForCode maps a MongoDB error code to a driver error kind, or nil.
func kindForCode(code int) error {
	switch code {
	case 18: // AuthenticationFailed
		return driver.ErrAuth
	case 13, 8000: // Unauthorized, AtlasError (unauthorized on Atlas)
		return driver.ErrPermission
	case 26: // NamespaceNotFound
		return driver.ErrNotFound
	case 2, 9, 14, 15, 17, 59, 168, 15952, 15983, 16410, 17276, 28667, 31249, 40323, 40324, 40352, 40414, 40415, 51091:
		// BadValue, FailedToParse, TypeMismatch, Overflow, InvalidLength,
		// CommandNotFound, InvalidPipelineOperator, unknown group operator,
		// unknown expression, field path with $, $expr arity, unknown
		// accumulator, path collision, unrecognized stage, stage spec,
		// empty field path, missing required field, unknown field, regex.
		return driver.ErrSyntax
	case 11000, 11001, 121, 66: // DuplicateKey, DocumentValidationFailure, ImmutableField
		return driver.ErrConstraint
	case 112, 251, 24: // WriteConflict, NoSuchTransaction, LockTimeout
		return driver.ErrConflict
	case 115, 166, 40573, 20: // CommandNotSupported, CommandNotSupportedOnView, change streams need a replica set, IllegalOperation
		return driver.ErrUnsupported
	case 6, 7, 89, 91, 189, 10107, 11600, 11602, 13435, 13436: // host unreachable/not found, timeouts, shutdown, not primary
		return driver.ErrConnection
	}
	return nil
}
