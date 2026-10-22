package bigquery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	bq "cloud.google.com/go/bigquery"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"github.com/xavidop/tobira/driver"
)

func TestMapError(t *testing.T) {
	apiErr := func(code int, reason, msg string) error {
		return &googleapi.Error{Code: code, Message: msg, Errors: []googleapi.ErrorItem{{Reason: reason, Message: msg}}}
	}
	tests := []struct {
		name string
		err  error
		kind error
		code string
	}{
		{"404", apiErr(404, "notFound", "Not found: Table p:ds.nope"), driver.ErrNotFound, "notFound"},
		{"401", apiErr(401, "unauthorized", "Request had invalid authentication credentials"), driver.ErrAuth, "unauthorized"},
		{"403", apiErr(403, "accessDenied", "Access Denied: Table p:ds.t"), driver.ErrPermission, "accessDenied"},
		{"syntax", apiErr(400, "invalidQuery", "Syntax error: Unexpected identifier"), driver.ErrSyntax, "invalidQuery"},
		{"invalid query not found", apiErr(400, "invalidQuery", "Not found: Dataset p:nope was not found in location US"), driver.ErrNotFound, "invalidQuery"},
		{"bytes billed", apiErr(400, "bytesBilledLimitExceeded", "Query exceeded limit for bytes billed: 1000."), driver.ErrLimit, "bytesBilledLimitExceeded"},
		{"job bytes billed", &bq.Error{Reason: "bytesBilledLimitExceeded", Message: "Query exceeded limit for bytes billed: 1000."}, driver.ErrLimit, "bytesBilledLimitExceeded"},
		{"duplicate", apiErr(409, "duplicate", "Already Exists: Table p:ds.t"), driver.ErrConflict, "duplicate"},
		{"unavailable", apiErr(503, "backendError", "Backend error"), driver.ErrConnection, "backendError"},
		{"job notFound", &bq.Error{Reason: "notFound", Message: "Not found: Table p:ds.nope"}, driver.ErrNotFound, "notFound"},
		{"job invalidQuery", &bq.Error{Reason: "invalidQuery", Message: "Syntax error: x"}, driver.ErrSyntax, "invalidQuery"},
		{"emulator not found", &bq.Error{Reason: "jobInternalError", Message: "failed to analyze: Table not found: nope [at 1:15]"}, driver.ErrNotFound, "jobInternalError"},
		{"emulator syntax", &bq.Error{Reason: "jobInternalError", Message: "failed to parse statement: Syntax error: Unexpected identifier"}, driver.ErrSyntax, "jobInternalError"},
		{"oauth", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, driver.ErrAuth, ""},
		{"net", &url.Error{Op: "Post", URL: "http://x", Err: &net.OpError{Op: "dial", Err: errors.New("refused")}}, driver.ErrConnection, ""},
		{"wrapped", fmt.Errorf("x: %w", apiErr(404, "notFound", "gone")), driver.ErrNotFound, "notFound"},
	}
	for _, tt := range tests {
		err := mapError(context.Background(), "query", tt.err)
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name || de.Op != "query" {
			t.Errorf("%s: %v is not a bigquery driver.Error", tt.name, err)
			continue
		}
		if de.Kind != tt.kind {
			t.Errorf("%s: kind = %v, want %v", tt.name, de.Kind, tt.kind)
		}
		if de.Code != tt.code {
			t.Errorf("%s: code = %q, want %q", tt.name, de.Code, tt.code)
		}
	}
}

// A query over the byte cap says how to raise it.
func TestBytesBilledHint(t *testing.T) {
	err := mapError(context.Background(), "query", &bq.Error{Reason: "bytesBilledLimitExceeded", Message: "Query exceeded limit for bytes billed: 1000."})
	if !errors.Is(err, driver.ErrLimit) || !strings.Contains(err.Error(), "max_bytes_billed") {
		t.Errorf("bytes billed error = %v, want ErrLimit naming max_bytes_billed", err)
	}
}

// scrubError leaves no way to reach the unmasked text: not through the
// message, and not through the wrapped API error (whose Body is raw).
func TestScrubErrorHidesNestedErrors(t *testing.T) {
	const secret = "SECRETKEYMATERIAL-0123456789"
	cfg := driver.ConnConfig{DSN: "bigquery://p/ds", Password: driver.NewSecret(secret)}
	inner := &googleapi.Error{Code: 404, Message: "Not found: " + secret, Body: `{"raw":"` + secret + `"}`,
		Errors: []googleapi.ErrorItem{{Reason: "notFound", Message: secret}}}
	err := scrubError(mapError(context.Background(), "query", inner), cfg, []string{secret})
	if strings.Contains(err.Error(), secret) {
		t.Errorf("message holds the secret: %v", err)
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		t.Errorf("the raw API error is still reachable: %v", ge.Body)
	}
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("classification lost: %v", err)
	}
	// Context errors keep their identity through the mask.
	ctxErr := scrubError(driver.Wrap(nil, Name, "query", fmt.Errorf("%s: %w", secret, context.Canceled)), cfg, []string{secret})
	if !errors.Is(ctxErr, context.Canceled) || strings.Contains(ctxErr.Error(), secret) {
		t.Errorf("masked context error = %v", ctxErr)
	}
}

func TestMapErrorContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, in := range []error{context.Canceled, &url.Error{Op: "Post", URL: "http://x", Err: context.Canceled}, errors.New("stream closed")} {
		err := mapError(ctx, "query", in)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("mapError(%v) with a cancelled ctx = %v, want context.Canceled", in, err)
		}
		if errors.Is(err, driver.ErrConnection) {
			t.Errorf("a cancelled call is not a connection failure: %v", err)
		}
	}
	if err := mapError(context.Background(), "query", nil); err != nil {
		t.Errorf("mapError(nil) = %v", err)
	}
}
