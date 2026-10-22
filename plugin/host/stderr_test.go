package host

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// logLines decodes the JSON records a slog JSON handler wrote.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func TestStderrLogLinesAreRedactedAndTruncated(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := newStderrLog(log, "custom", func(s string) string { return strings.ReplaceAll(s, "hunter22", "xxxxx") }, 16)

	// go-plugin hands over a line in pieces, the newline last.
	for _, p := range []string{"connecting with hun", "ter22", "\n", "second\nthird line is far too long to keep", "\n", "pw hunter22\n"} {
		if n, err := w.Write([]byte(p)); err != nil || n != len(p) {
			t.Fatalf("Write(%q) = %d, %v", p, n, err)
		}
	}
	_, _ = w.Write([]byte("unterminated"))
	w.Flush()

	recs := logLines(t, &buf)
	var got []string
	for _, r := range recs {
		if r["level"] != "DEBUG" || r["plugin"] != "custom" || r["msg"] != "plugin stderr" {
			t.Errorf("record %v: want a DEBUG \"plugin stderr\" record for plugin custom", r)
		}
		got = append(got, r["line"].(string))
	}
	want := []string{
		"connecting with … (truncated)", // redacted after reassembly, then capped
		"second",
		"third line is fa… (truncated)",
		"pw xxxxx",
		"unterminated",
	}
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStderrLogEscapesControls(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := newStderrLog(log, "custom", nil, 1024)
	_, _ = w.Write([]byte("bell\x07 and \x1b[31mred\r\n"))
	recs := logLines(t, &buf)
	if len(recs) != 1 {
		t.Fatalf("records = %v", recs)
	}
	if line := recs[0]["line"].(string); strings.ContainsAny(line, "\x07\x1b\r") {
		t.Errorf("line %q keeps control characters", line)
	}
}

func TestStderrLogSkipsWorkWhenDebugIsOff(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	calls := 0
	w := newStderrLog(log, "custom", func(s string) string { calls++; return s }, 64)
	big := bytes.Repeat([]byte("line\n"), 10000)
	if n, err := w.Write(big); n != len(big) || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	w.Flush()
	if calls != 0 || buf.Len() != 0 {
		t.Errorf("with debug off: %d redactions, %d log bytes", calls, buf.Len())
	}
}

func TestStderrLogManyLinesInOneWrite(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := newStderrLog(log, "custom", nil, 64)
	_, _ = w.Write(bytes.Repeat([]byte("ab\n"), 3))
	if got := len(logLines(t, &buf)); got != 3 {
		t.Errorf("%d records, want 3", got)
	}
}
