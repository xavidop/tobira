package bigquery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"cloud.google.com/go/auth"
	bq "cloud.google.com/go/bigquery"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"github.com/xavidop/tobira/driver"
)

// bytesBilledHint is added to a driver.ErrLimit error: a query refused
// because it would bill more than the job's maximumBytesBilled (the DSN's
// max_bytes_billed).
const bytesBilledHint = "the query would bill more than max_bytes_billed allows: narrow it (select fewer columns, filter on the partitioning column) or raise max_bytes_billed in the DSN (0 for no cap)"

// mapError classifies err as a *driver.Error. Context errors keep their
// identity and carry no kind; so does any failure while ctx is done, which
// our own cancellation caused. API errors are classified by HTTP status
// and reason (stored in Error.Code); a job error by its reason. The
// emulator reports every query failure with reason jobInternalError, so a
// missing table or a syntax error is also recognised by its message.
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
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		reason := ""
		if len(ge.Errors) > 0 {
			reason = ge.Errors[0].Reason
		}
		return withHint(&driver.Error{Kind: kindForStatus(ge.Code, reason, ge.Message), Driver: Name, Op: op, Code: reason, Err: err})
	}
	var je *bq.Error
	if errors.As(err, &je) {
		return withHint(&driver.Error{Kind: kindForReason(je.Reason, je.Message), Driver: Name, Op: op, Code: je.Reason, Err: err})
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

// withHint adds the byte-cap hint to a driver.ErrLimit error.
func withHint(de *driver.Error) error {
	if de.Kind == driver.ErrLimit {
		de.Err = fmt.Errorf("%s: %w", bytesBilledHint, de.Err)
	}
	return de
}

// kindForStatus classifies an API error by HTTP status, then by reason.
func kindForStatus(status int, reason, msg string) error {
	switch status {
	case 401:
		return driver.ErrAuth
	case 403:
		switch reason {
		case "quotaExceeded", "rateLimitExceeded", "billingNotEnabled":
			return nil
		}
		if k := kindForReason(reason, msg); k != nil {
			return k
		}
		return driver.ErrPermission
	case 404:
		return driver.ErrNotFound
	case 409:
		return driver.ErrConflict
	case 500, 502, 503, 504:
		if k := kindForReason(reason, msg); k != nil {
			return k
		}
		return driver.ErrConnection
	}
	return kindForReason(reason, msg)
}

// kindForReason classifies a BigQuery error reason
// (https://cloud.google.com/bigquery/docs/error-messages), falling back to
// the message for the emulator's catch-all jobInternalError.
func kindForReason(reason, msg string) error {
	switch reason {
	case "bytesBilledLimitExceeded":
		return driver.ErrLimit
	case "notFound":
		return driver.ErrNotFound
	case "accessDenied":
		return driver.ErrPermission
	case "duplicate":
		return driver.ErrConflict
	case "backendError", "internalError":
		return driver.ErrConnection
	case "invalidQuery", "jobInternalError", "invalid":
		switch {
		case strings.Contains(msg, "Not found:"), strings.Contains(msg, "not found:"), strings.Contains(msg, "Table not found"),
			strings.Contains(msg, "Dataset not found"):
			return driver.ErrNotFound
		case strings.Contains(msg, "Syntax error"):
			return driver.ErrSyntax
		}
		if reason == "invalidQuery" {
			return driver.ErrSyntax
		}
	}
	return nil
}
