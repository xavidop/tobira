package vertica

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	vertigo "github.com/vertica/vertica-sql-go"

	"github.com/xavidop/tobira/driver"
)

func TestErrorMapping(t *testing.T) {
	ve := func(code, state string) error {
		return &vertigo.VError{Severity: "ERROR", ErrorCode: code, SQLState: state, Message: "boom"}
	}
	tests := []struct {
		name string
		err  error
		kind error
		code string
	}{
		{"bad password", ve("3781", "28000"), driver.ErrAuth, "28000"},
		{"bad password (28P01)", ve("2248", "28P01"), driver.ErrAuth, "28P01"},
		{"no such table", ve("4566", "42V01"), driver.ErrNotFound, "42V01"},
		{"no such column", ve("2624", "42703"), driver.ErrNotFound, "42703"},
		{"no such schema", ve("4650", "3F000"), driver.ErrNotFound, "3F000"},
		{"no such function", ve("3457", "42883"), driver.ErrNotFound, "42883"},
		{"syntax", ve("4856", "42601"), driver.ErrSyntax, "42601"},
		{"permission", ve("4367", "42501"), driver.ErrPermission, "42501"},
		{"read-only transaction", ve("2448", "25006"), driver.ErrReadOnly, "25006"},
		{"read-only transaction (26000)", ve("2448", "26000"), driver.ErrReadOnly, "26000"},
		{"not null", ve("2501", "23502"), driver.ErrConstraint, "23502"},
		{"unique", ve("6745", "23505"), driver.ErrConstraint, "23505"},
		{"unsupported", ve("1", "0A000"), driver.ErrUnsupported, "0A000"},
		{"connection", ve("1", "08006"), driver.ErrConnection, "08006"},
		{"data exception keeps its class", ve("3681", "22P02"), nil, "22P02"},
		{"wrapped", fmt.Errorf("x: %w", ve("4566", "42V01")), driver.ErrNotFound, "42V01"},
		{"eof", io.EOF, driver.ErrConnection, ""},
		{"net error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, driver.ErrConnection, ""},
		{"plain error", errors.New("odd"), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mapError(context.Background(), "query", tt.err)
			var de *driver.Error
			if !errors.As(err, &de) {
				t.Fatalf("mapError = %#v, want *driver.Error", err)
			}
			if de.Kind != tt.kind {
				t.Errorf("Kind = %v, want %v", de.Kind, tt.kind)
			}
			if de.Code != tt.code {
				t.Errorf("Code = %q, want %q", de.Code, tt.code)
			}
			if de.Driver != Name || de.Op != "query" {
				t.Errorf("Driver/Op = %s/%s", de.Driver, de.Op)
			}
		})
	}
	if mapError(context.Background(), "query", nil) != nil {
		t.Error("mapError(nil) != nil")
	}
}

// A cancelled query keeps the context error's identity.
func TestErrorMappingCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mapError(ctx, "query", &vertigo.VError{Severity: "ERROR", ErrorCode: "5001", SQLState: "57014", Message: "Execution canceled by operator"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("mapError(cancelled) = %v, want context.Canceled", err)
	}
	err = mapError(context.Background(), "query", context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("mapError(deadline) = %v", err)
	}
	err = mapError(ctx, "query", io.EOF)
	if !errors.Is(err, context.Canceled) || errors.Is(err, driver.ErrConnection) {
		t.Errorf("mapError(eof after cancel) = %v, want the context error and no ErrConnection", err)
	}
}

func TestScrubError(t *testing.T) {
	cfg := driver.ConnConfig{DSN: "vertica://u@h/db", Password: driver.NewSecret("pa55-word-xyz")}
	err := scrubError(mapError(context.Background(), "open", errors.New("login failed for pa55-word-xyz")), cfg)
	if strings.Contains(err.Error(), "pa55-word-xyz") {
		t.Errorf("scrubError kept the password: %v", err)
	}
}
