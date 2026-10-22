package cosmos

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"

	"github.com/xavidop/tobira/driver"
)

func responseError(status int, substatus, body string) error {
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8081/dbs/app/colls/people/docs", nil)
	h := http.Header{}
	if substatus != "" {
		h.Set("x-ms-substatus", substatus)
	}
	return runtime.NewResponseError(&http.Response{
		StatusCode: status, Status: http.StatusText(status), Header: h, Request: req,
		Body: io.NopCloser(strings.NewReader(body)),
	})
}

func TestMapError(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		err  error
		kind error
		code string
		msg  string
	}{
		{"not found", responseError(404, "", `{"code":"NotFound","message":"Resource Not Found. Learn more: https://aka.ms/cosmosdb-tsg-not-found"}`), driver.ErrNotFound, "404", "Resource Not Found"},
		{"missing container (emulator 400)", responseError(400, "", `{"Errors":["Owner resource does not exist. Database: d, Collection: nope"]}`), driver.ErrNotFound, "400", "Owner resource does not exist"},
		{"missing container (404 sub 1003)", responseError(404, "1003", `{"code":"NotFound","message":"Owner resource does not exist"}`), driver.ErrNotFound, "404/1003", "Owner resource"},
		{"syntax", responseError(400, "", `{"errors":[{"severity":"Error","code":"SC1001","message":"Syntax error, incorrect syntax near 'SELEC'."}]}`), driver.ErrSyntax, "400", "Syntax error"},
		{"composite index", responseError(400, "", `{"code":"BadRequest","message":"The order by query does not have a corresponding composite index that it can be served from."}`), driver.ErrUnsupported, "400", "composite index"},
		{"auth", responseError(401, "", `{"code":"Unauthorized","message":"The input authorization token can't serve the request."}`), driver.ErrAuth, "401", "authorization token"},
		{"forbidden", responseError(403, "", `{"code":"Forbidden","message":"blocked"}`), driver.ErrPermission, "403", "blocked"},
		{"conflict", responseError(409, "", ``), driver.ErrConstraint, "409", "Conflict"},
		{"etag", responseError(412, "", ``), driver.ErrConflict, "412", "Precondition Failed"},
		{"too large", responseError(413, "", `{"code":"RequestEntityTooLarge","message":"Request size is too large"}`), driver.ErrConstraint, "413", "too large"},
		{"unavailable", responseError(503, "", `{"code":"ServiceUnavailable","message":"busy"}`), driver.ErrConnection, "503", "busy"},
		{"throttled", responseError(429, "", `{"code":"TooManyRequests","message":"Request rate is large"}`), driver.ErrConnection, "429", "throttled"},
		{"retry with", responseError(449, "", `{"code":"RetryWith","message":"retry"}`), driver.ErrConflict, "449", "retry"},
		{"network", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, driver.ErrConnection, "", "connection refused"},
	} {
		got := mapError(ctx, "query", tc.err)
		var de *driver.Error
		if !errors.As(got, &de) {
			t.Errorf("%s: mapError gave %T, want *driver.Error", tc.name, got)
			continue
		}
		if de.Kind != tc.kind || de.Code != tc.code || de.Driver != Name || de.Op != "query" {
			t.Errorf("%s: got kind %v code %q driver %q op %q; want kind %v code %q", tc.name, de.Kind, de.Code, de.Driver, de.Op, tc.kind, tc.code)
		}
		if !strings.Contains(got.Error(), tc.msg) {
			t.Errorf("%s: message %q lacks %q", tc.name, got.Error(), tc.msg)
		}
		if strings.Contains(got.Error(), "-----") || strings.Contains(got.Error(), "\n") {
			t.Errorf("%s: message keeps the SDK's multi-line dump: %q", tc.name, got.Error())
		}
	}
	if mapError(ctx, "x", nil) != nil {
		t.Error("mapError(nil) != nil")
	}
	err := mapError(ctx, "query", context.Canceled)
	if !errors.Is(err, context.Canceled) || driver.KindOf(err) != nil {
		t.Errorf("a context error must keep its identity and carry no kind: %v", err)
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if err := mapError(done, "query", errors.New("transport closed")); !errors.Is(err, context.Canceled) {
		t.Errorf("an error after our own cancellation must match context.Canceled: %v", err)
	}
	de := driver.Wrap(driver.ErrSyntax, Name, "query", errors.New("x"))
	if mapError(ctx, "other", de) != de {
		t.Error("a *driver.Error must pass through")
	}
}
