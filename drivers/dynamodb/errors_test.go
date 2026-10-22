package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/xavidop/tobira/driver"
)

func TestMapErrorCodes(t *testing.T) {
	for code, kind := range map[string]error{
		"ResourceNotFoundException":                driver.ErrNotFound,
		"UnrecognizedClientException":              driver.ErrAuth,
		"InvalidSignatureException":                driver.ErrAuth,
		"MissingAuthenticationTokenException":      driver.ErrAuth,
		"ExpiredTokenException":                    driver.ErrAuth,
		"AccessDeniedException":                    driver.ErrPermission,
		"ValidationException":                      driver.ErrSyntax,
		"SerializationException":                   driver.ErrSyntax,
		"DuplicateItem":                            driver.ErrConstraint,
		"DuplicateItemException":                   driver.ErrConstraint,
		"ConditionalCheckFailedException":          driver.ErrConflict,
		"TransactionConflictException":             driver.ErrConflict,
		"ResourceInUseException":                   driver.ErrConflict,
		"ItemCollectionSizeLimitExceededException": driver.ErrConstraint,
		"SomethingNew":                             nil,
	} {
		err := mapError(context.Background(), "query", &smithy.GenericAPIError{Code: code, Message: "m"})
		var de *driver.Error
		if !errors.As(err, &de) {
			t.Fatalf("%s: got %T", code, err)
		}
		if de.Kind != kind || de.Code != code || de.Driver != Name || de.Op != "query" {
			t.Errorf("%s: got kind %v code %q driver %q op %q", code, de.Kind, de.Code, de.Driver, de.Op)
		}
	}
}

func TestMapErrorContextAndNetwork(t *testing.T) {
	if err := mapError(context.Background(), "q", nil); err != nil {
		t.Errorf("nil: %v", err)
	}
	err := mapError(context.Background(), "q", fmt.Errorf("wrapped: %w", context.Canceled))
	if !errors.Is(err, context.Canceled) || driver.KindOf(err) != nil {
		t.Errorf("canceled: %v (kind %v)", err, driver.KindOf(err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mapError(ctx, "q", errors.New("some transport error")); !errors.Is(err, context.Canceled) {
		t.Errorf("error while ctx is done: %v", err)
	}
	err = mapError(context.Background(), "q", &net.OpError{Op: "dial", Err: errors.New("refused")})
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("dial: %v", err)
	}
	de := driver.Wrap(driver.ErrReadOnly, Name, "q", errors.New("x"))
	if got := mapError(context.Background(), "other", de); got != de {
		t.Errorf("a *driver.Error must pass through, got %v", got)
	}
}

// A server answering with an AWS JSON error is classified through the real
// SDK stack: bad credentials are ErrAuth, and the secret key never shows,
// even when the server echoes the request's Authorization header.
func TestOpenBadCredentialsAgainstFakeServer(t *testing.T) {
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.WriteHeader(http.StatusBadRequest)
		msg := "The security token included in the request is invalid. " + strings.ReplaceAll(r.Header.Get("Authorization"), `"`, "") + " " + secret
		_, _ = fmt.Fprintf(w, `{"__type":"com.amazon.coral.service#UnrecognizedClientException","message":%q}`, msg)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := New().Open(ctx, driver.ConnConfig{
		DSN:      "dynamodb://AKIDEXAMPLE@us-east-1?endpoint=" + srv.URL,
		Password: driver.NewSecret(secret),
	})
	if err == nil {
		_ = c.Close()
		t.Fatal("Open succeeded against a server refusing the credentials")
	}
	if !errors.Is(err, driver.ErrAuth) {
		t.Errorf("want ErrAuth, got %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the secret key is in the error: %v", err)
	}
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != "UnrecognizedClientException" {
		t.Errorf("want code UnrecognizedClientException, got %v", err)
	}
}
