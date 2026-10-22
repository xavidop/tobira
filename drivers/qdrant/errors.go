package qdrant

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. A gRPC status is classified by
// its code, which is also stored in Error.Code ("NotFound"). When ctx is
// done, our own cancellation caused the failure: the error carries the ctx
// error (errors.Is works) and no kind. A *driver.Error passes through.
func mapError(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var de *driver.Error
	if errors.As(err, &de) {
		return err
	}
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(err, ctxErr) {
				return driver.Wrap(nil, Name, op, err)
			}
			return driver.Wrap(nil, Name, op, errors.Join(err, ctxErr))
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return driver.Wrap(nil, Name, op, err)
	}
	st, ok := status.FromError(err)
	if !ok {
		return driver.Wrap(nil, Name, op, err)
	}
	return &driver.Error{Kind: kindForStatus(st), Driver: Name, Op: op, Code: st.Code().String(), Err: errors.New(st.Message())}
}

// kindForStatus is kindForCode, except that a reply over the client's
// receive cap (ResourceExhausted, "received message larger than max") is a
// size limit. Other ResourceExhausted errors (quotas, rate limits) are not.
func kindForStatus(st *status.Status) error {
	if st.Code() == codes.ResourceExhausted && strings.Contains(st.Message(), "larger than max") {
		return driver.ErrLimit
	}
	return kindForCode(st.Code())
}

// kindForCode maps a gRPC status code to a driver error kind, or nil.
func kindForCode(c codes.Code) error {
	switch c {
	case codes.Unauthenticated:
		return driver.ErrAuth
	case codes.PermissionDenied:
		return driver.ErrPermission
	case codes.NotFound:
		return driver.ErrNotFound
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return driver.ErrSyntax
	case codes.AlreadyExists, codes.Aborted:
		return driver.ErrConflict
	case codes.Unavailable:
		return driver.ErrConnection
	case codes.Unimplemented:
		return driver.ErrUnsupported
	}
	return nil
}
