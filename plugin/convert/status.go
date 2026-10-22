package convert

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// kindCode maps a driver error kind to the gRPC code a failed RPC carries.
// The code is advisory: the host rebuilds the kind from ErrorInfo.kind,
// never from the code.
func kindCode(kind error) (codes.Code, bool) {
	switch kind {
	case driver.ErrNotFound, driver.ErrUnknownDriver:
		return codes.NotFound, true
	case driver.ErrAuth:
		return codes.Unauthenticated, true
	case driver.ErrPermission:
		return codes.PermissionDenied, true
	case driver.ErrReadOnly, driver.ErrConstraint:
		return codes.FailedPrecondition, true
	case driver.ErrUnsupported:
		return codes.Unimplemented, true
	case driver.ErrConnection:
		return codes.Unavailable, true
	case driver.ErrSyntax:
		return codes.InvalidArgument, true
	case driver.ErrConflict:
		return codes.Aborted, true
	case driver.ErrLimit:
		return codes.ResourceExhausted, true
	}
	return codes.Unknown, false
}

// codeOf picks the gRPC code for err: a context error first, then the
// driver kind, then this package's ErrLimit and ErrInvalid.
func codeOf(err error) codes.Code {
	switch {
	case errors.Is(err, context.Canceled):
		return codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return codes.DeadlineExceeded
	}
	if c, ok := kindCode(driver.KindOf(err)); ok {
		return c
	}
	switch {
	case errors.Is(err, ErrLimit):
		return codes.ResourceExhausted
	case errors.Is(err, ErrInvalid):
		return codes.InvalidArgument
	}
	return codes.Unknown
}

func redactWith(redact func(string) string, s string) string {
	if redact == nil {
		return s
	}
	return redact(s)
}

// ErrorInfoFromError describes err for the wire. A *driver.Error keeps its
// Driver, Op and Code, and its message is that of its Err; any other error
// is described by its whole text. The kind is driver.KindName(err), and
// "limit" for an unclassified error wrapping this package's ErrLimit.
// redact masks the message (nil leaves it as is).
func ErrorInfoFromError(err error, redact func(string) string) *pb.ErrorInfo {
	info := &pb.ErrorInfo{Kind: driver.KindName(err)}
	if info.Kind == "" && errors.Is(err, ErrLimit) {
		info.Kind = driver.KindName(driver.ErrLimit)
	}
	if de, ok := err.(*driver.Error); ok {
		info.Driver, info.Op, info.Code = de.Driver, de.Op, de.Code
		if de.Err != nil {
			info.Message = redactWith(redact, de.Err.Error())
		}
		return info
	}
	info.Message = redactWith(redact, err.Error())
	return info
}

// ToStatus turns an error returned by a driver (or by this package) into the
// gRPC status error an RPC returns: a code chosen from its kind and one
// ErrorInfo detail. redact masks every message before it leaves the plugin;
// nil leaves them as they are. A nil err is nil. A gRPC status that
// already carries an ErrorInfo (one StatusError or ToStatus built) is
// returned unchanged; any other status, such as one an upstream gRPC
// service returned to the driver, is re-wrapped like a plain error: its
// message redacted and its code chosen from driver.KindOf, never kept.
func ToStatus(err error, redact func(string) string) error {
	if err == nil {
		return nil
	}
	if s, ok := err.(interface{ GRPCStatus() *status.Status }); ok && ErrorInfoOf(s.GRPCStatus()) != nil {
		return err
	}
	st := status.New(codeOf(err), redactWith(redact, err.Error()))
	if withInfo, derr := st.WithDetails(ErrorInfoFromError(err, redact)); derr == nil {
		st = withInfo
	}
	return st.Err()
}

// StatusError builds a status error the plugin raises itself, such as an
// unknown handle (codes.NotFound, driver.ErrNotFound) or a capability the
// driver lacks (codes.Unimplemented, driver.ErrUnsupported). kind may be nil.
func StatusError(code codes.Code, kind error, msg string) error {
	st := status.New(code, msg)
	if withInfo, err := st.WithDetails(&pb.ErrorInfo{Kind: driver.KindName(kind), Message: msg}); err == nil {
		st = withInfo
	}
	return st.Err()
}

// ErrorInfoOf returns the ErrorInfo detail of st, or nil when it has none.
func ErrorInfoOf(st *status.Status) *pb.ErrorInfo {
	for _, d := range st.Details() {
		if info, ok := d.(*pb.ErrorInfo); ok {
			return info
		}
	}
	return nil
}

// capText truncates s to MaxNameBytes: a plugin's error text is shown, not
// trusted.
func capText(s string) string {
	if len(s) > MaxNameBytes {
		return s[:MaxNameBytes]
	}
	return s
}

// causeError is an error with a fixed text that still unwraps to the
// context error it stands for.
type causeError struct {
	msg   string
	cause error
}

func (e *causeError) Error() string { return e.msg }
func (e *causeError) Unwrap() error { return e.cause }

// FromErrorInfo rebuilds the *driver.Error an ErrorInfo describes. An
// unknown kind is left unclassified, and every text is capped at
// MaxNameBytes. cause, when non-nil, is a context error the result also
// unwraps to.
func FromErrorInfo(info *pb.ErrorInfo, cause error) error {
	msg := capText(info.GetMessage())
	var inner error
	switch {
	case cause != nil && msg == cause.Error():
		inner = cause
	case cause != nil:
		inner = &causeError{msg: msg, cause: cause}
	case msg != "":
		inner = errors.New(msg)
	}
	return &driver.Error{
		Kind:   driver.KindByName(info.GetKind()),
		Driver: capText(info.GetDriver()),
		Op:     capText(info.GetOp()),
		Code:   capText(info.GetCode()),
		Err:    inner,
	}
}

// FromStatus is the host side of ToStatus: it turns an RPC error back into a
// driver error. With an ErrorInfo detail it rebuilds that *driver.Error.
// Without one (a transport failure, a crashed plugin, a plugin in another
// language that sent a bare status) it classifies by code: Unavailable is
// driver.ErrConnection and Unimplemented driver.ErrUnsupported. Canceled
// and DeadlineExceeded also unwrap to context.Canceled and
// context.DeadlineExceeded. An error that is not a status is returned
// unchanged, and nil is nil. The host redacts the result again before
// showing it.
func FromStatus(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	var cause error
	switch st.Code() {
	case codes.Canceled:
		cause = context.Canceled
	case codes.DeadlineExceeded:
		cause = context.DeadlineExceeded
	}
	if info := ErrorInfoOf(st); info != nil {
		return FromErrorInfo(info, cause)
	}
	msg := capText(st.Message())
	switch {
	case cause != nil:
		return &driver.Error{Err: &causeError{msg: msg, cause: cause}}
	case st.Code() == codes.Unavailable:
		return driver.Wrap(driver.ErrConnection, "", "", errors.New(msg))
	case st.Code() == codes.Unimplemented:
		return driver.Wrap(driver.ErrUnsupported, "", "", errors.New(msg))
	}
	return &driver.Error{Err: errors.New(msg)}
}
