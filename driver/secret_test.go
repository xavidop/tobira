package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestSecretNeverPrints(t *testing.T) {
	s := NewSecret("hunter2")

	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("m", "pw", s)
	var textBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&textBuf, nil)).Info("m", "pw", s)

	js, err := json.Marshal(struct{ P Secret }{s})
	if err != nil {
		t.Fatal(err)
	}
	ptr := &s

	outs := map[string]string{
		"fmt":      fmt.Sprintf("%v %+v %#v %s", s, s, s, s),
		"fmt-ptr":  fmt.Sprintf("%v %+v %#v %s", ptr, ptr, ptr, ptr),
		"fmt-d":    fmt.Sprintf("%d %x %q", s, s, s),
		"json":     string(js),
		"slog":     logBuf.String(),
		"slogtext": textBuf.String(),
		"println":  fmt.Sprintln(s),
	}
	for name, out := range outs {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(out, "hunter2") {
				t.Errorf("leaked secret: %s", out)
			}
			if !strings.Contains(out, "[redacted]") {
				t.Errorf("missing [redacted]: %s", out)
			}
		})
	}
}

func TestSecretReveal(t *testing.T) {
	tests := []struct {
		in   string
		zero bool
	}{
		{"hunter2", false},
		{"", true},
	}
	for _, tt := range tests {
		s := NewSecret(tt.in)
		if s.Reveal() != tt.in {
			t.Errorf("Reveal() = %q, want %q", s.Reveal(), tt.in)
		}
		if s.IsZero() != tt.zero {
			t.Errorf("IsZero(%q) = %v, want %v", tt.in, s.IsZero(), tt.zero)
		}
	}
	var z Secret
	if !z.IsZero() || z.Reveal() != "" {
		t.Error("zero Secret should be zero and reveal empty")
	}
}

type holder struct{ pw Secret }

func TestSecretInUnexportedField(t *testing.T) {
	h := holder{pw: NewSecret("hunter2")}
	ph := &h
	nested := struct{ H *holder }{ph}
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		for name, v := range map[string]any{"value": h, "pointer": ph, "nested": nested} {
			out := fmt.Sprintf(f, v)
			if strings.Contains(out, "hunter2") {
				t.Errorf("%s %s leaked: %s", f, name, out)
			}
		}
	}
}
