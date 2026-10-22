package influxdb

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. Context errors keep their
// identity and carry no kind. A Flight (gRPC) error is classified by its
// status code, refined by the server's message, and the code's name is kept
// in Error.Code; the message is the server's, without the gRPC framing.
func mapError(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var de *driver.Error
	if errors.As(err, &de) {
		return err
	}
	var ctxErr error
	if ctx != nil {
		ctxErr = ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return driver.Wrap(nil, Name, op, err)
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		msg := st.Message()
		serr := statusError{msg: msg, err: err}
		switch st.Code() {
		case codes.Canceled, codes.DeadlineExceeded:
			if ctxErr != nil {
				return &driver.Error{Driver: Name, Op: op, Code: st.Code().String(), Err: errors.Join(serr, ctxErr)}
			}
		case codes.Unavailable:
			if ctxErr != nil {
				return &driver.Error{Driver: Name, Op: op, Code: st.Code().String(), Err: errors.Join(serr, ctxErr)}
			}
		}
		return &driver.Error{Kind: kindForStatus(st.Code(), msg), Driver: Name, Op: op, Code: st.Code().String(), Err: serr}
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) {
		if ctxErr != nil {
			return &driver.Error{Driver: Name, Op: op, Err: errors.Join(err, ctxErr)}
		}
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForStatus maps a gRPC status and the server's message to a driver
// error kind, or nil.
func kindForStatus(code codes.Code, msg string) error {
	lower := strings.ToLower(msg)
	switch code {
	case codes.InvalidArgument, codes.Internal, codes.Unknown, codes.FailedPrecondition, codes.Unimplemented:
		// The server's refusal of hidden DML ("Query DELETE … not
		// implemented yet") is its read-only layer speaking.
		if strings.Contains(lower, "not implemented") && containsDML(lower) {
			return driver.ErrReadOnly
		}
	}
	switch code {
	case codes.ResourceExhausted:
		// A reply over the client's receive cap; not a quota.
		if strings.Contains(lower, "larger than max") {
			return driver.ErrLimit
		}
	case codes.Unauthenticated:
		return driver.ErrAuth
	case codes.PermissionDenied:
		return driver.ErrPermission
	case codes.Unavailable:
		return driver.ErrConnection
	case codes.NotFound:
		return driver.ErrNotFound
	case codes.Unimplemented:
		return driver.ErrUnsupported
	case codes.AlreadyExists, codes.Aborted:
		return driver.ErrConflict
	case codes.InvalidArgument, codes.Internal, codes.Unknown, codes.FailedPrecondition:
		switch {
		case strings.Contains(lower, "dml not supported"), strings.Contains(lower, "ddl not supported"):
			return driver.ErrReadOnly
		case strings.Contains(lower, "not found"), strings.Contains(lower, "no field named"):
			return driver.ErrNotFound
		case strings.Contains(lower, "not implemented"), strings.Contains(lower, "not supported"):
			return driver.ErrUnsupported
		case code == codes.InvalidArgument:
			return driver.ErrSyntax
		}
	}
	return nil
}

// statusError shows the server's message but keeps the gRPC error
// reachable through Unwrap.
type statusError struct {
	msg string
	err error
}

func (s statusError) Error() string { return s.msg }
func (s statusError) Unwrap() error { return s.err }

// containsDML reports whether a lower-cased message names a write.
func containsDML(lower string) bool {
	for _, w := range []string{"insert", "update", "delete", "merge", "copy", "create", "drop", "alter", "truncate"} {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}
