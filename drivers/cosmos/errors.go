package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"

	"github.com/xavidop/tobira/driver"
)

// maxMessage caps a server message kept in an error.
const maxMessage = 500

// mapError classifies err as a *driver.Error. A *driver.Error passes
// through. Context errors keep their identity and carry no kind; so does
// any error while ctx is done. A service error is classified by its HTTP
// status (Code is "status" or "status/substatus") and keeps only the
// server's message, not the SDK's multi-line dump. Network failures are
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
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		msg := serverMessage(re)
		code := strconv.Itoa(re.StatusCode)
		if re.RawResponse != nil {
			if sub := re.RawResponse.Header.Get("x-ms-substatus"); sub != "" && sub != "0" {
				code += "/" + sub
			}
		}
		if re.StatusCode == http.StatusTooManyRequests {
			msg = "throttled: the request rate exceeds the provisioned throughput; retry later or raise the RU/s (" + msg + ")"
		}
		return &driver.Error{Kind: kindForStatus(re.StatusCode, msg), Driver: Name, Op: op, Code: code, Err: serverError{msg: msg, err: re}}
	}
	var ne net.Error
	var ue *url.Error
	if errors.As(err, &ne) || errors.As(err, &ue) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// kindForStatus maps a Cosmos DB status code (and, for 400, its message)
// to a driver error kind, or nil.
func kindForStatus(status int, msg string) error {
	switch status {
	case http.StatusBadRequest:
		lower := strings.ToLower(msg)
		switch {
		case strings.Contains(lower, "owner resource does not exist"):
			return driver.ErrNotFound // the emulator says 400 for a missing container
		case strings.Contains(lower, "composite index"),
			strings.Contains(lower, "cross partition query") && (strings.Contains(lower, "not supported") || strings.Contains(lower, "cannot be directly served") || strings.Contains(lower, "can not be directly served")):
			return driver.ErrUnsupported
		}
		return driver.ErrSyntax
	case http.StatusUnauthorized:
		return driver.ErrAuth
	case http.StatusForbidden:
		return driver.ErrPermission
	case http.StatusNotFound:
		return driver.ErrNotFound
	case http.StatusConflict, http.StatusRequestEntityTooLarge:
		return driver.ErrConstraint
	case http.StatusPreconditionFailed:
		return driver.ErrConflict // ETag mismatch
	case 449:
		// Retry-with: a write raced another on the same item and the SDK
		// gave up retrying; like an ETag mismatch, reload and try again.
		return driver.ErrConflict
	case http.StatusRequestTimeout, http.StatusGone, http.StatusServiceUnavailable,
		http.StatusTooManyRequests: // throttled even after the SDK's retries
		return driver.ErrConnection
	}
	return nil
}

// serverError shows the server's message and unwraps to the SDK error.
type serverError struct {
	msg string
	err error
}

func (e serverError) Error() string { return e.msg }
func (e serverError) Unwrap() error { return e.err }

// serverMessage extracts the human message from a Cosmos DB error body:
// {"code","message"}, {"errors":[{"message"}]} or {"Errors":["…"]},
// flattened to one line and capped. An empty body gives the status text.
func serverMessage(re *azcore.ResponseError) string {
	var body []byte
	if re.RawResponse != nil {
		body, _ = runtime.Payload(re.RawResponse)
	}
	msg := messageFromBody(body)
	if msg == "" {
		msg = http.StatusText(re.StatusCode)
	}
	if msg == "" {
		msg = "status " + strconv.Itoa(re.StatusCode)
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > maxMessage {
		msg = msg[:maxMessage] + "…"
	}
	return msg
}

func messageFromBody(body []byte) string {
	var b struct {
		Message string `json:"message"`
		Errors  []json.RawMessage
	}
	if json.Unmarshal(body, &b) != nil {
		return strings.TrimSpace(string(body))
	}
	var parts []string
	if m := innerMessage(b.Message); m != "" {
		parts = append(parts, m)
	}
	for _, e := range b.Errors {
		var s string
		if json.Unmarshal(e, &s) == nil {
			parts = append(parts, s)
			continue
		}
		var o struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e, &o) == nil && o.Message != "" {
			parts = append(parts, o.Message)
		}
	}
	return strings.Join(parts, "; ")
}

// innerMessage cuts the service's "Message: {…}\r\nActivityId: …, …"
// decoration down to the inner errors, or the first line.
func innerMessage(m string) string {
	if i := strings.Index(m, "{"); i >= 0 {
		if j := strings.LastIndex(m, "}"); j > i {
			if inner := messageFromBody([]byte(m[i : j+1])); inner != "" {
				return inner
			}
		}
	}
	if i := strings.Index(m, "\r\nActivityId"); i >= 0 {
		m = m[:i]
	}
	if i := strings.Index(m, ", ActivityId"); i >= 0 {
		m = m[:i]
	}
	return strings.TrimSpace(m)
}
