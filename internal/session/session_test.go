package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
)

// recorder wraps a driver and records the ConnConfig of every Open.
type recorder struct {
	driver.Driver
	mu   sync.Mutex
	cfgs []driver.ConnConfig
}

func (r *recorder) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	r.mu.Lock()
	r.cfgs = append(r.cfgs, cfg)
	r.mu.Unlock()
	return r.Driver.Open(ctx, cfg)
}

func newManager(t *testing.T, readOnly bool, conns ...config.Connection) (*Manager, *drivertest.Fake, *recorder) {
	t.Helper()
	f := drivertest.NewFake(drivertest.WithPeople())
	rec := &recorder{Driver: f.Driver()}
	reg := driver.NewRegistry()
	if err := reg.Add(rec); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: conns}
	return New(cfg, reg, secrets.Resolver{}, readOnly), f, rec
}

func opens(f *drivertest.Fake) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Op == "open" {
			n++
		}
	}
	return n
}

var fakeConn = config.Connection{Name: "dev", Driver: "fake", DSN: "fake://x", Color: "blue"}

func TestOpenOnce(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, f, _ := newManager(t, false, fakeConn)
	f.SetLatency(20 * 1e6)
	var wg sync.WaitGroup
	conns := make([]driver.Conn, 10)
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := m.Open(context.Background(), "dev")
			if err != nil {
				t.Error(err)
			}
			conns[i] = c
		}()
	}
	wg.Wait()
	if n := opens(f); n != 1 {
		t.Fatalf("driver opens = %d, want 1", n)
	}
	for _, c := range conns {
		if c == nil || c != conns[0] {
			t.Fatal("Opens returned different connections")
		}
	}
	if c, ok := m.Conn("dev"); !ok || c != conns[0] {
		t.Fatal("Conn did not return the open connection")
	}
	if _, err := m.Open(context.Background(), "dev"); err != nil || opens(f) != 1 {
		t.Fatalf("Ready Open must return at once: err=%v opens=%d", err, opens(f))
	}
	if err := m.CloseAll(); err != nil {
		t.Fatal(err)
	}
}

func TestStatusTransitions(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, f, _ := newManager(t, false, fakeConn)
	if s := m.State("dev"); s.Status != Idle || s.Name != "dev" {
		t.Fatalf("initial state = %+v", s)
	}
	f.SetLatency(100 * 1e6)
	done := make(chan error, 1)
	go func() { _, err := m.Open(context.Background(), "dev"); done <- err }()
	// Poll without spinning until the open (100ms of latency) finishes.
	sawConnecting := false
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timeout := time.After(5 * time.Second)
wait:
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			break wait
		case <-tick.C:
			sawConnecting = sawConnecting || m.State("dev").Status == Connecting
		case <-timeout:
			t.Fatal("open did not finish")
		}
	}
	if !sawConnecting {
		t.Fatal("never observed Connecting")
	}
	s := m.State("dev")
	if s.Status != Ready || s.Err != "" || s.Info.Name != "fake" {
		t.Fatalf("ready state = %+v", s)
	}
	if err := m.Close("dev"); err != nil {
		t.Fatal(err)
	}
	if s := m.State("dev"); s.Status != Idle {
		t.Fatalf("after Close = %+v", s)
	}
	if _, ok := m.Conn("dev"); ok {
		t.Fatal("Conn must be false after Close")
	}
}

func TestOpenFailureRedacted(t *testing.T) {
	defer goleak.VerifyNone(t)
	c := config.Connection{Name: "bad", Driver: "fake", DSN: "fake://u:s3cret@h/db"}
	m, f, _ := newManager(t, false, c)
	f.FailNext("open", errors.New("bad postgres://u:s3cret@h/db"))
	_, err := m.Open(context.Background(), "bad")
	if err == nil {
		t.Fatal("want error")
	}
	s := m.State("bad")
	if s.Status != Failed || s.Err == "" {
		t.Fatalf("state = %+v", s)
	}
	if strings.Contains(s.Err, "s3cret") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("secret leaked: state=%q err=%q", s.Err, err)
	}
	if _, err := m.Open(context.Background(), "bad"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if s := m.State("bad"); s.Status != Ready || s.Err != "" {
		t.Fatalf("after retry = %+v", s)
	}
	if opens(f) != 2 {
		t.Fatalf("opens = %d, want 2", opens(f))
	}
	_ = m.CloseAll()
}

func TestOpenUnknownAndCancelledWaiter(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, f, _ := newManager(t, false, fakeConn)
	if _, err := m.Open(context.Background(), "nope"); err == nil {
		t.Fatal("want unknown-connection error")
	}
	f.SetLatency(100 * 1e6)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Open(ctx, "dev"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The shared open keeps going; a later Open joins or reuses it.
	if _, err := m.Open(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if n := opens(f); n != 1 {
		t.Fatalf("opens = %d, want 1", n)
	}
	_ = m.CloseAll()
}

func TestForceReadOnly(t *testing.T) {
	for _, tt := range []struct {
		name  string
		force bool
		conn  bool
		want  bool
	}{
		{"neither", false, false, false},
		{"config only", false, true, true},
		{"forced", true, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer goleak.VerifyNone(t)
			c := fakeConn
			c.ReadOnly = tt.conn
			m, _, rec := newManager(t, tt.force, c)
			if got := m.State("dev").ReadOnly; got != tt.want {
				t.Fatalf("State.ReadOnly = %v, want %v", got, tt.want)
			}
			if _, err := m.Open(context.Background(), "dev"); err != nil {
				t.Fatal(err)
			}
			if got := rec.cfgs[0].ReadOnly; got != tt.want {
				t.Fatalf("driver ReadOnly = %v, want %v", got, tt.want)
			}
			_ = m.CloseAll()
		})
	}
}

func TestAddDSN(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, _, rec := newManager(t, true, fakeConn)
	dsn := "fake://u:s3cret@h/db"
	name, err := m.AddDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if name != driver.RedactDSN(dsn) || strings.Contains(name, "s3cret") {
		t.Fatalf("name = %q", name)
	}
	if again, err := m.AddDSN(dsn); err != nil || again != name {
		t.Fatalf("second AddDSN = %q, %v", again, err)
	}
	if _, err := m.AddDSN("fake://other/db"); err != nil {
		t.Fatal(err)
	}
	clash := config.Connection{Name: "fake://h/db", Driver: "fake", DSN: "fake://h/db"}
	m2, _, _ := newManager(t, false, clash)
	if _, err := m2.AddDSN("fake://h/db"); err == nil {
		t.Fatal("want collision error")
	}
	names := m.Names()
	if len(names) != 3 || names[0] != "dev" || names[1] != name {
		t.Fatalf("Names = %v", names)
	}
	s := m.State(name)
	if s.Status != Idle || !s.ReadOnly || s.Info.Name != "fake" {
		t.Fatalf("state = %+v", s)
	}
	if _, err := m.Open(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	if rec.cfgs[0].DSN != dsn || !rec.cfgs[0].ReadOnly {
		t.Fatalf("driver cfg = %+v", rec.cfgs[0])
	}
	if m.Color(name) != "" || m.Color("dev") != "blue" {
		t.Fatalf("colors: %q %q", m.Color(name), m.Color("dev"))
	}
	_ = m.CloseAll()
}

func TestCloseAll(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, _, _ := newManager(t, false, fakeConn, config.Connection{Name: "two", Driver: "fake", DSN: "fake://2"})
	for _, n := range []string{"dev", "two"} {
		if _, err := m.Open(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.CloseAll(); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseAll(); err != nil {
		t.Fatalf("second CloseAll: %v", err)
	}
	for _, n := range []string{"dev", "two"} {
		if s := m.State(n); s.Status != Idle {
			t.Fatalf("%s = %+v", n, s)
		}
	}
	if _, err := m.Open(context.Background(), "dev"); err != nil {
		t.Fatalf("reopen after CloseAll: %v", err)
	}
	_ = m.CloseAll()
}

func TestRedactedErrorKeepsKind(t *testing.T) {
	for _, kind := range []error{driver.ErrAuth, driver.ErrConnection} {
		t.Run(kind.Error(), func(t *testing.T) {
			defer goleak.VerifyNone(t)
			c := config.Connection{Name: "bad", Driver: "fake", DSN: "fake://u:s3cret@h/db"}
			m, f, _ := newManager(t, false, c)
			f.FailNext("open", fmt.Errorf("%w: postgres://u:s3cret@h/db", kind))
			_, err := m.Open(context.Background(), "bad")
			if !errors.Is(err, kind) {
				t.Fatalf("errors.Is(%v, %v) = false", err, kind)
			}
			if strings.Contains(err.Error(), "s3cret") || strings.Contains(m.State("bad").Err, "s3cret") {
				t.Fatalf("secret leaked: %q / %q", err, m.State("bad").Err)
			}
			if u := errors.Unwrap(err); u != nil {
				t.Fatalf("Unwrap exposes %v", u)
			}
			var de *driver.Error
			if errors.As(err, &de) && strings.Contains(de.Error(), "s3cret") {
				t.Fatalf("As leaks: %v", de)
			}
		})
	}
}

// closeCounter wraps a driver and counts Close calls on the conns it opens.
type closeCounter struct {
	driver.Driver
	closed atomic.Int32
}

type countedConn struct {
	driver.Conn
	d *closeCounter
}

func (c countedConn) Close() error { c.d.closed.Add(1); return c.Conn.Close() }

func (d *closeCounter) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return countedConn{c, d}, nil
}

func counted(t *testing.T) (*Manager, *drivertest.Fake, *closeCounter) {
	t.Helper()
	f := drivertest.NewFake(drivertest.WithPeople())
	cc := &closeCounter{Driver: f.Driver()}
	reg := driver.NewRegistry()
	if err := reg.Add(cc); err != nil {
		t.Fatal(err)
	}
	return New(&config.Config{Connections: []config.Connection{fakeConn}}, reg, secrets.Resolver{}, false), f, cc
}

func TestCloseDuringConnecting(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, f, cc := counted(t)
	f.SetLatency(100 * time.Millisecond)
	errc := make(chan error, 1)
	go func() { _, err := m.Open(context.Background(), "dev"); errc <- err }()
	for m.State("dev").Status != Connecting {
		time.Sleep(time.Millisecond)
	}
	if err := m.Close("dev"); err != nil {
		t.Fatal(err)
	}
	if s := m.State("dev"); s.Status != Idle {
		t.Fatalf("after Close = %+v", s)
	}
	if err := <-errc; !errors.Is(err, driver.ErrConnection) {
		t.Fatalf("waiter err = %v, want ErrConnection", err)
	}
	if n := cc.closed.Load(); n != 1 {
		t.Fatalf("discarded conn closed %d times, want 1", n)
	}
	if s := m.State("dev"); s.Status != Idle {
		t.Fatalf("after open completed = %+v", s)
	}
	if _, ok := m.Conn("dev"); ok {
		t.Fatal("Conn must be false")
	}
}

func TestConcurrentOpenClose(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, f, _ := counted(t)
	f.SetLatency(time.Millisecond)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 30 {
				switch (i + j) % 4 {
				case 0, 1:
					_, _ = m.Open(context.Background(), "dev")
				case 2:
					_ = m.Close("dev")
				default:
					_ = m.CloseAll()
				}
				_ = m.State("dev")
			}
		}()
	}
	wg.Wait()
	if err := m.CloseAll(); err != nil {
		t.Fatal(err)
	}
	if s := m.State("dev"); s.Status != Idle {
		t.Fatalf("final = %+v", s)
	}
}

func TestRedact(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	res := secrets.Resolver{Getenv: func(k string) string {
		if k == secrets.EnvName("dev") {
			return "pw-Secret-9"
		}
		return ""
	}}
	m := New(&config.Config{Connections: []config.Connection{
		{Name: "dev", Driver: "fake", DSN: "fake://u:s3cretpw@host/db"},
	}}, reg, res, false)
	t.Cleanup(func() { _ = m.CloseAll() })

	// Before any open only the DSN is known.
	if got := m.Redact("dev", "dial fake://u:s3cretpw@host/db failed"); strings.Contains(got, "s3cretpw") {
		t.Fatalf("DSN secret leaked: %q", got)
	}
	c, err := m.Open(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	f.FailNext("collections", errors.New("server said pw-Secret-9 is wrong"))
	_, err = c.Collections(context.Background(), driver.NamespaceRef{})
	if err == nil {
		t.Fatal("expected failure")
	}
	if got := m.Redact("dev", err.Error()); strings.Contains(got, "pw-Secret-9") {
		t.Fatalf("resolved secret leaked: %q", got)
	}
	if got := m.Redact("missing", "password=hunter2 bad"); strings.Contains(got, "hunter2") {
		t.Fatalf("generic redaction missing: %q", got)
	}
}

func TestReload(t *testing.T) {
	defer goleak.VerifyNone(t)
	two := config.Connection{Name: "two", Driver: "fake", DSN: "fake://2"}
	three := config.Connection{Name: "three", Driver: "fake", DSN: "fake://3", Tags: []string{"a"}}
	m, _, _ := newManager(t, false, fakeConn, two, three)
	ctx := context.Background()
	for _, n := range []string{"dev", "two", "three"} {
		if _, err := m.Open(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	adhoc, err := m.AddDSN("fake://adhoc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(ctx, adhoc); err != nil {
		t.Fatal(err)
	}

	changedTwo := two
	changedTwo.ReadOnly = true
	changedThree := three
	changedThree.Tags = []string{"a"} // equal content, distinct slice: unchanged
	fresh := config.Connection{Name: "fresh", Driver: "fake", DSN: "fake://new"}
	added, changed, removed := m.Reload(&config.Config{Connections: []config.Connection{fresh, changedTwo, changedThree}})

	if got := fmt.Sprint(added, changed, removed); got != "[fresh] [two] [dev]" {
		t.Fatalf("added/changed/removed = %s", got)
	}
	if got, want := fmt.Sprint(m.Names()), "[fresh two three "+adhoc+"]"; got != want {
		t.Fatalf("Names = %s, want %s", got, want)
	}
	if s := m.State("dev"); s.Status != Idle || s.Name != "dev" {
		t.Fatalf("removed connection state = %+v", s)
	}
	if _, ok := m.Conn("dev"); ok {
		t.Fatal("removed connection still open")
	}
	if s := m.State("two"); s.Status != Idle || !s.ReadOnly {
		t.Fatalf("changed connection = %+v, want closed and read-only", s)
	}
	if s := m.State("three"); s.Status != Ready {
		t.Fatalf("unchanged connection = %+v, want still Ready", s)
	}
	if s := m.State(adhoc); s.Status != Ready {
		t.Fatalf("ad-hoc connection = %+v, want untouched", s)
	}
	if s := m.State("fresh"); s.Status != Idle {
		t.Fatalf("added = %+v", s)
	}
	// A changed connection reopens lazily with its new config.
	if _, err := m.Open(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	// Reload again with the same config is a no-op.
	a, c, r := m.Reload(&config.Config{Connections: []config.Connection{fresh, changedTwo, changedThree}})
	if len(a)+len(c)+len(r) != 0 {
		t.Fatalf("idempotent reload reported %v %v %v", a, c, r)
	}
	if err := m.CloseAll(); err != nil {
		t.Fatal(err)
	}
}

func TestRedactKeepsResolvedSecretsAfterReset(t *testing.T) {
	defer goleak.VerifyNone(t)
	res := secrets.Resolver{Getenv: func(k string) string {
		if k == secrets.EnvName("dev") {
			return "hunter2-secret"
		}
		return ""
	}}
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	m := New(&config.Config{Connections: []config.Connection{fakeConn}}, reg, res, false)
	if _, err := m.Open(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	if got := m.Redact("dev", "boom hunter2-secret"); strings.Contains(got, "hunter2-secret") {
		t.Fatalf("secret not masked while open: %q", got)
	}
	changed := fakeConn
	changed.Color = "red"
	m.Reload(&config.Config{Connections: []config.Connection{changed}})
	// A late error after the reset is still masked.
	if got := m.Redact("dev", "boom hunter2-secret"); strings.Contains(got, "hunter2-secret") {
		t.Fatalf("secret forgotten on reload: %q", got)
	}
	_ = m.Close("dev")
	if got := m.Redact("dev", "boom hunter2-secret"); strings.Contains(got, "hunter2-secret") {
		t.Fatalf("secret forgotten on close: %q", got)
	}
	// Removing the connection from the config drops it.
	m.Reload(&config.Config{})
	if got := m.Redact("dev", "boom hunter2-secret"); !strings.Contains(got, "hunter2-secret") {
		t.Fatalf("removed connection still tracked: %q", got)
	}
	_ = m.CloseAll()
}

func TestReloadNil(t *testing.T) {
	m, _, _ := newManager(t, false, fakeConn)
	a, c, r := m.Reload(nil)
	if len(a)+len(c) != 0 || fmt.Sprint(r) != "[dev]" {
		t.Fatalf("%v %v %v", a, c, r)
	}
}

func TestTestConnection(t *testing.T) {
	defer goleak.VerifyNone(t)
	res := secrets.Resolver{Getenv: func(k string) string {
		if k == "PROBE_PW" {
			return "hunter2-secret"
		}
		return ""
	}, Resolve: func(_ context.Context, ref string) ([]byte, error) {
		if ref == "env:PROBE_PW" {
			return []byte("hunter2-secret"), nil
		}
		return nil, errors.New("no such ref")
	}}
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	m := New(&config.Config{}, reg, res, false)
	ctx := context.Background()
	c := config.Connection{Name: "probe", Driver: "fake", DSN: "fake://x", Password: "env:PROBE_PW"}

	if err := m.Test(ctx, c); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if len(m.Names()) != 0 {
		t.Fatalf("Test registered a connection: %v", m.Names())
	}
	if n := opens(f); n != 1 {
		t.Fatalf("opens = %d", n)
	}

	f.FailNext("open", errors.New("auth failed for password=hunter2-secret at fake://u:hunter2-secret@x"))
	err := m.Test(ctx, c)
	if err == nil || strings.Contains(err.Error(), "hunter2-secret") {
		t.Fatalf("err = %v, want a redacted failure", err)
	}

	bad := c
	bad.Password = "env:MISSING"
	if err := m.Test(ctx, bad); err == nil {
		t.Fatal("unresolvable ref must fail")
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := m.Test(cctx, c); err == nil {
		t.Fatal("cancelled Test must fail")
	}
}

func TestDrivers(t *testing.T) {
	m, _, _ := newManager(t, false)
	if d := m.Drivers(); len(d) != 1 || d[0].Name != "fake" {
		t.Fatalf("Drivers = %+v", d)
	}
}

func TestResolveFailureRedactsPasswordSetting(t *testing.T) {
	defer goleak.VerifyNone(t)
	const secret = "hunter2-literal"
	// The resolver error embeds the configured password value; both the
	// background open and Test must mask it.
	c := config.Connection{Name: "pw-" + secret, Driver: "fake", DSN: "fake://u:dsnsecret@h/db", Password: secret}
	reg := driver.NewRegistry()
	m := New(&config.Config{Connections: []config.Connection{c}}, reg, secrets.Resolver{
		Resolve: func(_ context.Context, ref string) ([]byte, error) {
			return nil, errors.New("cannot resolve " + ref)
		},
	}, false)
	_, err := m.Open(context.Background(), c.Name)
	if err == nil {
		t.Fatal("want error")
	}
	testErr := m.Test(context.Background(), c)
	if testErr == nil {
		t.Fatal("want Test error")
	}
	for _, got := range []string{err.Error(), testErr.Error(), m.State(c.Name).Err} {
		if strings.Contains(got, secret) || strings.Contains(got, "dsnsecret") || !strings.Contains(got, "xxxxx") {
			t.Errorf("resolution failure not redacted: %q", got)
		}
	}
}

func TestBrokenEntries(t *testing.T) {
	defer goleak.VerifyNone(t)
	good := config.Connection{Name: "dev", Driver: "fake", DSN: "fake://dev"}
	cfg := &config.Config{
		Connections: []config.Connection{good},
		Broken: []config.BrokenConnection{
			{Name: "leaky", Line: 5, Err: "password must be a secret reference"},
			{Name: "dev", Line: 9, Err: "duplicate name"},
			{Line: 11, Err: "name is required"},
		},
	}
	reg := driver.NewRegistry()
	f := drivertest.NewFake(drivertest.WithPeople())
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	m := New(cfg, reg, secrets.Resolver{}, false)
	want := "[dev leaky (entry at line 9) (entry at line 11)]"
	if got := fmt.Sprint(m.Names()); got != want {
		t.Fatalf("Names = %s, want %s", got, want)
	}
	s := m.State("leaky")
	if !s.Broken || s.Status != Failed || s.Err != "password must be a secret reference" {
		t.Fatalf("State(leaky) = %+v", s)
	}
	if m.State("dev").Broken {
		t.Fatal("the good dev entry is marked broken")
	}
	if _, err := m.Open(context.Background(), "leaky"); err == nil || err.Error() != s.Err {
		t.Fatalf("Open(leaky) err = %v, want %q", err, s.Err)
	}
	if _, err := m.Open(context.Background(), "dev"); err != nil {
		t.Fatalf("Open(dev): %v", err)
	}
	if got := m.Broken(); len(got) != 3 || got[0].Name != "leaky" || got[2].Line != 11 {
		t.Fatalf("Broken = %+v", got)
	}

	// Repair leaky; break dev's duplicate away; the good one stays open.
	fixed := config.Connection{Name: "leaky", Driver: "fake", DSN: "fake://leaky"}
	added, changed, removed := m.Reload(&config.Config{Connections: []config.Connection{good, fixed}})
	if fmt.Sprint(added, changed, removed) != "[] [leaky] [(entry at line 9) (entry at line 11)]" {
		t.Fatalf("added/changed/removed = %v %v %v", added, changed, removed)
	}
	if s := m.State("leaky"); s.Broken || s.Status != Idle || s.Err != "" {
		t.Fatalf("repaired State = %+v", s)
	}
	if _, ok := m.Conn("dev"); !ok {
		t.Fatal("good dev was closed by the reload")
	}
	// And back to broken.
	m.Reload(&config.Config{Connections: []config.Connection{good}, Broken: []config.BrokenConnection{{Name: "leaky", Line: 5, Err: "x"}}})
	if s := m.State("leaky"); !s.Broken || s.Status != Failed {
		t.Fatalf("re-broken State = %+v", s)
	}
	_ = m.CloseAll()
}

func TestBrokenEntriesAreNeverDropped(t *testing.T) {
	cfg := &config.Config{
		Connections: []config.Connection{{Name: "(entry at line 9)", Driver: "fake", DSN: "fake://x"}},
		Broken: []config.BrokenConnection{
			{Line: 9, Err: "a"}, {Line: 9, Err: "b"}, {Name: "dup", Line: 3, Err: "c"}, {Name: "dup", Line: 3, Err: "d"},
		},
	}
	for _, reload := range []bool{false, true} {
		m := New(&config.Config{}, driver.NewRegistry(), secrets.Resolver{}, false)
		if reload {
			m.Reload(cfg)
		} else {
			m = New(cfg, driver.NewRegistry(), secrets.Resolver{}, false)
		}
		if got := len(m.Broken()); got != 4 || len(m.Names()) != 5 {
			t.Errorf("reload=%v: %d broken, names %v", reload, got, m.Names())
		}
	}
}

func TestRedactMasksBothTheOldResolvedConfigAndTheNewDSN(t *testing.T) {
	defer goleak.VerifyNone(t)
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	old := config.Connection{Name: "dev", Driver: "fake", DSN: "fake://u:oldpw-1234@h/db"}
	m := New(&config.Config{Connections: []config.Connection{old}}, reg, secrets.Resolver{}, false)
	if _, err := m.Open(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}
	// The DSN changes (and with it the password); the old connection closes.
	next := old
	next.DSN = "fake://u:newpw-5678@h/db"
	if _, changed, _ := m.Reload(&config.Config{Connections: []config.Connection{next}}); len(changed) != 1 {
		t.Fatalf("changed = %v", changed)
	}
	got := m.Redact("dev", "late error: oldpw-1234 and newpw-5678")
	for _, secret := range []string{"oldpw-1234", "newpw-5678"} {
		if strings.Contains(got, secret) {
			t.Errorf("Redact left %q in %q", secret, got)
		}
	}
	// A connection that never opened still masks its (new) DSN.
	m2 := New(&config.Config{Connections: []config.Connection{next}}, reg, secrets.Resolver{}, false)
	if got := m2.Redact("dev", "newpw-5678"); strings.Contains(got, "newpw-5678") {
		t.Errorf("unopened connection leaks its DSN password: %q", got)
	}
	_ = m.CloseAll()
}

func TestBrokenKeyRedactsName(t *testing.T) {
	defer goleak.VerifyNone(t)
	raw := "postgres://u:supersecret@h/db"
	cfg := &config.Config{Broken: []config.BrokenConnection{{Name: raw, Line: 3, Err: "bad"}}}
	m := New(cfg, driver.NewRegistry(), secrets.Resolver{}, false)
	names := m.Names()
	if len(names) != 1 || strings.Contains(names[0], "supersecret") {
		t.Fatalf("Names = %q, want one redacted key", names)
	}
	b, ok := m.BrokenEntry(names[0])
	if !ok || b.Name != raw {
		t.Fatalf("BrokenEntry keeps the raw name: got %+v ok=%v", b, ok)
	}
}

func TestBrokenKeyStableAcrossLineShift(t *testing.T) {
	defer goleak.VerifyNone(t)
	mk := func(line int) *config.Config {
		return &config.Config{Broken: []config.BrokenConnection{
			{Name: "alpha", Line: line, Err: "bad"}, {Line: line + 4, Err: "bad"},
		}}
	}
	m := New(mk(3), driver.NewRegistry(), secrets.Resolver{}, false)
	if got := fmt.Sprint(m.Names()); got != "[alpha (entry at line 7)]" {
		t.Fatalf("Names = %s", got)
	}
	// An edit above moves both entries down; the named one keeps its key.
	m.Reload(mk(5))
	if got := fmt.Sprint(m.Names()); got != "[alpha (entry at line 9)]" {
		t.Fatalf("Names after shift = %s", got)
	}
	if b, ok := m.BrokenEntry("alpha"); !ok || b.Line != 5 {
		t.Fatalf("alpha = %+v ok=%v", b, ok)
	}
}

// ConfigGeneration identifies a connection's configuration: it changes when
// a Reload changes the entry, and a name reused after a removal never gets an
// earlier generation back. Unknown names are 0.
func TestConfigGeneration(t *testing.T) {
	m, _, _ := newManager(t, false, fakeConn)
	g1 := m.ConfigGeneration("dev")
	if g1 == 0 || m.ConfigGeneration("nope") != 0 {
		t.Fatalf("generations: dev %d, unknown %d", g1, m.ConfigGeneration("nope"))
	}
	m.Reload(&config.Config{Connections: []config.Connection{fakeConn}})
	if m.ConfigGeneration("dev") != g1 {
		t.Fatal("an unchanged reload changed the generation")
	}
	changed := fakeConn
	changed.DSN = "fake://y"
	m.Reload(&config.Config{Connections: []config.Connection{changed}})
	g2 := m.ConfigGeneration("dev")
	if g2 == g1 || g2 == 0 {
		t.Fatalf("a changed entry kept generation %d", g2)
	}
	m.Reload(&config.Config{})
	m.Reload(&config.Config{Connections: []config.Connection{fakeConn}})
	if g3 := m.ConfigGeneration("dev"); g3 == g1 || g3 == g2 || g3 == 0 {
		t.Fatalf("a re-added name reused generation %d", g3)
	}
	if err := m.Close("dev"); err != nil {
		t.Fatal(err)
	}
	if m.ConfigGeneration("dev") == 0 {
		t.Fatal("close lost the generation")
	}
	name, err := m.AddDSN("fake://adhoc")
	if err != nil || m.ConfigGeneration(name) == 0 {
		t.Fatalf("ad-hoc generation: %v", err)
	}
}
