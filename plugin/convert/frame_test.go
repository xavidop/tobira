package convert_test

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

func testColumns() []driver.Column {
	return []driver.Column{
		{Name: "id", NativeType: "integer", Type: driver.TypeInt, Source: &driver.CollectionRef{Namespace: "main", Name: "t"}, Key: true},
		{Name: "name", NativeType: "text", Type: driver.TypeString},
		{Name: "", NativeType: "", Type: driver.TypeUnknown},
	}
}

func testRows(n int) []driver.Row {
	rows := make([]driver.Row, n)
	for i := range rows {
		rows[i] = driver.Row{driver.Int(int64(i)), driver.String("row " + strconv.Itoa(i)), driver.Null()}
	}
	return rows
}

func TestColumnsRoundTrip(t *testing.T) {
	var cols []driver.Column
	for typ := driver.TypeNull; typ <= driver.TypeUnknown; typ++ {
		cols = append(cols, driver.Column{Name: typ.String(), NativeType: "n" + typ.String(), Type: typ})
	}
	cols = append(cols, testColumns()...)
	p, err := convert.ColumnsToProto(cols)
	if err != nil {
		t.Fatal(err)
	}
	got, err := convert.ColumnsFromProto(wire(t, p, &pb.Columns{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(cols) {
		t.Fatalf("got %d columns, want %d", len(got), len(cols))
	}
	for i := range cols {
		w, g := cols[i], got[i]
		if w.Name != g.Name || w.NativeType != g.NativeType || w.Type != g.Type || w.Key != g.Key {
			t.Errorf("column %d: got %+v, want %+v", i, g, w)
		}
		if (w.Source == nil) != (g.Source == nil) || (w.Source != nil && *w.Source != *g.Source) {
			t.Errorf("column %d source: got %v, want %v", i, g.Source, w.Source)
		}
	}
}

func TestColumnsLimits(t *testing.T) {
	long := strings.Repeat("c", convert.MaxNameBytes+1)
	tooMany := make([]driver.Column, convert.MaxColumns+1)
	if _, err := convert.ColumnsToProto(tooMany); !errors.Is(err, convert.ErrLimit) {
		t.Errorf("ColumnsToProto(%d columns): err = %v, want ErrLimit", len(tooMany), err)
	}
	if _, err := convert.ColumnsToProto([]driver.Column{{Name: long}}); !errors.Is(err, convert.ErrLimit) {
		t.Errorf("ColumnsToProto(long name): err = %v, want ErrLimit", err)
	}
	if _, err := convert.ColumnsToProto(make([]driver.Column, convert.MaxColumns)); err != nil {
		t.Errorf("ColumnsToProto(%d columns): %v", convert.MaxColumns, err)
	}

	many := &pb.Columns{Columns: make([]*pb.Column, convert.MaxColumns+1)}
	for i := range many.Columns {
		many.Columns[i] = &pb.Column{}
	}
	cases := []struct {
		name string
		p    *pb.Columns
		want error
	}{
		{"too many", many, convert.ErrLimit},
		{"long name", &pb.Columns{Columns: []*pb.Column{{Name: long}}}, convert.ErrLimit},
		{"long native type", &pb.Columns{Columns: []*pb.Column{{NativeType: long}}}, convert.ErrLimit},
		{"long source name", &pb.Columns{Columns: []*pb.Column{{Source: &pb.CollectionRef{Name: long}}}}, convert.ErrLimit},
		{"long source namespace", &pb.Columns{Columns: []*pb.Column{{Source: &pb.CollectionRef{Namespace: long}}}}, convert.ErrLimit},
		{"unknown type", &pb.Columns{Columns: []*pb.Column{{Type: pb.ValueType(99)}}}, convert.ErrInvalid},
		{"negative type", &pb.Columns{Columns: []*pb.Column{{Type: pb.ValueType(-1)}}}, convert.ErrInvalid},
		{"nil column", &pb.Columns{Columns: []*pb.Column{nil}}, convert.ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := convert.ColumnsFromProto(tc.p); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestStatsRoundTrip(t *testing.T) {
	in := driver.Stats{
		RowsAffected: -1,
		Duration:     1500 * time.Millisecond,
		Truncated:    true,
		NextCursor:   "opaque\xff",
		Warnings:     []string{"w1", "w2"},
		Notices:      []string{"n"},
	}
	ps, err := convert.StatsToProto(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := convert.StatsFromProto(wire(t, ps, &pb.Stats{}))
	if err != nil {
		t.Fatal(err)
	}
	if got.RowsAffected != in.RowsAffected || got.Duration != in.Duration || got.Truncated != in.Truncated ||
		got.NextCursor != in.NextCursor || strings.Join(got.Warnings, "|") != "w1|w2" || strings.Join(got.Notices, "|") != "n" {
		t.Fatalf("stats changed: got %+v, want %+v", got, in)
	}
	if got, err := convert.StatsFromProto(nil); err != nil || got.Duration != 0 {
		t.Fatalf("StatsFromProto(nil) = %+v, %v", got, err)
	}
	if _, err := convert.StatsFromProto(&pb.Stats{Duration: nil}); err != nil {
		t.Fatalf("StatsFromProto(no duration): %v", err)
	}
}

func TestRowRoundTrip(t *testing.T) {
	row := testRows(1)[0]
	p, err := convert.RowToProto(row)
	if err != nil {
		t.Fatal(err)
	}
	got, err := convert.RowFromProto(wire(t, p, &pb.Row{}), len(row))
	if err != nil {
		t.Fatal(err)
	}
	for i := range row {
		if !sameValue(row[i], got[i]) {
			t.Errorf("cell %d: %v -> %v", i, row[i], got[i])
		}
	}
	if _, err := convert.RowFromProto(p, len(row)+1); !errors.Is(err, convert.ErrInvalid) {
		t.Errorf("width mismatch: err = %v, want ErrInvalid", err)
	}
	if _, err := convert.RowFromProto(nil, 0); !errors.Is(err, convert.ErrInvalid) {
		t.Errorf("nil row: err = %v, want ErrInvalid", err)
	}
}

func TestFrameConstructors(t *testing.T) {
	cf, err := convert.ColumnsFrame(testColumns())
	if err != nil || len(cf.GetColumns().GetColumns()) != 3 {
		t.Fatalf("ColumnsFrame = %v, %v", cf, err)
	}
	rf, err := convert.RowsFrame(testRows(convert.RowsPerFrame))
	if err != nil || len(rf.GetRows().GetRows()) != convert.RowsPerFrame {
		t.Fatalf("RowsFrame = %d rows, %v", len(rf.GetRows().GetRows()), err)
	}
	if _, err := convert.RowsFrame(testRows(convert.RowsPerFrame + 1)); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("RowsFrame(%d rows): err = %v, want ErrLimit", convert.RowsPerFrame+1, err)
	}
	sf, err := convert.StatsFrame(driver.Stats{RowsAffected: 3})
	if err != nil || sf.GetStats().GetRowsAffected() != 3 {
		t.Fatalf("StatsFrame = %v", sf)
	}
}

// capture is a send func that records frames, going over the wire.
type capture struct {
	t      *testing.T
	frames []*pb.ResultFrame
	failAt int // fail the n-th send (1-based); 0 = never
}

func (c *capture) send(f *pb.ResultFrame) error {
	if c.failAt > 0 && len(c.frames)+1 == c.failAt {
		return errors.New("send failed")
	}
	c.frames = append(c.frames, wire(c.t, f, &pb.ResultFrame{}))
	return nil
}

// recv returns a recv func that replays frames and then io.EOF.
func recv(frames []*pb.ResultFrame) func() (*pb.ResultFrame, error) {
	i := 0
	return func() (*pb.ResultFrame, error) {
		if i >= len(frames) {
			return nil, io.EOF
		}
		f := frames[i]
		i++
		return f, nil
	}
}

// closeTracker is a ResultSet wrapper that records Close.
type closeTracker struct {
	driver.ResultSet
	closed int
}

func (c *closeTracker) Close() error { c.closed++; return c.ResultSet.Close() }

func TestSendResultSetBatches(t *testing.T) {
	stats := driver.Stats{RowsAffected: -1, Duration: time.Second, Warnings: []string{"w"}}
	rs := &closeTracker{ResultSet: driver.NewSliceResultSet(testColumns(), testRows(600), stats)}
	c := &capture{t: t}
	if err := convert.SendResultSet(context.Background(), rs, c.send); err != nil {
		t.Fatal(err)
	}
	if rs.closed == 0 {
		t.Error("SendResultSet did not close the result set")
	}
	var shape []string
	for _, f := range c.frames {
		switch k := f.GetFrame().(type) {
		case *pb.ResultFrame_Columns:
			shape = append(shape, "columns")
		case *pb.ResultFrame_Rows:
			shape = append(shape, "rows:"+strconv.Itoa(len(k.Rows.GetRows())))
		case *pb.ResultFrame_Stats:
			shape = append(shape, "stats")
		}
	}
	if got, want := strings.Join(shape, " "), "columns rows:256 rows:256 rows:88 stats"; got != want {
		t.Fatalf("frames = %q, want %q", got, want)
	}

	got, err := convert.NewFrameResultSet(context.Background(), recv(c.frames), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, st, err := driver.Collect(context.Background(), got, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 600 {
		t.Fatalf("got %d rows, want 600", len(rows))
	}
	for i, r := range rows {
		for j := range r {
			if !sameValue(testRows(600)[i][j], r[j]) {
				t.Fatalf("row %d cell %d: %v", i, j, r[j])
			}
		}
	}
	if st.Duration != time.Second || st.RowsAffected != -1 || len(st.Warnings) != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if len(got.Columns()) != 3 || got.Columns()[0].Name != "id" {
		t.Fatalf("columns = %+v", got.Columns())
	}
}

func TestSendResultSetEmpty(t *testing.T) {
	c := &capture{t: t}
	rs := driver.NewSliceResultSet(testColumns(), nil, driver.Stats{RowsAffected: 4})
	if err := convert.SendResultSet(context.Background(), rs, c.send); err != nil {
		t.Fatal(err)
	}
	if len(c.frames) != 2 || c.frames[0].GetColumns() == nil || c.frames[1].GetStats() == nil {
		t.Fatalf("frames = %v", c.frames)
	}
	got, err := convert.NewFrameResultSet(context.Background(), recv(c.frames), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, st, err := driver.Collect(context.Background(), got, 0)
	if err != nil || len(rows) != 0 || st.RowsAffected != 4 {
		t.Fatalf("Collect = %d rows, %+v, %v", len(rows), st, err)
	}
}

func TestSendResultSetSplitsLargeBatches(t *testing.T) {
	// 256 rows of 128 KiB each would make a 32 MiB frame; the sender flushes
	// early to keep every frame under the byte budget.
	big := strings.Repeat("x", 128<<10)
	rows := make([]driver.Row, 256)
	for i := range rows {
		rows[i] = driver.Row{driver.String(big)}
	}
	c := &capture{t: t}
	rs := driver.NewSliceResultSet([]driver.Column{{Name: "v", Type: driver.TypeString}}, rows, driver.Stats{})
	if err := convert.SendResultSet(context.Background(), rs, c.send); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, f := range c.frames {
		if r := f.GetRows(); r != nil {
			total += len(r.GetRows())
			if n := protoSize(f); n > convert.FrameBytesBudget {
				t.Errorf("a rows frame is %d bytes, over the %d budget", n, convert.FrameBytesBudget)
			}
		}
	}
	if total != 256 {
		t.Fatalf("sent %d rows, want 256", total)
	}
}

func TestSendResultSetErrors(t *testing.T) {
	t.Run("send error", func(t *testing.T) {
		rs := &closeTracker{ResultSet: driver.NewSliceResultSet(testColumns(), testRows(3), driver.Stats{})}
		c := &capture{t: t, failAt: 2}
		if err := convert.SendResultSet(context.Background(), rs, c.send); err == nil || err.Error() != "send failed" {
			t.Fatalf("err = %v, want the send error", err)
		}
		if rs.closed == 0 {
			t.Error("result set not closed after a send error")
		}
	})
	t.Run("next error", func(t *testing.T) {
		boom := errors.New("boom")
		rs := &closeTracker{ResultSet: &failingRS{cols: testColumns(), err: boom}}
		c := &capture{t: t}
		if err := convert.SendResultSet(context.Background(), rs, c.send); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if rs.closed == 0 {
			t.Error("result set not closed after a Next error")
		}
	})
	t.Run("row width", func(t *testing.T) {
		rs := driver.NewSliceResultSet(testColumns(), []driver.Row{{driver.Int(1)}}, driver.Stats{})
		c := &capture{t: t}
		if err := convert.SendResultSet(context.Background(), rs, c.send); !errors.Is(err, convert.ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
	t.Run("too many columns", func(t *testing.T) {
		rs := driver.NewSliceResultSet(make([]driver.Column, convert.MaxColumns+1), nil, driver.Stats{})
		c := &capture{t: t}
		if err := convert.SendResultSet(context.Background(), rs, c.send); !errors.Is(err, convert.ErrLimit) {
			t.Fatalf("err = %v, want ErrLimit", err)
		}
	})
}

// failingRS fails its first Next.
type failingRS struct {
	cols []driver.Column
	err  error
}

func (f *failingRS) Columns() []driver.Column { return f.cols }
func (f *failingRS) Next(context.Context) (driver.Row, bool, error) {
	return nil, false, f.err
}
func (f *failingRS) Stats() driver.Stats { return driver.Stats{} }
func (f *failingRS) Close() error        { return nil }

// multiRS is a two-set MultiResultSet.
type multiRS struct {
	sets  []driver.ResultSet
	i     int
	close int
}

func (m *multiRS) Columns() []driver.Column { return m.sets[m.i].Columns() }
func (m *multiRS) Next(ctx context.Context) (driver.Row, bool, error) {
	return m.sets[m.i].Next(ctx)
}
func (m *multiRS) Stats() driver.Stats { return m.sets[m.i].Stats() }
func (m *multiRS) Close() error        { m.close++; return nil }
func (m *multiRS) NextResultSet(context.Context) (bool, error) {
	if m.i+1 >= len(m.sets) {
		return false, nil
	}
	m.i++
	return true, nil
}

func TestMultiResultSetRoundTrip(t *testing.T) {
	second := []driver.Column{{Name: "n", Type: driver.TypeInt}}
	src := &multiRS{sets: []driver.ResultSet{
		driver.NewSliceResultSet(testColumns(), testRows(2), driver.Stats{RowsAffected: -1}),
		driver.NewSliceResultSet(second, []driver.Row{{driver.Int(9)}}, driver.Stats{RowsAffected: 1}),
	}}
	c := &capture{t: t}
	if err := convert.SendResultSet(context.Background(), src, c.send); err != nil {
		t.Fatal(err)
	}
	if src.close != 1 {
		t.Errorf("closed %d times, want 1", src.close)
	}
	got, err := convert.NewFrameResultSet(context.Background(), recv(c.frames), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rows, st, err := collectNoClose(ctx, got)
	if err != nil || len(rows) != 2 || st.RowsAffected != -1 {
		t.Fatalf("first set: %d rows, %+v, %v", len(rows), st, err)
	}
	more, err := got.NextResultSet(ctx)
	if err != nil || !more {
		t.Fatalf("NextResultSet = %v, %v; want true", more, err)
	}
	if cols := got.Columns(); len(cols) != 1 || cols[0].Name != "n" {
		t.Fatalf("second set columns = %+v", cols)
	}
	rows, st, err = collectNoClose(ctx, got)
	if err != nil || len(rows) != 1 || st.RowsAffected != 1 {
		t.Fatalf("second set: %d rows, %+v, %v", len(rows), st, err)
	}
	if more, err := got.NextResultSet(ctx); err != nil || more {
		t.Fatalf("NextResultSet at the end = %v, %v; want false", more, err)
	}
}

func TestNextResultSetSkipsUnreadRows(t *testing.T) {
	second := []driver.Column{{Name: "n", Type: driver.TypeInt}}
	src := &multiRS{sets: []driver.ResultSet{
		driver.NewSliceResultSet(testColumns(), testRows(300), driver.Stats{}),
		driver.NewSliceResultSet(second, []driver.Row{{driver.Int(9)}}, driver.Stats{}),
	}}
	c := &capture{t: t}
	if err := convert.SendResultSet(context.Background(), src, c.send); err != nil {
		t.Fatal(err)
	}
	got, err := convert.NewFrameResultSet(context.Background(), recv(c.frames), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, ok, err := got.Next(ctx); !ok || err != nil {
		t.Fatalf("first Next = %v, %v", ok, err)
	}
	if more, err := got.NextResultSet(ctx); err != nil || !more {
		t.Fatalf("NextResultSet = %v, %v", more, err)
	}
	row, ok, err := got.Next(ctx)
	if err != nil || !ok {
		t.Fatalf("Next on second set = %v, %v", ok, err)
	}
	if n, _ := row[0].AsInt(); n != 9 {
		t.Fatalf("second set row = %v", row)
	}
}

func collectNoClose(ctx context.Context, rs driver.ResultSet) ([]driver.Row, driver.Stats, error) {
	var rows []driver.Row
	for {
		r, ok, err := rs.Next(ctx)
		if err != nil {
			return rows, rs.Stats(), err
		}
		if !ok {
			return rows, rs.Stats(), nil
		}
		rows = append(rows, r)
	}
}

func colsFrame(n int) *pb.ResultFrame {
	cols := make([]*pb.Column, n)
	for i := range cols {
		cols[i] = &pb.Column{Name: "c" + strconv.Itoa(i), Type: pb.ValueType_VALUE_TYPE_INT}
	}
	return &pb.ResultFrame{Frame: &pb.ResultFrame_Columns{Columns: &pb.Columns{Columns: cols}}}
}

func rowsFrame(rows ...[]*pb.Value) *pb.ResultFrame {
	b := &pb.RowBatch{}
	for _, r := range rows {
		b.Rows = append(b.Rows, &pb.Row{Values: r})
	}
	return &pb.ResultFrame{Frame: &pb.ResultFrame_Rows{Rows: b}}
}

func statsFrame() *pb.ResultFrame {
	return &pb.ResultFrame{Frame: &pb.ResultFrame_Stats{Stats: &pb.Stats{}}}
}

func intProto(n int64) *pb.Value { return &pb.Value{Kind: &pb.Value_IntValue{IntValue: n}} }

func TestNewFrameResultSetRejectsBadStreams(t *testing.T) {
	boom := errors.New("transport")
	tooMany := make([][]*pb.Value, convert.RowsPerFrame+1)
	for i := range tooMany {
		tooMany[i] = []*pb.Value{intProto(1)}
	}
	cases := []struct {
		name    string
		frames  []*pb.ResultFrame
		recvErr error
		open    error // error expected from NewFrameResultSet
		next    error // error expected from draining
	}{
		{name: "empty stream", open: convert.ErrInvalid},
		{name: "rows first", frames: []*pb.ResultFrame{rowsFrame()}, open: convert.ErrInvalid},
		{name: "stats first", frames: []*pb.ResultFrame{statsFrame()}, open: convert.ErrInvalid},
		{name: "nil frame", frames: []*pb.ResultFrame{nil}, open: convert.ErrInvalid},
		{name: "empty frame", frames: []*pb.ResultFrame{{}}, open: convert.ErrInvalid},
		{name: "transport error on open", recvErr: boom, open: boom},
		{name: "too many columns", frames: []*pb.ResultFrame{colsFrame(convert.MaxColumns + 1)}, open: convert.ErrLimit},
		{name: "no stats", frames: []*pb.ResultFrame{colsFrame(1), rowsFrame([]*pb.Value{intProto(1)})}, next: io.ErrUnexpectedEOF},
		{name: "row too narrow", frames: []*pb.ResultFrame{colsFrame(2), rowsFrame([]*pb.Value{intProto(1)}), statsFrame()}, next: convert.ErrInvalid},
		{name: "row too wide", frames: []*pb.ResultFrame{colsFrame(1), rowsFrame([]*pb.Value{intProto(1), intProto(2)}), statsFrame()}, next: convert.ErrInvalid},
		{name: "batch too big", frames: []*pb.ResultFrame{colsFrame(1), rowsFrame(tooMany...), statsFrame()}, next: convert.ErrLimit},
		{name: "columns twice", frames: []*pb.ResultFrame{colsFrame(1), colsFrame(1)}, next: convert.ErrInvalid},
		{name: "bad value", frames: []*pb.ResultFrame{colsFrame(1), rowsFrame([]*pb.Value{{}}), statsFrame()}, next: convert.ErrInvalid},
		{name: "empty rows frame", frames: []*pb.ResultFrame{colsFrame(1), rowsFrame(), statsFrame()}, next: convert.ErrInvalid},
		{name: "nil rows batch", frames: []*pb.ResultFrame{colsFrame(1), {Frame: &pb.ResultFrame_Rows{}}, statsFrame()}, next: convert.ErrInvalid},
		{name: "nil frame mid stream", frames: []*pb.ResultFrame{colsFrame(1), nil}, next: convert.ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := recv(tc.frames)
			if tc.recvErr != nil {
				r = func() (*pb.ResultFrame, error) { return nil, tc.recvErr }
			}
			rs, err := convert.NewFrameResultSet(context.Background(), r, nil)
			if tc.open != nil {
				if !errors.Is(err, tc.open) {
					t.Fatalf("NewFrameResultSet: err = %v, want %v", err, tc.open)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewFrameResultSet: %v", err)
			}
			_, _, err = driver.Collect(context.Background(), rs, 0)
			if !errors.Is(err, tc.next) {
				t.Fatalf("Collect: err = %v, want %v", err, tc.next)
			}
		})
	}
}

func TestFrameAfterStatsMustOpenANewSet(t *testing.T) {
	frames := []*pb.ResultFrame{colsFrame(1), statsFrame(), rowsFrame([]*pb.Value{intProto(1)})}
	rs, err := convert.NewFrameResultSet(context.Background(), recv(frames), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rs.Next(context.Background()); ok || err != nil {
		t.Fatalf("Next = %v, %v; want end of set", ok, err)
	}
	if _, err := rs.NextResultSet(context.Background()); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("NextResultSet: err = %v, want ErrInvalid", err)
	}
}

func TestFrameResultSetCloseAndCancel(t *testing.T) {
	frames := []*pb.ResultFrame{colsFrame(1), rowsFrame([]*pb.Value{intProto(1)}, []*pb.Value{intProto(2)}), statsFrame()}
	closed := 0
	rs, err := convert.NewFrameResultSet(context.Background(), recv(frames), func() { closed++ })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := rs.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next on a canceled context: err = %v", err)
	}
	if err := rs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rs.Close(); err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("close func called %d times, want 1", closed)
	}
	if _, ok, err := rs.Next(context.Background()); ok || err != nil {
		t.Fatalf("Next after Close = %v, %v; want end", ok, err)
	}
	if more, err := rs.NextResultSet(context.Background()); more || err != nil {
		t.Fatalf("NextResultSet after Close = %v, %v; want false", more, err)
	}
}

func TestFrameResultSetEndCallsClose(t *testing.T) {
	// Draining to EOF also releases the stream, so a caller that forgets
	// Close does not leak it.
	frames := []*pb.ResultFrame{colsFrame(1), statsFrame()}
	closed := 0
	rs, err := convert.NewFrameResultSet(context.Background(), recv(frames), func() { closed++ })
	if err != nil {
		t.Fatal(err)
	}
	if more, err := rs.NextResultSet(context.Background()); more || err != nil {
		t.Fatalf("NextResultSet = %v, %v", more, err)
	}
	_ = rs.Close()
	if closed != 1 {
		t.Fatalf("close func called %d times, want 1", closed)
	}
}

func TestFrameResultSetOpenErrorCallsClose(t *testing.T) {
	closed := 0
	if _, err := convert.NewFrameResultSet(context.Background(), recv(nil), func() { closed++ }); err == nil {
		t.Fatal("want an error for an empty stream")
	}
	if closed != 1 {
		t.Fatalf("close func called %d times on a failed open, want 1", closed)
	}
}

func TestStatsLimits(t *testing.T) {
	long := strings.Repeat("w", convert.MaxNameBytes+1)
	many := make([]string, convert.MaxStatsMessages+1)
	cases := []struct {
		name string
		st   driver.Stats
	}{
		{"too many warnings", driver.Stats{Warnings: many}},
		{"too many notices", driver.Stats{Notices: many}},
		{"long warning", driver.Stats{Warnings: []string{long}}},
		{"long notice", driver.Stats{Notices: []string{long}}},
		{"long cursor", driver.Stats{NextCursor: strings.Repeat("c", convert.MaxCursorBytes+1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := convert.StatsToProto(tc.st); !errors.Is(err, convert.ErrLimit) {
				t.Errorf("StatsToProto: err = %v, want ErrLimit", err)
			}
			p := &pb.Stats{Warnings: tc.st.Warnings, Notices: tc.st.Notices, NextCursor: tc.st.NextCursor}
			if _, err := convert.StatsFromProto(p); !errors.Is(err, convert.ErrLimit) {
				t.Errorf("StatsFromProto: err = %v, want ErrLimit", err)
			}
		})
	}
	atLimit := driver.Stats{
		Warnings:   make([]string, convert.MaxStatsMessages),
		Notices:    []string{strings.Repeat("n", convert.MaxNameBytes)},
		NextCursor: strings.Repeat("c", convert.MaxCursorBytes),
	}
	p, err := convert.StatsToProto(atLimit)
	if err != nil {
		t.Fatalf("StatsToProto at the limits: %v", err)
	}
	if _, err := convert.StatsFromProto(p); err != nil {
		t.Fatalf("StatsFromProto at the limits: %v", err)
	}
}

func TestSendResultSetRejectsOversizedStats(t *testing.T) {
	rs := driver.NewSliceResultSet(testColumns(), nil, driver.Stats{Warnings: make([]string, convert.MaxStatsMessages+1)})
	c := &capture{t: t}
	if err := convert.SendResultSet(context.Background(), rs, c.send); !errors.Is(err, convert.ErrLimit) {
		t.Fatalf("err = %v, want ErrLimit", err)
	}
}

// endlessEmpty is a recv that sends empty rows frames forever after the
// columns, as a hostile plugin might.
func endlessEmpty() (func() (*pb.ResultFrame, error), *int) {
	n := 0
	return func() (*pb.ResultFrame, error) {
		n++
		if n == 1 {
			return colsFrame(1), nil
		}
		if n > 10_000 {
			return nil, errors.New("recv called past any reasonable bound")
		}
		return rowsFrame(), nil
	}, &n
}

func TestEmptyRowsFramesCannotPinNext(t *testing.T) {
	r, n := endlessEmpty()
	rs, err := convert.NewFrameResultSet(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rs.Next(context.Background()); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("Next: err = %v, want ErrInvalid", err)
	}
	if *n != 2 {
		t.Fatalf("recv called %d times, want 2", *n)
	}
}

// blockingRecv returns a recv that sends the frames, then blocks until
// release is called, and that release.
func blockingRecv(frames []*pb.ResultFrame) (func() (*pb.ResultFrame, error), func(), *int) {
	unblock := make(chan struct{})
	released := 0
	i := 0
	recvFn := func() (*pb.ResultFrame, error) {
		if i < len(frames) {
			i++
			return frames[i-1], nil
		}
		<-unblock
		return nil, errors.New("stream canceled")
	}
	release := func() {
		released++
		close(unblock)
	}
	return recvFn, release, &released
}

func TestNextCancelsABlockedRecv(t *testing.T) {
	r, release, released := blockingRecv([]*pb.ResultFrame{colsFrame(1), rowsFrame([]*pb.Value{intProto(1)})})
	rs, err := convert.NewFrameResultSet(context.Background(), r, release)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if _, ok, err := rs.Next(ctx); !ok || err != nil {
		t.Fatalf("first Next = %v, %v", ok, err)
	}
	time.AfterFunc(20*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() {
		_, _, err := rs.Next(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Next: err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next stayed blocked after its context was canceled")
	}
	if err := rs.Close(); err != nil {
		t.Fatal(err)
	}
	if *released != 1 {
		t.Fatalf("release called %d times, want 1", *released)
	}
}

func TestNextResultSetCancelsABlockedRecv(t *testing.T) {
	r, release, _ := blockingRecv([]*pb.ResultFrame{colsFrame(1), statsFrame()})
	rs, err := convert.NewFrameResultSet(context.Background(), r, release)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := rs.NextResultSet(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NextResultSet: err = %v, want DeadlineExceeded", err)
	}
	// The failure is sticky.
	if _, _, err := rs.Next(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next after a failed NextResultSet: err = %v, want the sticky error", err)
	}
}

func TestNewFrameResultSetCancelsABlockedRecv(t *testing.T) {
	r, release, released := blockingRecv(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := convert.NewFrameResultSet(ctx, r, release); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if *released != 1 {
		t.Fatalf("release called %d times, want 1", *released)
	}
}

func TestStickyErrorBeatsEndOfSet(t *testing.T) {
	// After the set's stats, a bad frame fails NextResultSet; Next must then
	// report that failure, not a clean end.
	frames := []*pb.ResultFrame{colsFrame(1), statsFrame(), rowsFrame([]*pb.Value{intProto(1)})}
	rs, err := convert.NewFrameResultSet(context.Background(), recv(frames), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.NextResultSet(context.Background()); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("NextResultSet: err = %v", err)
	}
	if _, _, err := rs.Next(context.Background()); !errors.Is(err, convert.ErrInvalid) {
		t.Fatalf("Next: err = %v, want the sticky ErrInvalid", err)
	}
}

// A row over MaxRowBytes ends the result with ErrLimit (driver.ErrLimit on
// the wire), after the rows before it are sent, and every frame sent fits
// one message.
func TestSendResultSetWideRow(t *testing.T) {
	cols := make([]driver.Column, 20)
	narrow, wide := make(driver.Row, 20), make(driver.Row, 20)
	for i := range cols {
		cols[i] = driver.Column{Name: "c" + strconv.Itoa(i), Type: driver.TypeString}
		narrow[i], wide[i] = driver.String("a"), driver.String(strings.Repeat("x", 900<<10))
	}
	c := &capture{t: t}
	rs := driver.NewSliceResultSet(cols, []driver.Row{narrow, narrow, wide, narrow}, driver.Stats{})
	err := convert.SendResultSet(context.Background(), rs, c.send)
	if !errors.Is(err, convert.ErrLimit) || !strings.Contains(err.Error(), "row 2") {
		t.Fatalf("err = %v, want ErrLimit naming row 2", err)
	}
	sent := 0
	for _, f := range c.frames {
		sent += len(f.GetRows().GetRows())
		if n := protoSize(f); n > convert.MaxMessageBytes {
			t.Errorf("a frame is %d bytes, over the message cap", n)
		}
	}
	if sent != 2 {
		t.Errorf("sent %d rows before the wide one, want 2", sent)
	}
	if st := status.Convert(convert.ToStatus(err, nil)); convert.ErrorInfoOf(st).GetKind() != "limit" {
		t.Errorf("wire kind %q, want limit", convert.ErrorInfoOf(st).GetKind())
	}
}
