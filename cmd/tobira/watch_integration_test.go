//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWatchPostgresSmoke runs `tobira watch` against a real PostgreSQL:
// a NOTIFY sent with `tobira query` arrives as one JSON line, and --max
// ends the watch with exit 0. It needs TOBIRA_PG_DSN (for example
// postgres://u:pw@localhost:5432/db?sslmode=disable) and is
// skipped without it. Output is redacted with the connection's settings,
// so a password that appears inside the channel name or payload is masked
// there too: use one that does not.
func TestWatchPostgresSmoke(t *testing.T) {
	dsn := os.Getenv("TOBIRA_PG_DSN")
	if dsn == "" {
		t.Skip("set TOBIRA_PG_DSN to run the PostgreSQL watch smoke test")
	}
	bin := buildBinary(t)
	env := isolatedEnv(t)
	cmd := exec.Command(bin, "watch", dsn, "watch_smoke_chan", "--max", "1", "--timeout", "30s")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// LISTEN is set up once the watch starts; notify until it reports.
	deadline := time.After(25 * time.Second)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("watch: %v\nstderr: %s", err, stderr.String())
			}
			var ev struct {
				Op, Channel, Payload string
			}
			line := strings.TrimSpace(stdout.String())
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("not one JSON line: %v\n%s", err, line)
			}
			if ev.Op != "notify" || ev.Channel != "watch_smoke_chan" || ev.Payload != "hello" {
				t.Fatalf("event = %+v", ev)
			}
			return
		case <-tick.C:
			if out, code := run(t, bin, env, "query", dsn, "select pg_notify('watch_smoke_chan', 'hello')"); code != 0 {
				t.Fatalf("notify: exit %d: %s", code, out)
			}
		case <-deadline:
			_ = cmd.Process.Kill()
			t.Fatalf("no event; stderr: %s", stderr.String())
		}
	}
}
