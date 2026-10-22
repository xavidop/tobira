package chroma

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestKindForStatus(t *testing.T) {
	for _, tt := range []struct {
		status int
		name   string
		want   error
	}{
		{400, "InvalidArgumentError", driver.ErrSyntax},
		{401, "AuthError", driver.ErrAuth},
		{403, "AuthorizationError", driver.ErrPermission},
		{404, "NotFoundError", driver.ErrNotFound},
		{409, "UniqueConstraintError", driver.ErrConflict},
		{422, "ChromaError", driver.ErrSyntax},
		{429, "RateLimitError", driver.ErrConnection},
		{502, "", driver.ErrConnection},
		{503, "", driver.ErrConnection},
		{504, "", driver.ErrConnection},
		{500, "InvalidArgumentError", driver.ErrSyntax},
		{500, "InternalError", nil},
	} {
		err := mapError(context.Background(), "query", &apiError{status: tt.status, name: tt.name, message: "m"})
		if tt.want == nil {
			if k := driver.KindOf(err); k != nil {
				t.Errorf("%d %s: kind %v, want none", tt.status, tt.name, k)
			}
		} else if !errors.Is(err, tt.want) {
			t.Errorf("%d %s: want %v, got %v", tt.status, tt.name, tt.want, err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name || de.Code == "" {
			t.Errorf("%d: want a *driver.Error with a code, got %#v", tt.status, err)
		}
	}
}

func TestAPIErrorMessageIsCapped(t *testing.T) {
	err := mapError(context.Background(), "query", &apiError{status: 400, name: "InvalidArgumentError", message: strings.Repeat("x", 5000)})
	if len(err.Error()) > 700 {
		t.Errorf("message is %d bytes", len(err.Error()))
	}
}

func TestMapErrorContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mapError(ctx, "query", errors.New("dial failed"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
	err = mapError(context.Background(), "query", &url.Error{Op: "Post", URL: "http://h/x", Err: errors.New("connection refused")})
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("network error: want ErrConnection, got %v", err)
	}
}

func TestConnFailMasksToken(t *testing.T) {
	const tok = "tok/with+odd chars"
	c := newTestConnCfg(t, driver.ConnConfig{Password: driver.NewSecret(tok)})
	for _, msg := range []string{tok, url.QueryEscape(tok), url.PathEscape(tok)} {
		err := c.fail(context.Background(), "query", &apiError{status: 400, name: "x", message: "bad " + msg})
		if strings.Contains(err.Error(), msg) {
			t.Errorf("fail kept %q: %v", msg, err)
		}
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("fail lost the kind: %v", err)
		}
	}
}
