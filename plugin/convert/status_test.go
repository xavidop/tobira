package convert_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

func TestToStatusCodes(t *testing.T) {
	cases := []struct {
		kind error
		code codes.Code
	}{
		{driver.ErrNotFound, codes.NotFound},
		{driver.ErrAuth, codes.Unauthenticated},
		{driver.ErrPermission, codes.PermissionDenied},
		{driver.ErrReadOnly, codes.FailedPrecondition},
		{driver.ErrUnsupported, codes.Unimplemented},
		{driver.ErrConnection, codes.Unavailable},
		{driver.ErrSyntax, codes.InvalidArgument},
		{driver.ErrConstraint, codes.FailedPrecondition},
		{driver.ErrConflict, codes.Aborted},
		{driver.ErrLimit, codes.ResourceExhausted},
		{driver.ErrUnknownDriver, codes.NotFound},
	}
	for _, c := range cases {
		t.Run(driver.KindName(c.kind), func(t *testing.T) {
			in := &driver.Error{Kind: c.kind, Driver: "x", Op: "query", Code: "42P01", Err: errors.New("boom")}
			st := status.Convert(convert.ToStatus(in, nil))
			if st.Code() != c.code {
				t.Fatalf("code %v, want %v", st.Code(), c.code)
			}
			info := convert.ErrorInfoOf(st)
			if info == nil || info.GetKind() != driver.KindName(c.kind) || info.GetCode() != "42P01" ||
				info.GetDriver() != "x" || info.GetOp() != "query" || info.GetMessage() != "boom" {
				t.Fatalf("info %+v", info)
			}
			back := convert.FromStatus(st.Err())
			var de *driver.Error
			if !errors.As(back, &de) || !errors.Is(back, c.kind) {
				t.Fatalf("back %v", back)
			}
			if back.Error() != in.Error() {
				t.Fatalf("message %q, want %q", back.Error(), in.Error())
			}
		})
	}
}

func TestToStatusSpecialErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code codes.Code
		is   error
	}{
		{"canceled", fmt.Errorf("q: %w", context.Canceled), codes.Canceled, context.Canceled},
		{"deadline", driver.Wrap(nil, "x", "query", context.DeadlineExceeded), codes.DeadlineExceeded, context.DeadlineExceeded},
		{"limit", fmt.Errorf("row: %w", convert.ErrLimit), codes.ResourceExhausted, driver.ErrLimit},
		{"invalid", fmt.Errorf("frame: %w", convert.ErrInvalid), codes.InvalidArgument, nil},
		{"plain", errors.New("plain"), codes.Unknown, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := status.Convert(convert.ToStatus(c.err, nil))
			if st.Code() != c.code {
				t.Fatalf("code %v, want %v", st.Code(), c.code)
			}
			if convert.ErrorInfoOf(st) == nil {
				t.Fatal("no ErrorInfo detail")
			}
			back := convert.FromStatus(st.Err())
			if c.is != nil && !errors.Is(back, c.is) {
				t.Fatalf("back %v does not wrap %v", back, c.is)
			}
			want := c.err.Error()
			if c.is == driver.ErrLimit {
				// Classified on the wire, so the rebuilt error names its kind.
				want += ": " + driver.ErrLimit.Error()
			}
			if back.Error() != want {
				t.Fatalf("message %q, want %q", back.Error(), want)
			}
		})
	}
}

func TestToStatusRedacts(t *testing.T) {
	cfg := driver.ConnConfig{DSN: "x://u:hunter22@h/db", Password: driver.NewSecret("t0psecret")}
	in := driver.Wrap(driver.ErrAuth, "x", "open", errors.New("dial x://u:hunter22@h/db failed with t0psecret"))
	err := convert.ToStatus(in, func(s string) string { return driver.RedactMessage(s, cfg) })
	st := status.Convert(err)
	for _, s := range []string{st.Message(), convert.ErrorInfoOf(st).GetMessage(), convert.FromStatus(err).Error()} {
		if strings.Contains(s, "hunter22") || strings.Contains(s, "t0psecret") {
			t.Fatalf("leaked secret: %q", s)
		}
	}
}

func TestToStatusKeepsStatusErrors(t *testing.T) {
	in := convert.StatusError(codes.NotFound, driver.ErrNotFound, "unknown connection handle")
	if got := convert.ToStatus(in, nil); got != in {
		t.Fatalf("status error was rewrapped: %v", got)
	}
	if convert.ToStatus(nil, nil) != nil {
		t.Fatal("nil error must stay nil")
	}
	back := convert.FromStatus(in)
	if !errors.Is(back, driver.ErrNotFound) || !strings.Contains(back.Error(), "unknown connection handle") {
		t.Fatalf("back %v", back)
	}
}

func TestFromStatusWithoutErrorInfo(t *testing.T) {
	cases := []struct {
		code codes.Code
		is   error
	}{
		{codes.Unavailable, driver.ErrConnection},
		{codes.Unimplemented, driver.ErrUnsupported},
		{codes.Canceled, context.Canceled},
		{codes.DeadlineExceeded, context.DeadlineExceeded},
	}
	for _, c := range cases {
		back := convert.FromStatus(status.Error(c.code, "transport says no"))
		if !errors.Is(back, c.is) {
			t.Fatalf("%v: %v does not wrap %v", c.code, back, c.is)
		}
	}
	if convert.FromStatus(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	plain := errors.New("not a status")
	if back := convert.FromStatus(plain); back.Error() != plain.Error() {
		t.Fatalf("plain: %v", back)
	}
}

func TestFromStatusCapsHostileMessages(t *testing.T) {
	huge := strings.Repeat("m", convert.MaxNameBytes*2)
	st, _ := status.New(codes.Internal, huge).WithDetails(&pb.ErrorInfo{Kind: "bogus_kind", Message: huge, Driver: huge, Op: huge, Code: huge})
	back := convert.FromStatus(st.Err())
	if len(back.Error()) > 5*convert.MaxNameBytes {
		t.Fatalf("message not capped: %d bytes", len(back.Error()))
	}
	if driver.KindOf(back) != nil {
		t.Fatalf("unknown kind classified as %v", driver.KindOf(back))
	}
}

func TestToStatusRewrapsForeignStatuses(t *testing.T) {
	// A driver built on a gRPC client can return the upstream status as is:
	// it must be redacted and must not keep the upstream code, which the host
	// would read as the plugin's own (Unavailable -> ErrConnection).
	cfg := driver.ConnConfig{Password: driver.NewSecret("t0psecret")}
	redact := func(s string) string { return driver.RedactMessage(s, cfg) }
	upstream := status.Error(codes.Unavailable, "backend said t0psecret is wrong")
	err := convert.ToStatus(upstream, redact)
	st := status.Convert(err)
	if st.Code() != codes.Unknown {
		t.Fatalf("code %v, want Unknown", st.Code())
	}
	info := convert.ErrorInfoOf(st)
	if info == nil || info.GetKind() != "" {
		t.Fatalf("info %+v", info)
	}
	assertRedacted(t, err, "t0psecret")
	if errors.Is(convert.FromStatus(err), driver.ErrConnection) {
		t.Fatal("an upstream Unavailable leaked through as ErrConnection")
	}

	// Classified by the driver, the kind decides.
	wrapped := driver.Wrap(driver.ErrConnection, "x", "query", status.Error(codes.Internal, "upstream t0psecret"))
	err = convert.ToStatus(wrapped, redact)
	st = status.Convert(err)
	if st.Code() != codes.Unavailable || convert.ErrorInfoOf(st).GetKind() != "connection" {
		t.Fatalf("wrapped: %v %+v", st.Code(), convert.ErrorInfoOf(st))
	}
	assertRedacted(t, err, "t0psecret")
}

func assertRedacted(t *testing.T, err error, secret string) {
	t.Helper()
	st := status.Convert(err)
	for _, s := range []string{st.Message(), convert.ErrorInfoOf(st).GetMessage(), convert.FromStatus(err).Error()} {
		if strings.Contains(s, secret) {
			t.Fatalf("leaked secret: %q", s)
		}
	}
}
