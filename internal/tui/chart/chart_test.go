package chart

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func series(vals ...float64) ([]time.Time, []float64) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := make([]time.Time, len(vals))
	for i := range ts {
		ts[i] = base.Add(time.Duration(i) * time.Minute)
	}
	return ts, vals
}

func TestSparkline(t *testing.T) {
	if got := Sparkline([]float64{0, 1, 2, 3, 4, 5, 6, 7}, 20); got != "▁▂▃▄▅▆▇█" {
		t.Fatalf("Sparkline = %q", got)
	}
	if got := Sparkline([]float64{3, 3, 3}, 10); got != "▄▄▄" {
		t.Fatalf("flat = %q", got)
	}
	// More points than columns: buckets of means, exactly w wide.
	vals := make([]float64, 100)
	for i := range vals {
		vals[i] = float64(i)
	}
	got := Sparkline(vals, 10)
	if n := ansi.StringWidth(got); n != 10 || !strings.HasPrefix(got, "▁") || !strings.HasSuffix(got, "█") {
		t.Fatalf("resampled = %q (%d wide)", got, n)
	}
	if Sparkline(nil, 10) != "" || Sparkline(vals, 0) != "" {
		t.Fatal("empty input or width draws something")
	}
}

func checkBox(t *testing.T, lines []string, w, h int) {
	t.Helper()
	if len(lines) != h {
		t.Fatalf("%d lines, want %d", len(lines), h)
	}
	for i, l := range lines {
		if n := ansi.StringWidth(l); n > w {
			t.Fatalf("line %d is %d wide, max %d: %q", i, n, w, l)
		}
	}
}

func TestViewNarrowIsASparkline(t *testing.T) {
	ts, vals := series(1, 5, 2, 8)
	lines := View(ts, vals, 30, 6)
	checkBox(t, lines, 30, 6)
	if lines[0] != "▁▅▂█" || !strings.Contains(lines[1], "min 1") || !strings.Contains(lines[1], "max 8") || !strings.Contains(lines[1], "last 8") {
		t.Fatalf("narrow view:\n%s", strings.Join(lines, "\n"))
	}
}

func TestViewWideIsALineChartWithLabels(t *testing.T) {
	ts, vals := series(0.5, 10, 22.5, 3, 7.25)
	lines := View(ts, vals, 60, 7)
	checkBox(t, lines, 60, 7)
	all := strings.Join(lines, "\n")
	if !strings.HasPrefix(lines[0], "22.5") || !strings.HasPrefix(lines[5], " 0.5") {
		t.Fatalf("axis labels:\n%s", all)
	}
	for _, want := range []string{"min 0.5", "max 22.5", "last 7.25", "00:00", "00:04", "•"} {
		if !strings.Contains(all, want) {
			t.Fatalf("lacks %q:\n%s", want, all)
		}
	}
	// The highest point is on the top row, the lowest on the bottom one.
	if !strings.Contains(lines[0], "•") || !strings.Contains(lines[5], "•") {
		t.Fatalf("extremes not on the edges:\n%s", all)
	}
}

func TestViewEmptyAndTiny(t *testing.T) {
	lines := View(nil, nil, 50, 4)
	checkBox(t, lines, 50, 4)
	if !strings.Contains(lines[0], "no points") {
		t.Fatalf("empty:\n%s", strings.Join(lines, "\n"))
	}
	ts, vals := series(1, 2)
	for _, sz := range [][2]int{{1, 1}, {0, 0}, {45, 1}, {45, 2}, {3, 5}} {
		checkBox(t, View(ts, vals, sz[0], sz[1]), max(sz[0], 0), max(sz[1], 0))
	}
}

func TestNonFinitePointsAreSkipped(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		ts, vals := series(1, 5, bad, 8, 3)
		all := strings.Join(View(ts, vals, 60, 7), "\n")
		if strings.Contains(all, "NaN") || strings.Contains(all, "Inf") {
			t.Fatalf("%v leaks into the chart:\n%s", bad, all)
		}
		lines := View(ts, vals, 60, 7)
		if !strings.HasPrefix(lines[0], "8│") || !strings.HasPrefix(lines[5], "1│") || !strings.Contains(all, "last 3") {
			t.Fatalf("%v breaks the scale:\n%s", bad, all)
		}
		if got := Sparkline(vals, 20); got != "▁▅ █▃" {
			t.Fatalf("%v sparkline = %q", bad, got)
		}
	}
	ts, vals := series(math.NaN(), math.Inf(1))
	if l := View(ts, vals, 60, 4); !strings.Contains(l[0], "no points") {
		t.Fatalf("all non-finite:\n%s", strings.Join(l, "\n"))
	}
}
