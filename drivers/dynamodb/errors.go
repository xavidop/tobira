package dynamodb

import (
	"context"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. A *driver.Error passes
// through. Context errors keep their identity (errors.Is still matches) and
// carry no kind; so does any error while ctx is done, since our own
// cancellation caused it. An AWS API error is classified by its error code,
// which is also stored in Error.Code, and shown by its message alone (no
// request id); an unknown code gets no kind. Transport failures are
// ErrConnection.
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
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code := ae.ErrorCode()
		msg := ae.ErrorMessage()
		if msg == "" {
			msg = code
		}
		return &driver.Error{Kind: kindForCode(code), Driver: Name, Op: op, Code: code, Err: maskedError{msg: msg, err: err}}
	}
	var (
		oe  *net.OpError
		dns *net.DNSError
		ue  *url.Error
		rse *smithyhttp.RequestSendError
	)
	if errors.As(err, &oe) || errors.As(err, &dns) || errors.As(err, &ue) || errors.As(err, &rse) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForCode maps a DynamoDB error code to a driver error kind, or nil.
func kindForCode(code string) error {
	switch code {
	case "ResourceNotFoundException", "TableNotFoundException":
		return driver.ErrNotFound
	case "UnrecognizedClientException", "InvalidSignatureException", "MissingAuthenticationTokenException",
		"ExpiredTokenException", "InvalidClientTokenId", "IncompleteSignature", "IncompleteSignatureException",
		"SignatureDoesNotMatch", "ExpiredToken":
		return driver.ErrAuth
	case "AccessDeniedException", "AccessDenied", "NotAuthorized":
		return driver.ErrPermission
	case "ValidationException", "SerializationException":
		return driver.ErrSyntax
	case "DuplicateItem", "DuplicateItemException", "ItemCollectionSizeLimitExceededException":
		return driver.ErrConstraint
	case "ConditionalCheckFailedException", "TransactionConflictException", "ResourceInUseException",
		"TransactionCanceledException", "IdempotentParameterMismatchException":
		return driver.ErrConflict
	}
	return nil
}

// fail maps err (mapError) and masks the connection's secrets in it.
func (c *conn) fail(ctx context.Context, op string, err error) error {
	return c.scrub(mapError(ctx, op, err))
}

// scrub masks every secret of the connection in the message of a
// *driver.Error, keeping its kind, code and the original error (through
// Unwrap). AWS does not echo secret keys, but a proxy or an emulator
// might.
func (c *conn) scrub(err error) error {
	var de *driver.Error
	if len(c.secrets) == 0 || !errors.As(err, &de) || de.Err == nil {
		return err
	}
	msg := de.Err.Error()
	clean := msg
	for _, s := range c.secrets {
		clean = strings.ReplaceAll(clean, s, redactedMark)
	}
	if clean != msg {
		de.Err = maskedError{msg: clean, err: de.Err}
	}
	return err
}

const redactedMark = "xxxxx"

// secretList returns the credential values to mask, in their own spelling
// and URL-escaped, longest first. Values under 4 bytes are left out: they
// cannot be masked without mangling the message.
func secretList(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if len(v) < 4 {
			continue
		}
		out = append(out, v, url.QueryEscape(v), url.PathEscape(v))
	}
	slices.SortFunc(out, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	return slices.Compact(out)
}

// maskedError shows a chosen message but keeps the original error
// reachable.
type maskedError struct {
	msg string
	err error
}

func (m maskedError) Error() string { return m.msg }
func (m maskedError) Unwrap() error { return m.err }
