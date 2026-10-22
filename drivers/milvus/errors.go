package milvus

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

// Milvus error codes (its merr package) the driver classifies.
const (
	codeCollectionNotFound    = 100
	codeCollectionNotLoaded   = 101
	codeCollectionPartLoaded  = 103
	codePartitionNotFound     = 200
	codePartitionNotLoaded    = 201
	codePartitionPartLoaded   = 202
	codeIndexNotFound         = 700
	codeDatabaseNotFound      = 800
	codeParameterInvalid      = 1100
	codeParameterMissing      = 1101
	codeParameterTooLarge     = 1102
	codeNotAuthenticated      = 1400
	codeNotPermitted          = 1401
	codeAliasNotFound         = 1600
	codeFieldNotFound         = 1700
	codeFieldInvalidName      = 1701
	codeNeedAuthenticate      = 1800
	codeCheckPrimaryKey       = 1806
	codeServiceNotReady       = 1
	codeServiceUnavailable    = 2
	codeOperationNotSupported = 3000
	codeServiceUnimplemented  = 10
)

// mapError classifies err as a *driver.Error. A gRPC status is classified
// by its code; a Milvus status by its numeric code, which is also
// kept in Error.Code. When ctx is done, our own cancellation caused the
// failure: the error carries the ctx error (errors.Is works) and no kind.
// A *driver.Error passes through.
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
	if st, ok := status.FromError(err); ok {
		kind := kindForGRPC(st.Code())
		msg := st.Message()
		if kind == driver.ErrLimit {
			msg += limitHint
		}
		return &driver.Error{Kind: kind, Driver: Name, Op: op, Code: st.Code().String(), Err: errors.New(msg)}
	}
	code := codeOf(err)
	kind := kindForCode(code)
	if kind == driver.ErrLimit {
		err = fmt.Errorf("%w%s", err, limitHint)
	}
	return &driver.Error{Kind: kind, Driver: Name, Op: op, Code: strconv.Itoa(int(code)), Err: err}
}

// limitHint tells how to get past a size limit.
const limitHint = ": narrow outputFields (vectors are large) or lower the limit"

// kindForGRPC maps a gRPC status code to a driver error kind, or nil.
func kindForGRPC(c codes.Code) error {
	switch c {
	case codes.Unauthenticated:
		return driver.ErrAuth
	case codes.PermissionDenied:
		return driver.ErrPermission
	case codes.NotFound:
		return driver.ErrNotFound
	case codes.InvalidArgument, codes.OutOfRange:
		return driver.ErrSyntax
	case codes.AlreadyExists, codes.Aborted:
		return driver.ErrConflict
	case codes.Unavailable:
		return driver.ErrConnection
	case codes.Unimplemented:
		return driver.ErrUnsupported
	case codes.ResourceExhausted:
		// A reply over the driver's 64 MiB cap, or a request over the
		// server's message limit.
		return driver.ErrLimit
	}
	return nil
}

// kindForCode maps a Milvus error code to a driver error kind, or nil.
func kindForCode(code int32) error {
	switch code {
	case codeCollectionNotFound, codePartitionNotFound, codeDatabaseNotFound, codeAliasNotFound, codeIndexNotFound:
		return driver.ErrNotFound
	case codeParameterTooLarge:
		return driver.ErrLimit
	case codeParameterInvalid, codeParameterMissing, codeFieldNotFound, codeFieldInvalidName, codeCheckPrimaryKey:
		return driver.ErrSyntax
	case codeNotAuthenticated, codeNeedAuthenticate:
		return driver.ErrAuth
	case codeNotPermitted:
		return driver.ErrPermission
	case codeServiceNotReady, codeServiceUnavailable:
		return driver.ErrConnection
	case codeOperationNotSupported, codeServiceUnimplemented:
		return driver.ErrUnsupported
	}
	return nil
}

// notLoaded reports whether err is Milvus saying the collection (or a
// partition) is not, or not fully, loaded.
func notLoaded(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := status.FromError(err); ok {
		return false
	}
	switch codeOf(err) {
	case codeCollectionNotLoaded, codeCollectionPartLoaded, codePartitionNotLoaded, codePartitionPartLoaded:
		return true
	}
	return false
}

// statusError is a failed Milvus status: its numeric code and reason.
type statusError struct {
	code   int32
	reason string
}

func (e *statusError) Error() string { return fmt.Sprintf("%s (%d)", e.reason, e.code) }

// legacyCodes maps the deprecated ErrorCode of a status without a numeric
// code (servers before 2.3) to the numeric code.
var legacyCodes = map[commonpb.ErrorCode]int32{ //nolint:staticcheck // servers before 2.3 send only the legacy code
	commonpb.ErrorCode_CollectionNotExists: codeCollectionNotFound,
	commonpb.ErrorCode_IllegalArgument:     codeParameterInvalid,
	commonpb.ErrorCode_PermissionDenied:    codeNotPermitted,
	commonpb.ErrorCode_NotReadyServe:       codeServiceNotReady,
	commonpb.ErrorCode_IndexNotExist:       codeIndexNotFound,
}

// checkRPC turns a reply's failed status into a *statusError; a transport
// error passes through. The driver checks statuses itself rather than
// through the SDK's merr package, which links Milvus's server packages
// (an embedded etcd server among them) into the binary.
func checkRPC(resp any, err error) error {
	if err != nil {
		return err
	}
	var st *commonpb.Status
	switch r := resp.(type) {
	case *commonpb.Status:
		st = r
	case interface{ GetStatus() *commonpb.Status }:
		st = r.GetStatus()
	default:
		return errors.New("milvus returned no reply")
	}
	if st.GetCode() == 0 && st.GetErrorCode() == commonpb.ErrorCode_Success { //nolint:staticcheck // servers before 2.3 send only the legacy code
		return nil
	}
	code := st.GetCode()
	if code == 0 {
		code = legacyCodes[st.GetErrorCode()] //nolint:staticcheck // servers before 2.3 send only the legacy code
	}
	return &statusError{code: code, reason: st.GetReason()}
}

// codeOf is the Milvus code of err, or 0.
func codeOf(err error) int32 {
	var se *statusError
	if errors.As(err, &se) {
		return se.code
	}
	return 0
}
