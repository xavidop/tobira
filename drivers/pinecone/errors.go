package pinecone

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. A gRPC status (the data
// plane) is classified by its code and a *pinecone.PineconeError (the
// control plane) by its HTTP status; either code is kept in Error.Code.
// When ctx is done, our own cancellation caused the failure: the error
// carries the ctx error (errors.Is works) and no kind. A *driver.Error
// passes through.
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
	var pe *pinecone.PineconeError
	if errors.As(err, &pe) {
		return &driver.Error{Kind: kindForHTTP(pe.Code), Driver: Name, Op: op, Code: strconv.Itoa(pe.Code), Err: errors.New(pe.Error())}
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		return &driver.Error{Kind: kindForStatus(st), Driver: Name, Op: op, Code: st.Code().String(), Err: errors.New(st.Message())}
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such host") {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
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

// kindForHTTP maps an HTTP status to a driver error kind, or nil.
func kindForHTTP(code int) error {
	switch code {
	case http.StatusUnauthorized:
		return driver.ErrAuth
	case http.StatusForbidden:
		return driver.ErrPermission
	case http.StatusNotFound:
		return driver.ErrNotFound
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return driver.ErrSyntax
	case http.StatusConflict:
		return driver.ErrConflict
	case http.StatusNotImplemented:
		return driver.ErrUnsupported
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return driver.ErrConnection
	}
	return nil
}

// fail maps err and masks the DSN, the password and the API key in its
// message, so no error a conn returns can carry the key.
func (c *conn) fail(ctx context.Context, op string, err error) error {
	err = mapError(ctx, op, err)
	if err == nil {
		return nil
	}
	var de *driver.Error
	if !errors.As(err, &de) {
		return err
	}
	msg := driver.RedactMessage(de.Err.Error(), c.cfg)
	if c.apiKey != "" {
		for _, form := range []string{c.apiKey, url.QueryEscape(c.apiKey), url.PathEscape(c.apiKey)} {
			msg = strings.ReplaceAll(msg, form, "xxxxx")
		}
	}
	if msg == de.Err.Error() {
		return err
	}
	out := *de
	out.Err = errors.New(msg)
	if ctxErr := contextCause(de.Err); ctxErr != nil {
		out.Err = errors.Join(out.Err, ctxErr)
	}
	return &out
}
