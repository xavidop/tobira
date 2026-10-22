package tui

import (
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestNiceWindow(t *testing.T) {
	for _, c := range []struct{ in, want time.Duration }{
		{time.Millisecond, time.Second}, {57 * time.Second, time.Minute}, {61 * time.Second, 2 * time.Minute},
		{7 * time.Minute, 10 * time.Minute}, {25 * time.Hour, 48 * time.Hour},
	} {
		if got := niceWindow(c.in); got != c.want {
			t.Errorf("niceWindow(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPointsOfReadsTimeAndValue(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cols := []driver.Column{{Name: "_time", Type: driver.TypeTime}, {Name: "host"}, {Name: "value", Type: driver.TypeFloat}}
	rows := []driver.Row{
		{driver.Time(at), driver.String("a"), driver.Float(1.5)},
		{driver.Time(at.Add(time.Minute)), driver.String("a"), driver.Int(2)},
		{driver.Time(at.Add(2 * time.Minute)), driver.String("a"), driver.Decimal("2.25")},
		{driver.Time(at.Add(3 * time.Minute)), driver.String("a"), driver.Null()}, // skipped
		{driver.Null(), driver.String("a"), driver.Float(9)},                      // skipped
	}
	p := pointsOf(cols, rows)
	if len(p.vals) != 3 || p.vals[0] != 1.5 || p.vals[1] != 2 || p.vals[2] != 2.25 || !p.times[2].Equal(at.Add(2*time.Minute)) {
		t.Fatalf("points = %+v", p)
	}
	// Other names: the first time column and the last numeric one.
	p = pointsOf([]driver.Column{{Name: "ts", Type: driver.TypeTime}, {Name: "n", Type: driver.TypeInt}},
		[]driver.Row{{driver.Time(at), driver.Int(4)}})
	if len(p.vals) != 1 || p.vals[0] != 4 {
		t.Fatalf("points = %+v", p)
	}
}
