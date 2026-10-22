package tuitest

import "testing"

func TestCheck(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"plain", "hello 日本語\nworld", true},
		{"sgr only", "\x1b[1;38;5;12mhi\x1b[m\x1b[0m", true},
		{"osc", "a\x1b]0;x\x07", false},
		{"osc st", "a\x1b]0;x\x1b\\", false},
		{"dcs", "a\x1bPq", false},
		{"bel", "a\x07", false},
		{"c1 csi", "a\u009bb", false},
		{"c1 osc", "a\u009db", false},
		{"raw c1 byte", "a\x9db", false},
		{"cr", "a\rb", false},
		{"tab", "a\tb", false},
		{"del", "a\x7f", false},
		{"non-sgr csi", "\x1b[2J", false},
		{"truncated esc", "a\x1b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := check(tt.in); (err == nil) != tt.ok {
				t.Fatalf("check(%q) = %v, want ok=%v", tt.in, err, tt.ok)
			}
		})
	}
}
