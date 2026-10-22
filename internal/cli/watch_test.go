package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

var watchPeople = driver.CollectionRef{Namespace: "main", Name: "tobira_people"}

// watchRun is a `tobira watch` running in the background.
type watchRun struct {
	stdout, stderr string
	code           int
	done           chan struct{}
}

// startWatch runs `tobira watch args...` and waits until its watch is
// established.
func startWatch(t *testing.T, h *harness, ctx context.Context, args ...string) *watchRun {
	t.Helper()
	r := &watchRun{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.stdout, r.stderr, r.code = h.exec(t, ctx, append([]string{"watch"}, args...)...)
	}()
	deadline := time.After(5 * time.Second)
	for h.fake.Watchers() == 0 {
		select {
		case <-r.done:
			t.Fatalf("watch ended before watching: code=%d stderr=%q", r.code, r.stderr)
		case <-deadline:
			t.Fatal("the watch did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	return r
}

func (r *watchRun) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not return")
	}
}

func watchEvent(i int64, op string) driver.Event {
	return driver.Event{
		Time: time.Date(2026, 10, 20, 9, 30, int(i), 0, time.UTC), Op: op, Channel: "tobira_people",
		Key:     driver.Object{{Key: "id", Value: driver.Int(i)}},
		Payload: driver.ObjectValue(driver.Member{Key: "name", Value: driver.String("person")}),
	}
}

func TestWatchJSONLinesUntilMax(t *testing.T) {
	h := newHarness(t)
	r := startWatch(t, h, context.Background(), "local-pg", "main.tobira_people", "--max", "2")
	h.fake.Emit(watchPeople, watchEvent(1, "insert"))
	h.fake.Emit(watchPeople, watchEvent(2, "update"))
	r.wait(t)
	if r.code != 0 || r.stderr != "" {
		t.Fatalf("code=%d stderr=%q, want 0", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2:\n%s", len(lines), r.stdout)
	}
	var ev struct {
		Time    string          `json:"time"`
		Op      string          `json:"op"`
		Channel string          `json:"channel"`
		Key     map[string]any  `json:"key"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("line 1 is not JSON: %v\n%s", err, lines[0])
	}
	if ev.Op != "insert" || ev.Channel != "tobira_people" || ev.Key["id"] != float64(1) ||
		!strings.HasPrefix(ev.Time, "2026-10-20T09:30:01") || string(ev.Payload) != `{"name":"person"}` {
		t.Fatalf("line 1 = %+v", ev)
	}
	if !strings.Contains(lines[1], `"op":"update"`) {
		t.Fatalf("line 2 = %s", lines[1])
	}
	if n := h.fake.Watchers(); n != 0 {
		t.Fatalf("%d watches left open", n)
	}
}

func TestWatchTargetIsAChannel(t *testing.T) {
	h := newHarness(t)
	r := startWatch(t, h, context.Background(), "ro", "orders", "--max", "1") // read-only connections may watch
	h.fake.Emit(driver.CollectionRef{Name: "orders"}, driver.Event{Time: time.Now(), Op: "notify", Channel: "orders", Payload: driver.String("paid")})
	r.wait(t)
	if r.code != 0 || !strings.Contains(r.stdout, `"payload":"paid"`) {
		t.Fatalf("code=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
	}
}

func TestWatchChannelFlagKeepsADottedTarget(t *testing.T) {
	h := newHarness(t)
	r := startWatch(t, h, context.Background(), "local-pg", "app.events", "--channel", "--max", "1")
	h.fake.Emit(driver.CollectionRef{Name: "app.events"}, driver.Event{Time: time.Now(), Op: "notify", Payload: driver.String("x")})
	r.wait(t)
	if r.code != 0 || !strings.Contains(r.stdout, `"op":"notify"`) {
		t.Fatalf("code=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
	}
}

func TestWatchTimeoutEndsCleanly(t *testing.T) {
	h := newHarness(t)
	start := time.Now()
	r := startWatch(t, h, context.Background(), "local-pg", "main.tobira_people", "--timeout", "300ms")
	h.fake.Emit(watchPeople, watchEvent(1, "insert"))
	r.wait(t)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	if r.code != 0 || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q, want 0 and one line", r.code, r.stdout, r.stderr)
	}
}

func TestWatchCancelExitsWithTheCancelCode(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := startWatch(t, h, ctx, "local-pg", "main.tobira_people")
	h.fake.Emit(watchPeople, watchEvent(1, "insert"))
	time.Sleep(50 * time.Millisecond)
	cancel() // Ctrl-C
	r.wait(t)
	if r.code != 3 || !strings.Contains(r.stderr, "canceled") {
		t.Fatalf("code=%d stderr=%q, want 3 and canceled", r.code, r.stderr)
	}
	if strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("stdout = %q, want the event received before the cancel", r.stdout)
	}
	if n := h.fake.Watchers(); n != 0 {
		t.Fatalf("%d watches left open", n)
	}
}

func TestWatchTableFormatEscapes(t *testing.T) {
	h := newHarness(t)
	r := startWatch(t, h, context.Background(), "local-pg", "main.tobira_people", "--format", "table", "--max", "1")
	ev := watchEvent(1, "insert")
	ev.Payload = driver.String("evil\x1b]0;pwned\x07\nnext")
	h.fake.Emit(watchPeople, ev)
	r.wait(t)
	if r.code != 0 {
		t.Fatalf("code=%d stderr=%q", r.code, r.stderr)
	}
	if strings.ContainsAny(r.stdout, "\x1b\x07") || !strings.Contains(r.stdout, "TIME") ||
		!strings.Contains(r.stdout, "insert") || strings.Count(r.stdout, "\n") != 2 {
		t.Fatalf("table output = %q", r.stdout)
	}
}

func TestWatchJSONEscapesAndRedacts(t *testing.T) {
	h := newHarness(t)
	h.cfg = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(h.cfg, []byte("connections:\n  - name: secret\n    driver: fake\n    dsn: fake://x\n    password: env:WATCH_PW\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WATCH_PW", "s3cretpassw0rd")
	r := startWatch(t, h, context.Background(), "secret", "main.tobira_people", "--max", "1")
	ev := watchEvent(1, "insert")
	ev.Payload = driver.String("pw is s3cretpassw0rd \x1b[31m")
	h.fake.Emit(watchPeople, ev)
	r.wait(t)
	if r.code != 0 || strings.Contains(r.stdout, "s3cretpassw0rd") || strings.Contains(r.stdout, "\x1b") {
		t.Fatalf("code=%d stdout=%q", r.code, r.stdout)
	}
}

// A change event whose payload has fields named like secrets (token, sig,
// secret) is data: the JSON line keeps them, while a password field is
// still masked.
func TestWatchJSONKeepsSecretNamedFields(t *testing.T) {
	h := newHarness(t)
	r := startWatch(t, h, context.Background(), "local-pg", "main.tobira_people", "--format", "jsonl", "--max", "1")
	ev := watchEvent(1, "update")
	ev.Payload = driver.ObjectValue(
		driver.Member{Key: "token", Value: driver.String("t-123")},
		driver.Member{Key: "sig", Value: driver.String("abc")},
		driver.Member{Key: "secret", Value: driver.String("shh")},
		driver.Member{Key: "password", Value: driver.String("Pw9Zq9x")},
	)
	h.fake.Emit(watchPeople, ev)
	r.wait(t)
	if r.code != 0 {
		t.Fatalf("code=%d stderr=%q", r.code, r.stderr)
	}
	for _, want := range []string{`"token":"t-123"`, `"sig":"abc"`, `"secret":"shh"`} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout = %q, want it to keep %s", r.stdout, want)
		}
	}
	if strings.Contains(r.stdout, "Pw9Zq9x") {
		t.Errorf("stdout = %q leaks the password field", r.stdout)
	}
}

func TestWatchDriverErrorIsRedacted(t *testing.T) {
	h := newHarness(t)
	r := startWatch(t, h, context.Background(), "local-pg", "main.tobira_people")
	h.fake.Emit(watchPeople, driver.Event{Err: driver.Wrap(driver.ErrConnection, "fake", "watch",
		errors.New("lost fake://user:hunter2xyz@host"))})
	r.wait(t)
	if r.code != 2 || strings.Contains(r.stderr, "hunter2xyz") || !strings.Contains(r.stderr, "lost") {
		t.Fatalf("code=%d stderr=%q, want 2 and a redacted error", r.code, r.stderr)
	}
}

func TestWatchRefusals(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no target", []string{"watch", "local-pg"}, 1, "accepts 2 arguments"},
		{"bad format", []string{"watch", "local-pg", "x", "--format", "csv"}, 1, "jsonl or table"},
		{"negative max", []string{"watch", "local-pg", "x", "--max", "-1"}, 1, "--max"},
		{"negative timeout", []string{"watch", "local-pg", "x", "--timeout", "-1s"}, 1, "--timeout"},
		{"unknown connection", []string{"watch", "nope", "x"}, 1, "unknown connection"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			_, errOut, code := h.exec(t, context.Background(), tc.args...)
			if code != tc.code || !strings.Contains(errOut, tc.want) {
				t.Fatalf("code=%d stderr=%q, want %d and %q", code, errOut, tc.code, tc.want)
			}
		})
	}
}

func TestWatchNeedsAWatcher(t *testing.T) {
	h := newHarness(t)
	h.fake = drivertest.NewFake(drivertest.WithPeople(), drivertest.WithoutCapabilities(driver.CapWatcher))
	_, errOut, code := h.exec(t, context.Background(), "watch", "local-pg", "main.tobira_people")
	if code != 3 || !strings.Contains(errOut, "watch support") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}
