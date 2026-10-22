package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/xavidop/tobira/driver"
)

func TestErrorMapping(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code, Message: "boom"} }
	tests := []struct {
		name string
		err  error
		kind error
		code string
	}{
		{"invalid password", pg("28P01"), driver.ErrAuth, "28P01"},
		{"invalid authorization", pg("28000"), driver.ErrAuth, "28000"},
		{"undefined table", pg("42P01"), driver.ErrNotFound, "42P01"},
		{"invalid schema", pg("3F000"), driver.ErrNotFound, "3F000"},
		{"invalid catalog", pg("3D000"), driver.ErrNotFound, "3D000"},
		{"undefined column", pg("42703"), driver.ErrNotFound, "42703"},
		{"read only tx", pg("25006"), driver.ErrReadOnly, "25006"},
		{"insufficient privilege", pg("42501"), driver.ErrPermission, "42501"},
		{"syntax", pg("42601"), driver.ErrSyntax, "42601"},
		{"unique", pg("23505"), driver.ErrConstraint, "23505"},
		{"fk", pg("23503"), driver.ErrConstraint, "23503"},
		{"serialization", pg("40001"), driver.ErrConflict, "40001"},
		{"deadlock", pg("40P01"), driver.ErrConflict, "40P01"},
		{"connection exception", pg("08006"), driver.ErrConnection, "08006"},
		{"too many connections", pg("53300"), driver.ErrConnection, "53300"},
		{"wrapped pg error", fmt.Errorf("ctx: %w", pg("42P01")), driver.ErrNotFound, "42P01"},
		{"connect error", refusedConnectError(t, context.Background()), driver.ErrConnection, ""},
		// pgconn.ConnectError cannot be built with an inner error outside
		// pgx; the mapper reaches the PgError through Unwrap either way.
		{"auth failure inside a connect chain", fmt.Errorf("failed to connect: %w", pg("28P01")), driver.ErrAuth, "28P01"},
		{"invalid text representation", pg("22P02"), driver.ErrSyntax, "22P02"},
		{"invalid datetime format", pg("22007"), driver.ErrSyntax, "22007"},
		{"datetime field overflow", pg("22008"), driver.ErrSyntax, "22008"},
		{"untranslatable character", pg("22P05"), driver.ErrSyntax, "22P05"},
		{"invalid parameter value", pg("22023"), driver.ErrSyntax, "22023"},
		{"null value not allowed", pg("22004"), driver.ErrSyntax, "22004"},
		{"division by zero has no kind", pg("22012"), nil, "22012"},
		{"numeric out of range has no kind", pg("22003"), nil, "22003"},
		{"unclassified", pg("XX000"), nil, "XX000"},
		{"plain error", errors.New("odd"), nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mapError(context.Background(), "query", tt.err)
			var de *driver.Error
			if !errors.As(err, &de) {
				t.Fatalf("not a *driver.Error: %v", err)
			}
			if de.Driver != Name || de.Op != "query" || de.Code != tt.code {
				t.Errorf("got driver=%q op=%q code=%q", de.Driver, de.Op, de.Code)
			}
			if tt.kind == nil && driver.KindOf(err) != nil {
				t.Errorf("kind = %v, want none", driver.KindOf(err))
			}
			if tt.kind != nil && !errors.Is(err, tt.kind) {
				t.Errorf("want %v, got %v", tt.kind, err)
			}
		})
	}
}

func TestErrorMappingNil(t *testing.T) {
	if mapError(context.Background(), "query", nil) != nil {
		t.Error("nil must stay nil")
	}
}

func TestErrorMappingContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want error
	}{
		{"57014 with canceled ctx", ctx, cancelled, context.Canceled},
		{"raw context error", context.Background(), context.DeadlineExceeded, context.DeadlineExceeded},
		{"wrapped context error", context.Background(), fmt.Errorf("x: %w", context.Canceled), context.Canceled},
		{"connect error with canceled ctx", ctx, refusedConnectError(t, ctx), context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mapError(tt.ctx, "query", tt.err)
			if !errors.Is(err, tt.want) {
				t.Errorf("errors.Is(%v, %v) = false", err, tt.want)
			}
			var de *driver.Error
			if !errors.As(err, &de) {
				t.Fatal("not a *driver.Error")
			}
		})
	}
	// 57014 without a context error (statement_timeout) is not a ctx error.
	err := mapError(context.Background(), "query", cancelled)
	if errors.Is(err, context.Canceled) {
		t.Error("57014 without ctx error must not look canceled")
	}
}

func TestErrorAlreadyMapped(t *testing.T) {
	in := driver.Wrap(driver.ErrAuth, Name, "open", errors.New("x"))
	if got := mapError(context.Background(), "query", in); got != in {
		t.Error("a *driver.Error must pass through unchanged")
	}
}

// refusedConnectError dials a closed port and returns the *pgconn.ConnectError.
func refusedConnectError(t *testing.T, ctx context.Context) error {
	t.Helper()
	_, err := pgconn.Connect(ctx, "postgres://u@127.0.0.1:1/db?connect_timeout=2&sslmode=disable")
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("want *pgconn.ConnectError, got %T: %v", err, err)
	}
	return err
}
