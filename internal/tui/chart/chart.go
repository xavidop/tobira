// Package chart draws a time series as text: a sparkline on a narrow pane,
// an ASCII line chart with min, max and last labels otherwise. It only
// renders; the caller fetches the points and styles the lines.
package chart

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/internal/query"
)

// SparkBelow is the width under which View draws a sparkline.
const SparkBelow = 40

var bars = []rune("▁▂▃▄▅▆▇█")

// finite reports a value that can be drawn: NaN and ±Inf (which a mean
// over a database's special values can return) are skipped everywhere.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// resample fits vals into at most w buckets, each the mean of its finite
// points; a bucket with none is NaN (drawn as a gap).
func resample(vals []float64, w int) []float64 {
	if len(vals) <= w {
		return vals
	}
	out := make([]float64, w)
	for i := range out {
		lo, hi := i*len(vals)/w, (i+1)*len(vals)/w
		sum, n := 0.0, 0
		for _, v := range vals[lo:hi] {
			if finite(v) {
				sum, n = sum+v, n+1
			}
		}
		out[i] = math.NaN()
		if n > 0 {
			out[i] = sum / float64(n)
		}
	}
	return out
}

// bounds is the range of the finite values; ok is false when there is none.
func bounds(vals []float64) (lo, hi float64, ok bool) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, v := range vals {
		if finite(v) {
			lo, hi, ok = min(lo, v), max(hi, v), true
		}
	}
	return lo, hi, ok
}

// lastFinite is the last finite value.
func lastFinite(vals []float64) float64 {
	for i := len(vals) - 1; i >= 0; i-- {
		if finite(vals[i]) {
			return vals[i]
		}
	}
	return math.NaN()
}

// level places v on a scale of n steps (0..n-1) between lo and hi; a flat
// series sits in the middle. A non-finite v is -1: not drawn.
func level(v, lo, hi float64, n int) int {
	if !finite(v) {
		return -1
	}
	if hi <= lo {
		return (n - 1) / 2
	}
	return min(max(int(math.Round((v-lo)/(hi-lo)*float64(n-1))), 0), n-1)
}

// Sparkline draws vals as at most w bars, ▁ for the lowest and █ for the
// highest.
func Sparkline(vals []float64, w int) string {
	if w < 1 || len(vals) == 0 {
		return ""
	}
	vals = resample(vals, w)
	lo, hi, _ := bounds(vals)
	var b strings.Builder
	for _, v := range vals {
		if l := level(v, lo, hi, len(bars)); l >= 0 {
			b.WriteRune(bars[l])
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// Num formats a value for a label: at most three decimals, no trailing
// zeros.
func Num(v float64) string {
	if math.Abs(v) < 1e15 {
		v = math.Round(v*1000) / 1000
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func stamp(t, ref time.Time) string {
	t, ref = t.UTC(), ref.UTC()
	if t.YearDay() == ref.YearDay() && t.Year() == ref.Year() {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// View draws the series (points in time order) into exactly h lines no
// wider than w: a sparkline and a min/max/last line under SparkBelow
// columns, otherwise a line chart with the max and min on the axis and a
// footer with the time span and the min, max and last values.
func View(times []time.Time, vals []float64, w, h int) []string {
	if h < 1 {
		return nil
	}
	lines := make([]string, h)
	if w < 1 {
		return lines
	}
	lo, hi, ok := bounds(vals)
	if !ok || len(times) != len(vals) {
		lines[0] = query.Truncate("no points in the range", w)
		return lines
	}
	last := lastFinite(vals)
	stats := "min " + Num(lo) + " · max " + Num(hi) + " · last " + Num(last)
	if w < SparkBelow || h < 3 {
		lines[0] = Sparkline(vals, w)
		if h > 1 {
			lines[1] = query.Truncate(stats, w)
		}
		return lines
	}
	plotH := h - 1
	hiS, loS := Num(hi), Num(lo)
	lw := max(len(hiS), len(loS))
	pw := w - lw - 1
	if pw < 2 {
		lines[0] = Sparkline(vals, w)
		return lines
	}
	pts := resample(vals, pw)
	grid := make([][]rune, plotH)
	for r := range grid {
		grid[r] = []rune(strings.Repeat(" ", len(pts)))
	}
	prev := -1
	for x, v := range pts {
		lv := level(v, lo, hi, plotH)
		if lv < 0 {
			prev = -1 // a gap: the line does not join across it
			continue
		}
		row := plotH - 1 - lv
		if prev >= 0 {
			for r := min(prev, row) + 1; r < max(prev, row); r++ {
				grid[r][x] = '│'
			}
		}
		grid[row][x] = '•'
		prev = row
	}
	for r := range plotH {
		label := ""
		switch r {
		case 0:
			label = hiS
		case plotH - 1:
			label = loS
		}
		lines[r] = strings.Repeat(" ", lw-len(label)) + label + "│" + string(grid[r])
	}
	span := stamp(times[0], times[0])
	if len(times) > 1 {
		span += " … " + stamp(times[len(times)-1], times[0])
	}
	lines[plotH] = query.Truncate(span+"   "+strings.ReplaceAll(stats, " · ", "  "), w)
	return lines
}
