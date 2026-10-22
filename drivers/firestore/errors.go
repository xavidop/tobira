package firestore

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"cloud.google.com/go/auth"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

// writeOps are the operations whose FailedPrecondition is a failed write
// precondition (the document changed since it was read), not a missing
// index.
var writeOps = map[string]bool{"insert": true, "update": true, "delete": true}

// indexHint is added to an ErrUnsupported for a query that needs an index.
const indexHint = "the query needs a composite index: the message names the link that creates it"

// mapError classifies err as a *driver.Error. Context errors keep their
// identity and carry no kind; so does any failure while ctx is done, which
// our own cancellation caused. gRPC errors are classified by status code,
// which is kept in Error.Code.
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
		return driver.Wrap(nil, Name, op, errors.Join(err, ctx.Err()))
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		kind := kindForCode(op, st.Code())
		if kind == driver.ErrUnsupported && st.Code() == codes.FailedPrecondition && strings.Contains(strings.ToLower(st.Message()), "index") {
			err = errors.Join(errors.New(indexHint), err)
		}
		return &driver.Error{Kind: kind, Driver: Name, Op: op, Code: st.Code().String(), Err: err}
	}
	var re *oauth2.RetrieveError
	var ae *auth.Error
	if errors.As(err, &re) || errors.As(err, &ae) || strings.Contains(err.Error(), "could not find default credentials") {
		return driver.Wrap(driver.ErrAuth, Name, op, err)
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForCode classifies a gRPC status code
// (https://cloud.google.com/firestore/docs/understand-error-codes).
func kindForCode(op string, code codes.Code) error {
	switch code {
	case codes.NotFound:
		return driver.ErrNotFound
	case codes.AlreadyExists, codes.Aborted:
		return driver.ErrConflict
	case codes.PermissionDenied:
		return driver.ErrPermission
	case codes.Unauthenticated:
		return driver.ErrAuth
	case codes.InvalidArgument, codes.OutOfRange:
		return driver.ErrSyntax
	case codes.FailedPrecondition:
		if writeOps[op] {
			return driver.ErrConflict
		}
		return driver.ErrUnsupported
	case codes.Unavailable:
		return driver.ErrConnection
	case codes.Unimplemented:
		return driver.ErrUnsupported
	}
	return nil
}
