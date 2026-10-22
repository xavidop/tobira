package host

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	// maxStderrLine is the most bytes of one plugin stderr line logged.
	maxStderrLine = 4 << 10
	// stderrSlack is how far past the cap a line is still buffered, so a
	// secret that straddles the cap is whole when it is redacted.
	stderrSlack   = 1 << 10
	truncatedMark = "… (truncated)"
)

// stderrLog turns a plugin's stderr into debug records, one per line,
// redacted and capped. go-plugin writes every line to it from one
// goroutine; the mutex makes it safe regardless.
type stderrLog struct {
	log    *slog.Logger
	plugin string
	redact func(string) string
	max    int

	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func newStderrLog(log *slog.Logger, plugin string, redact func(string) string, max int) *stderrLog {
	return &stderrLog{log: log, plugin: plugin, redact: redact, max: max}
}

// Write implements io.Writer. It never fails. With debug logging off it
// drops p without buffering or redacting anything, so a plugin flooding
// stderr costs nothing.
func (w *stderrLog) Write(p []byte) (int, error) {
	n := len(p)
	if !w.log.Enabled(context.Background(), slog.LevelDebug) {
		return n, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if room := w.max + stderrSlack - len(w.buf); room > 0 {
			w.buf = append(w.buf, chunk[:min(room, len(chunk))]...)
		}
		if len(chunk) > 0 && len(w.buf) >= w.max+stderrSlack {
			w.truncated = true
		}
		if i < 0 {
			break
		}
		w.emit()
		p = p[i+1:]
	}
	return n, nil
}

// Flush logs a final line that had no newline.
func (w *stderrLog) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit()
	}
}

// emit logs the buffered line and resets the buffer. w.mu must be held.
func (w *stderrLog) emit() {
	line := strings.TrimSuffix(string(w.buf), "\r")
	truncated := w.truncated
	w.buf, w.truncated = w.buf[:0], false
	if w.redact != nil {
		line = w.redact(line)
	}
	if len(line) > w.max {
		line, truncated = cutUTF8(line, w.max), true
	}
	line = escapeControls(line)
	if truncated {
		line += truncatedMark
	}
	w.log.Debug("plugin stderr", "plugin", w.plugin, "line", line)
}

// cutUTF8 cuts s to at most n bytes without splitting a rune.
func cutUTF8(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// escapeControls makes control characters and invalid UTF-8 visible, so a
// plugin cannot smuggle terminal escapes into a log someone tails.
func escapeControls(s string) string {
	clean := true
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == utf8.RuneError:
			if _, size := utf8.DecodeRuneInString(s[i:]); size == 1 {
				fmt.Fprintf(&b, `\x%02x`, s[i])
				continue
			}
			b.WriteRune(r)
		case unicode.IsControl(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
