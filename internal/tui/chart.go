package tui

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/async"
	"github.com/xavidop/tobira/internal/tui/chart"
	"github.com/xavidop/tobira/internal/tui/detail"
	"github.com/xavidop/tobira/internal/tui/palette"
)

const (
	// defaultRange is the chart's time range until :range: the last hour.
	defaultRange = time.Hour
	// chartH is the chart pane's height, its title line included.
	chartH = 9
	// maxChartPoints bounds the points one chart reads.
	maxChartPoints = 10000
	// chartAggregate is how points are combined in a window.
	chartAggregate = "mean"
)

// noChartText refuses :chart on a connection that cannot read time ranges.
const noChartText = "charts need a time-series driver"

// timeRange is the chart's range: the last span up to now, or fixed bounds
// (end exclusive).
type timeRange struct {
	last       time.Duration
	start, end time.Time
}

// bounds resolves the range at now.
func (r timeRange) bounds(now time.Time) (time.Time, time.Time) {
	if r.last > 0 {
		return now.Add(-r.last), now
	}
	return r.start, r.end
}

func (r timeRange) String() string {
	if r.last > 0 {
		return "last " + span(r.last)
	}
	return r.start.UTC().Format("2006-01-02 15:04") + ".." + r.end.UTC().Format("2006-01-02 15:04")
}

// span writes a duration like a TTL: 1h, 1m30s, 7d.
func span(d time.Duration) string {
	s, _ := detail.TTLText(driver.Int(int64(d / time.Second)))
	return s
}

// chartPoints is what a chart read collected.
type chartPoints struct {
	times     []time.Time
	vals      []float64
	truncated bool // maxChartPoints cut the read
}

// chartDoneMsg is the result of a chart read; its ID is checked against the
// chart's tracker, so a cancelled or superseded read is dropped.
type chartDoneMsg = async.Result[chartPoints]

// chartState is the chart pane under the grid (:chart).
type chartState struct {
	open    bool
	field   string
	window  time.Duration // as asked; 0 = chosen from the range and the width
	used    time.Duration // the window of the read shown
	conn    string
	ref     driver.CollectionRef
	rng     timeRange
	loading bool
	err     string // redacted
	pts     chartPoints
	runs    async.Tracker
	// done is closed when the read in flight has returned; a write that may
	// close the connection waits for it (stopChartOn), and paused marks a
	// read it cancelled, which resumes after the write (resumeChart).
	done   chan struct{}
	paused bool
}

func newChartState() chartState { return chartState{rng: timeRange{last: defaultRange}} }

// chartCmd opens (or, with off, closes) the chart of a field of the open
// collection.
func (a *Model) chartCmd(c palette.ChartCmd) tea.Cmd {
	if c.Off {
		if !a.chart.open {
			return a.flash("no chart is open", false)
		}
		a.closeChart()
		return nil
	}
	if !a.cur.set || a.src == nil {
		return a.flash("open a measurement to chart it", false)
	}
	if _, ok := driver.As[driver.TimeSeriesReader](a.src.Conn()); !ok {
		return a.flash(noChartText, false)
	}
	a.chart.runs.Cancel()
	a.chart.open, a.chart.field, a.chart.window = true, c.Field, c.Window
	a.chart.conn, a.chart.ref = a.cur.conn, a.cur.ref
	a.chart.pts, a.chart.err = chartPoints{}, ""
	return a.loadChart()
}

// rangeCmd sets the chart's time range and redraws an open chart.
func (a *Model) rangeCmd(c palette.RangeCmd) tea.Cmd {
	a.chart.rng = timeRange{last: c.Last, start: c.Start, end: c.End}
	flash := a.flash("chart range: "+a.chart.rng.String(), false)
	if !a.chart.open {
		return flash
	}
	return tea.Batch(flash, a.loadChart())
}

// loadChart reads the chart's points in a cancellable command: the range
// in windows of the asked size, or of about one per column.
func (a *Model) loadChart() tea.Cmd {
	start, end := a.chart.rng.bounds(a.clock())
	window := a.chartWindow(start, end)
	a.chart.used, a.chart.paused = window, false // the title names this read, even if it fails at once
	conn, ok := a.mgr.Conn(a.chart.conn)
	if !ok {
		a.chart.err = "connection " + a.chart.conn + " is not open"
		return nil
	}
	ts, ok := driver.As[driver.TimeSeriesReader](conn)
	if !ok {
		a.chart.err = noChartText
		return nil
	}
	a.chart.loading, a.chart.err = true, ""
	id, ctx := a.chart.runs.Start(context.Background())
	done := make(chan struct{})
	a.chart.done = done
	q := driver.RangeQuery{Field: a.chart.field, Start: start, End: end, Window: window, Aggregate: chartAggregate}
	ref := a.chart.ref
	return async.Do(id, ctx, func(ctx context.Context) (chartPoints, error) {
		defer close(done)
		rs, err := ts.Range(ctx, ref, q)
		if err != nil {
			return chartPoints{}, err
		}
		cols := rs.Columns()
		rows, st, err := driver.Collect(ctx, rs, maxChartPoints)
		if err != nil {
			return chartPoints{}, err
		}
		p := pointsOf(cols, rows)
		p.truncated = st.Truncated
		return p, nil
	})
}

// chartWindow is the window of a read of [start, end): the one asked, or a
// round step of about one point per column of the pane.
func (a *Model) chartWindow(start, end time.Time) time.Duration {
	if a.chart.window > 0 {
		return a.chart.window
	}
	return niceWindow(end.Sub(start) / time.Duration(max(a.lay.grid.w-8, 10)))
}

// syncChart reads the chart again when a resize changed its automatic
// window, so the points keep about one per column.
func (a *Model) syncChart() tea.Cmd {
	if !a.chart.open || a.chart.loading || a.chart.paused || a.chart.window > 0 || a.chart.err != "" {
		return nil
	}
	start, end := a.chart.rng.bounds(a.clock())
	if a.chartWindow(start, end) == a.chart.used {
		return nil
	}
	return a.loadChart()
}

// stopChartOn cancels a chart read on conn ("" for any) before a config
// write or reload may close it, hands its done channel to that write to
// wait for, and marks it paused so it resumes after.
func (a *Model) stopChartOn(conn string) {
	if !a.chart.loading || conn != "" && a.chart.conn != conn {
		return
	}
	a.chart.runs.Cancel()
	a.chart.loading, a.chart.paused = false, true
	if a.chart.done != nil {
		a.stopping = append(stillRunning(a.stopping), a.chart.done)
	}
}

// resumeChart restarts a read stopChartOn paused, once the write is done
// and the chart is still open (its connection survived).
func (a *Model) resumeChart() tea.Cmd {
	if !a.chart.paused {
		return nil
	}
	a.chart.paused = false
	if !a.chart.open {
		return nil
	}
	return a.loadChart()
}

// niceSteps are the windows an automatic chart window rounds up to.
var niceSteps = []time.Duration{
	time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second,
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// niceWindow rounds d up to a step a person would pick: 1m rather than 57s.
// Past a day it is whole days.
func niceWindow(d time.Duration) time.Duration {
	for _, s := range niceSteps {
		if d <= s {
			return s
		}
	}
	day := 24 * time.Hour
	return (d + day - 1) / day * day
}

// pointsOf reads the _time and value columns of a Range result (the first
// time column and the last numeric one when they are named otherwise).
// Rows without a time or a number are skipped.
func pointsOf(cols []driver.Column, rows []driver.Row) chartPoints {
	ti := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == "_time" })
	if ti < 0 {
		ti = slices.IndexFunc(cols, func(c driver.Column) bool { return c.Type == driver.TypeTime })
	}
	vi := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == "value" })
	for i := len(cols) - 1; vi < 0 && i >= 0; i-- {
		switch cols[i].Type {
		case driver.TypeInt, driver.TypeFloat, driver.TypeDecimal:
			vi = i
		}
	}
	var p chartPoints
	if ti < 0 || vi < 0 {
		return p
	}
	for _, r := range rows {
		if ti >= len(r) || vi >= len(r) {
			continue
		}
		t, ok := r[ti].AsTime()
		f, fok := r[vi].AsFloat() // Int too
		if d, dok := r[vi].AsText(); !fok && dok && r[vi].Type() == driver.TypeDecimal {
			x, err := strconv.ParseFloat(d, 64)
			f, fok = x, err == nil
		}
		if ok && fok && !math.IsNaN(f) && !math.IsInf(f, 0) {
			p.times, p.vals = append(p.times, t), append(p.vals, f)
		}
	}
	return p
}

// chartDone shows a finished chart read.
func (a *Model) chartDone(r chartDoneMsg) tea.Cmd {
	if !a.chart.runs.Finish(r.ID) {
		return nil
	}
	a.chart.loading = false
	if r.Err != nil {
		if errors.Is(r.Err, context.Canceled) {
			a.chart.err = "cancelled"
			return nil
		}
		a.chart.err = a.mgr.Redact(a.chart.conn, r.Err.Error())
		return a.flash(a.chart.err, true)
	}
	a.chart.pts = r.Val
	return nil
}

// closeChart closes the chart pane and abandons its read.
func (a *Model) closeChart() {
	a.chart.runs.Cancel()
	a.chart.open, a.chart.loading, a.chart.paused, a.chart.err, a.chart.pts = false, false, false, "", chartPoints{}
}

// cancelChart cancels a chart read in flight; it reports whether one ran.
func (a *Model) cancelChart() bool {
	if !a.chart.loading {
		return false
	}
	a.chart.runs.Cancel()
	a.chart.loading, a.chart.err = false, "cancelled"
	return true
}

// chartTitle is the chart pane's title line text.
func (a *Model) chartTitle() string {
	title := "chart · " + query.EscapeControls(a.chart.field) + " · " + chartAggregate + " " + span(a.chart.used) +
		" · " + a.chart.rng.String()
	if a.chart.pts.truncated {
		title += " (truncated)"
	}
	return title
}

// chartPane draws the chart pane into r: its title line, then the chart.
func (a *Model) chartPane(r rect) []string {
	st := a.th.Styles
	lines := []string{a.titleLine(a.chartTitle(), r.w, false)}
	h := r.h - 1
	var body []string
	switch {
	case a.chart.loading:
		body = []string{st.Dim.Render("loading…")}
	case a.chart.err != "":
		wrapped := ansi.Wrap(query.EscapeControls(a.chart.err), max(r.w, 1), "")
		for _, l := range strings.Split(wrapped, "\n") {
			body = append(body, st.Error.Render(l))
		}
	default:
		for _, l := range chart.View(a.chart.pts.times, a.chart.pts.vals, r.w, h) {
			body = append(body, st.Cell.Render(l))
		}
	}
	return append(lines, fit(strings.Join(body, "\n"), r.w, h)...)
}
