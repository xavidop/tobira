package driver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testRows() []Row {
	return []Row{{Int(1)}, {Int(2)}, {Int(3)}}
}

func TestSliceResultSet(t *testing.T) {
	ctx := context.Background()
	cols := []Column{{Name: "id", Type: TypeInt}}
	stats := Stats{RowsAffected: -1, Duration: time.Second, Warnings: []string{"w"}}
	rs := NewSliceResultSet(cols, testRows(), stats)

	if got := rs.Columns(); len(got) != 1 || got[0].Name != "id" {
		t.Fatalf("Columns() = %v", got)
	}
	for i := int64(1); i <= 3; i++ {
		row, ok, err := rs.Next(ctx)
		if err != nil || !ok {
			t.Fatalf("row %d: ok=%v err=%v", i, ok, err)
		}
		if v, _ := row[0].AsInt(); v != i {
			t.Fatalf("row %d = %v", i, row)
		}
	}
	row, ok, err := rs.Next(ctx)
	if row != nil || ok || err != nil {
		t.Fatalf("end: got (%v,%v,%v)", row, ok, err)
	}
	got := rs.Stats()
	if got.RowsAffected != -1 || got.Duration != time.Second || len(got.Warnings) != 1 {
		t.Fatalf("Stats() = %+v", got)
	}

	rs2 := NewSliceResultSet(cols, testRows(), stats)
	if err := rs2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rs2.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	row, ok, err = rs2.Next(ctx)
	if row != nil || ok || err != nil {
		t.Fatalf("after Close: got (%v,%v,%v)", row, ok, err)
	}
}

func TestSliceResultSetCopiesRows(t *testing.T) {
	rows := testRows()
	rs := NewSliceResultSet(nil, rows, Stats{})
	rows[0] = Row{Int(99)}
	row, _, _ := rs.Next(context.Background())
	if v, _ := row[0].AsInt(); v != 1 {
		t.Fatalf("caller mutation leaked: %v", row)
	}
}

func TestSliceResultSetCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rs := NewSliceResultSet(nil, testRows(), Stats{})
	row, ok, err := rs.Next(ctx)
	if !errors.Is(err, context.Canceled) || ok || row != nil {
		t.Fatalf("got (%v,%v,%v)", row, ok, err)
	}
}

// errResultSet yields n rows then fails.
type errResultSet struct {
	n      int
	closed int
	stats  Stats
}

func (e *errResultSet) Columns() []Column { return nil }
func (e *errResultSet) Next(context.Context) (Row, bool, error) {
	if e.n == 0 {
		return nil, false, errors.New("boom")
	}
	e.n--
	return Row{Int(1)}, true, nil
}
func (e *errResultSet) Stats() Stats { return e.stats }
func (e *errResultSet) Close() error { e.closed++; return nil }

// countingResultSet counts Next calls and Close calls.
type countingResultSet struct {
	ResultSet
	nexts, closes int
	stats         Stats
}

func (c *countingResultSet) Next(ctx context.Context) (Row, bool, error) {
	c.nexts++
	return c.ResultSet.Next(ctx)
}
func (c *countingResultSet) Close() error { c.closes++; return c.ResultSet.Close() }
func (c *countingResultSet) Stats() Stats { return c.stats }

func TestCollect(t *testing.T) {
	ctx := context.Background()
	t.Run("max truncates", func(t *testing.T) {
		c := &countingResultSet{ResultSet: NewSliceResultSet(nil, testRows(), Stats{})}
		rows, st, err := Collect(ctx, c, 2)
		if err != nil || len(rows) != 2 || !st.Truncated {
			t.Fatalf("rows=%d truncated=%v err=%v", len(rows), st.Truncated, err)
		}
		if c.closes != 1 {
			t.Fatalf("closes = %d", c.closes)
		}
		if c.nexts != 3 {
			t.Fatalf("Next calls = %d, want 3 (max + one extra)", c.nexts)
		}
	})
	t.Run("max equal to row count is not truncated", func(t *testing.T) {
		rows, st, err := Collect(ctx, NewSliceResultSet(nil, testRows(), Stats{}), 3)
		if err != nil || len(rows) != 3 || st.Truncated {
			t.Fatalf("rows=%d truncated=%v err=%v", len(rows), st.Truncated, err)
		}
	})
	t.Run("max zero returns all", func(t *testing.T) {
		c := &countingResultSet{ResultSet: NewSliceResultSet(nil, testRows(), Stats{})}
		rows, st, err := Collect(ctx, c, 0)
		if err != nil || len(rows) != 3 || st.Truncated || c.closes != 1 {
			t.Fatalf("rows=%d truncated=%v err=%v closes=%d", len(rows), st.Truncated, err, c.closes)
		}
	})
	t.Run("rs truncation preserved", func(t *testing.T) {
		rs := NewSliceResultSet(nil, testRows(), Stats{Truncated: true})
		_, st, err := Collect(ctx, rs, 0)
		if err != nil || !st.Truncated {
			t.Fatalf("truncated=%v err=%v", st.Truncated, err)
		}
	})
	t.Run("error mid-stream", func(t *testing.T) {
		e := &errResultSet{n: 2}
		rows, _, err := Collect(ctx, e, 0)
		if err == nil || err.Error() != "boom" || len(rows) != 2 {
			t.Fatalf("rows=%d err=%v", len(rows), err)
		}
		if e.closed != 1 {
			t.Fatalf("closed = %d", e.closed)
		}
	})
}
