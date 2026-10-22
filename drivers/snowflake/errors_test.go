package snowflake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	sf "github.com/snowflakedb/gosnowflake/v2"

	"github.com/xavidop/tobira/driver"
)

func TestErrorMapping(t *testing.T) {
	sfe := func(num int, state string) error {
		return &sf.SnowflakeError{Number: num, SQLState: state, Message: "boom"}
	}
	tests := []struct {
		name string
		err  error
		kind error
		code string
	}{
		{"bad password", sfe(390100, "08004"), driver.ErrAuth, "08004"},
		{"locked user", sfe(390102, ""), driver.ErrAuth, "390102"},
		{"invalid jwt", sfe(390144, "08004"), driver.ErrAuth, "08004"},
		{"invalid oauth token", sfe(390303, "08004"), driver.ErrAuth, "08004"},
		{"expired oauth token", sfe(390318, "08004"), driver.ErrAuth, "08004"},
		{"client: empty password", sfe(260002, ""), driver.ErrAuth, "260002"},
		{"no such object", sfe(2003, "42S02"), driver.ErrNotFound, "42S02"},
		{"object does not exist", sfe(2043, "02000"), driver.ErrNotFound, "02000"},
		{"invalid identifier", sfe(904, "42000"), driver.ErrNotFound, "42000"},
		{"syntax", sfe(1003, "42000"), driver.ErrSyntax, "42000"},
		{"insufficient privileges", sfe(3001, "42501"), driver.ErrPermission, "42501"},
		{"privileges by state", sfe(99999, "42501"), driver.ErrPermission, "42501"},
		{"not null violated", sfe(100072, "22000"), driver.ErrConstraint, "22000"},
		{"integrity by state", sfe(99998, "23505"), driver.ErrConstraint, "23505"},
		{"feature not supported", sfe(2, "0A000"), driver.ErrUnsupported, "0A000"},
		{"connection by state", sfe(1, "08001"), driver.ErrConnection, "08001"},
		{"unknown keeps message", sfe(123456, "XX000"), nil, "XX000"},
		{"wrapped", fmt.Errorf("x: %w", sfe(2003, "42S02")), driver.ErrNotFound, "42S02"},
		{"eof", io.EOF, driver.ErrConnection, ""},
		{"net error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, driver.ErrConnection, ""},
		{"plain error", errors.New("odd"), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mapError(context.Background(), "query", tt.err)
			var de *driver.Error
			if !errors.As(err, &de) {
				t.Fatalf("not a *driver.Error: %v", err)
			}
			if de.Kind != tt.kind || de.Code != tt.code || de.Driver != Name || de.Op != "query" {
				t.Errorf("got kind=%v code=%q driver=%q op=%q", de.Kind, de.Code, de.Driver, de.Op)
			}
			if !errors.Is(err, tt.err) {
				t.Error("the cause is lost")
			}
		})
	}
}

func TestErrorMappingContext(t *testing.T) {
	if mapError(context.Background(), "q", nil) != nil {
		t.Error("mapError(nil) != nil")
	}
	err := mapError(context.Background(), "query", context.Canceled)
	if !errors.Is(err, context.Canceled) || driver.KindOf(err) != nil {
		t.Errorf("canceled: %v (kind %v)", err, driver.KindOf(err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// gosnowflake reports a cancelled query as SQL execution canceled (604).
	err = mapError(ctx, "query", &sf.SnowflakeError{Number: 604, SQLState: "57014", Message: "SQL execution canceled"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a server cancel after ctx ended: %v, want context.Canceled", err)
	}
	err = mapError(ctx, "query", io.EOF)
	if !errors.Is(err, context.Canceled) || errors.Is(err, driver.ErrConnection) {
		t.Errorf("a lost connection after ctx ended: %v, want context.Canceled and no ErrConnection", err)
	}
	// An existing *driver.Error passes through.
	in := driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("x"))
	if mapError(context.Background(), "other", in) != in {
		t.Error("a *driver.Error was rewrapped")
	}
}

func TestScrubError(t *testing.T) {
	cfg := driver.ConnConfig{DSN: "snowflake://u@a", Password: driver.NewSecret("very-secret-token")}
	err := scrubError(driver.Wrap(driver.ErrAuth, Name, "open", errors.New("login failed for very-secret-token")), cfg)
	if strings.Contains(err.Error(), "very-secret-token") {
		t.Errorf("scrubError kept the secret: %v", err)
	}
	if !errors.Is(err, driver.ErrAuth) {
		t.Errorf("scrubError lost the kind: %v", err)
	}
}
