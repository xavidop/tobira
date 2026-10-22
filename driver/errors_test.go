package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

var allKinds = []struct {
	name string
	err  error
}{
	{"not_found", ErrNotFound},
	{"auth", ErrAuth},
	{"permission", ErrPermission},
	{"read_only", ErrReadOnly},
	{"unsupported", ErrUnsupported},
	{"connection", ErrConnection},
	{"syntax", ErrSyntax},
	{"constraint", ErrConstraint},
	{"conflict", ErrConflict},
	{"limit", ErrLimit},
	{"unknown_driver", ErrUnknownDriver},
}

func TestWrapIs(t *testing.T) {
	err := Wrap(ErrNotFound, "pg", "query", io.EOF)
	if !errors.Is(err, ErrNotFound) {
		t.Error("want Is ErrNotFound")
	}
	if !errors.Is(err, io.EOF) {
		t.Error("want Is io.EOF")
	}
	if errors.Is(err, ErrAuth) {
		t.Error("must not match ErrAuth")
	}
	if got := Wrap(ErrNotFound, "pg", "query", nil); got != nil {
		t.Errorf("Wrap(nil) = %v, want nil", got)
	}
}

func TestWrapContext(t *testing.T) {
	err := Wrap(ErrConnection, "pg", "q", context.Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Error("want Is context.Canceled")
	}
	if !errors.Is(err, ErrConnection) {
		t.Error("want Is ErrConnection")
	}
}

func TestErrorString(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want string
	}{
		{"full", &Error{Kind: ErrNotFound, Driver: "postgres", Op: "query", Code: "42P01", Err: errors.New(`relation "x" does not exist`)},
			`postgres: query: relation "x" does not exist (42P01): not found`},
		{"nil kind", &Error{Driver: "postgres", Op: "query", Code: "42P01", Err: errors.New("boom")},
			`postgres: query: boom (42P01)`},
		{"no code", &Error{Kind: ErrSyntax, Driver: "sqlite", Op: "query", Err: errors.New("bad")},
			`sqlite: query: bad: invalid query`},
		{"no driver", &Error{Kind: ErrSyntax, Op: "query", Err: errors.New("bad")},
			`query: bad: invalid query`},
		{"no op", &Error{Kind: ErrSyntax, Driver: "sqlite", Err: errors.New("bad")},
			`sqlite: bad: invalid query`},
		{"no err", &Error{Kind: ErrAuth, Driver: "postgres", Op: "open"},
			`postgres: open: authentication failed`},
		{"no err with code", &Error{Kind: ErrAuth, Driver: "postgres", Op: "open", Code: "28P01"},
			`postgres: open: (28P01): authentication failed`},
		{"kind only", &Error{Kind: ErrConflict}, `conflict`},
		{"empty", &Error{}, ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUnwrapOmitsNil(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want int
	}{
		{"both", &Error{Kind: ErrAuth, Err: io.EOF}, 2},
		{"kind only", &Error{Kind: ErrAuth}, 1},
		{"err only", &Error{Err: io.EOF}, 1},
		{"none", &Error{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Unwrap()
			if len(got) != tt.want {
				t.Fatalf("len(Unwrap()) = %d, want %d", len(got), tt.want)
			}
			for _, e := range got {
				if e == nil {
					t.Error("nil entry in Unwrap()")
				}
			}
		})
	}
}

func TestKindOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"nil", nil, nil},
		{"unclassified", io.EOF, nil},
		{"wrapped", Wrap(ErrAuth, "pg", "open", io.EOF), ErrAuth},
		{"unclassified Error", Wrap(nil, "pg", "open", io.EOF), nil},
		{"fmt wrapped", fmt.Errorf("ctx: %w", Wrap(ErrSyntax, "pg", "q", io.EOF)), ErrSyntax},
		{"bare sentinel", fmt.Errorf("x: %w", ErrConflict), ErrConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KindOf(tt.err); got != tt.want {
				t.Errorf("KindOf = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKindNameRoundTrip(t *testing.T) {
	for _, k := range allKinds {
		t.Run(k.name, func(t *testing.T) {
			if got := KindName(k.err); got != k.name {
				t.Errorf("KindName(%v) = %q, want %q", k.err, got, k.name)
			}
			if got := KindName(Wrap(k.err, "d", "o", io.EOF)); got != k.name {
				t.Errorf("KindName(wrapped) = %q, want %q", got, k.name)
			}
			if got := KindByName(k.name); got != k.err {
				t.Errorf("KindByName(%q) = %v, want %v", k.name, got, k.err)
			}
		})
	}
	if got := KindName(io.EOF); got != "" {
		t.Errorf("KindName(unclassified) = %q, want empty", got)
	}
	if got := KindName(nil); got != "" {
		t.Errorf("KindName(nil) = %q, want empty", got)
	}
	if got := KindByName("nope"); got != nil {
		t.Errorf("KindByName(unknown) = %v, want nil", got)
	}
	if got := KindByName(""); got != nil {
		t.Errorf("KindByName(empty) = %v, want nil", got)
	}
}
