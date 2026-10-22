package convert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/xavidop/tobira/driver"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

const (
	// RowsPerFrame is the most rows one rows frame carries.
	RowsPerFrame = 256
	// FrameBytesBudget is the encoded size SendResultSet keeps a rows frame
	// under when it can: it flushes a batch early rather than let it grow
	// past this. A single row larger than the budget still goes out alone,
	// up to MaxMessageBytes.
	FrameBytesBudget = 4 << 20
	// MaxStatsMessages caps Stats.Warnings and Stats.Notices, each; every
	// message is also capped at MaxNameBytes.
	MaxStatsMessages = 256
	// MaxCursorBytes caps Stats.NextCursor.
	MaxCursorBytes = 64 << 10
	// MaxRowBytes is the per-row budget: the largest encoded row a rows
	// frame can carry alone and still fit in one MaxMessageBytes message.
	// SendResultSet ends a result at a wider row with ErrLimit, after
	// sending the rows before it.
	MaxRowBytes = MaxMessageBytes - 1024
)

// CollectionRefToProto encodes ref.
func CollectionRefToProto(ref driver.CollectionRef) *pb.CollectionRef {
	return &pb.CollectionRef{Namespace: ref.Namespace, Name: ref.Name}
}

// CollectionRefFromProto decodes p; nil decodes as the zero ref. Both names
// are capped at MaxNameBytes.
func CollectionRefFromProto(p *pb.CollectionRef) (driver.CollectionRef, error) {
	ref := driver.CollectionRef{Namespace: p.GetNamespace(), Name: p.GetName()}
	if err := checkRef(ref); err != nil {
		return driver.CollectionRef{}, err
	}
	return ref, nil
}

func checkRef(ref driver.CollectionRef) error {
	if err := checkName("namespace name", ref.Namespace); err != nil {
		return err
	}
	return checkName("collection name", ref.Name)
}

func checkColumn(i int, c driver.Column) error {
	if err := checkName(fmt.Sprintf("column %d name", i), c.Name); err != nil {
		return err
	}
	return checkName(fmt.Sprintf("column %d native type", i), c.NativeType)
}

// ColumnsToProto encodes a result's columns, under MaxColumns and
// MaxNameBytes.
func ColumnsToProto(cols []driver.Column) (*pb.Columns, error) {
	if len(cols) > MaxColumns {
		return nil, limitf("%d columns, over %d", len(cols), MaxColumns)
	}
	out := make([]*pb.Column, len(cols))
	for i, c := range cols {
		if err := checkColumn(i, c); err != nil {
			return nil, err
		}
		pc := &pb.Column{Name: c.Name, NativeType: c.NativeType, Type: pb.ValueType(c.Type), Key: c.Key}
		if c.Source != nil {
			if err := checkRef(*c.Source); err != nil {
				return nil, fmt.Errorf("column %d source: %w", i, err)
			}
			pc.Source = CollectionRefToProto(*c.Source)
		}
		out[i] = pc
	}
	return &pb.Columns{Columns: out}, nil
}

// ColumnsFromProto decodes a result's columns. More than MaxColumns, a name
// over MaxNameBytes, a nil column or a type driver.Type does not have is an
// error.
func ColumnsFromProto(p *pb.Columns) ([]driver.Column, error) {
	pcs := p.GetColumns()
	if len(pcs) > MaxColumns {
		return nil, limitf("%d columns, over %d", len(pcs), MaxColumns)
	}
	out := make([]driver.Column, len(pcs))
	for i, pc := range pcs {
		if pc == nil {
			return nil, invalidf("column %d missing", i)
		}
		t := pc.GetType()
		if t < pb.ValueType(driver.TypeNull) || t > pb.ValueType(driver.TypeUnknown) {
			return nil, invalidf("column %d has unknown type %d", i, int32(t))
		}
		c := driver.Column{Name: pc.GetName(), NativeType: pc.GetNativeType(), Type: driver.Type(t), Key: pc.GetKey()}
		if err := checkColumn(i, c); err != nil {
			return nil, err
		}
		if pc.GetSource() != nil {
			ref, err := CollectionRefFromProto(pc.GetSource())
			if err != nil {
				return nil, fmt.Errorf("column %d source: %w", i, err)
			}
			c.Source = &ref
		}
		out[i] = c
	}
	return out, nil
}

// RowToProto encodes one row, each value under the limits.
func RowToProto(row driver.Row) (*pb.Row, error) {
	vs, err := ValuesToProto(row)
	if err != nil {
		return nil, err
	}
	return &pb.Row{Values: vs}, nil
}

// RowFromProto decodes one row, which must have exactly ncols values.
func RowFromProto(p *pb.Row, ncols int) (driver.Row, error) {
	if p == nil {
		return nil, invalidf("missing row")
	}
	if n := len(p.GetValues()); n != ncols {
		return nil, invalidf("row has %d values for %d columns", n, ncols)
	}
	vs, err := ValuesFromProto(p.GetValues())
	if err != nil {
		return nil, err
	}
	return driver.Row(vs), nil
}

// checkStats enforces MaxStatsMessages, MaxNameBytes and MaxCursorBytes.
func checkStats(cursor string, warnings, notices []string) error {
	if len(cursor) > MaxCursorBytes {
		return limitf("next cursor is %d bytes, over %d", len(cursor), MaxCursorBytes)
	}
	for _, l := range []struct {
		what string
		msgs []string
	}{{"warning", warnings}, {"notice", notices}} {
		if len(l.msgs) > MaxStatsMessages {
			return limitf("%d %ss, over %d", len(l.msgs), l.what, MaxStatsMessages)
		}
		for i, m := range l.msgs {
			if err := checkName(fmt.Sprintf("%s %d", l.what, i), m); err != nil {
				return err
			}
		}
	}
	return nil
}

// StatsToProto encodes st, under MaxStatsMessages warnings and notices of at
// most MaxNameBytes each and a NextCursor of at most MaxCursorBytes.
func StatsToProto(st driver.Stats) (*pb.Stats, error) {
	if err := checkStats(st.NextCursor, st.Warnings, st.Notices); err != nil {
		return nil, err
	}
	return &pb.Stats{
		RowsAffected: st.RowsAffected,
		Duration:     durationpb.New(st.Duration),
		Truncated:    st.Truncated,
		NextCursor:   st.NextCursor,
		Warnings:     st.Warnings,
		Notices:      st.Notices,
	}, nil
}

// StatsFromProto decodes p under the same limits as StatsToProto; nil
// decodes as zero Stats. An out-of-range duration saturates, as durationpb
// does.
func StatsFromProto(p *pb.Stats) (driver.Stats, error) {
	if err := checkStats(p.GetNextCursor(), p.GetWarnings(), p.GetNotices()); err != nil {
		return driver.Stats{}, err
	}
	st := driver.Stats{
		RowsAffected: p.GetRowsAffected(),
		Truncated:    p.GetTruncated(),
		NextCursor:   p.GetNextCursor(),
		Warnings:     p.GetWarnings(),
		Notices:      p.GetNotices(),
	}
	if d := p.GetDuration(); d != nil {
		st.Duration = d.AsDuration()
	}
	return st, nil
}

// ColumnsFrame is the frame that opens a result set.
func ColumnsFrame(cols []driver.Column) (*pb.ResultFrame, error) {
	pc, err := ColumnsToProto(cols)
	if err != nil {
		return nil, err
	}
	return &pb.ResultFrame{Frame: &pb.ResultFrame_Columns{Columns: pc}}, nil
}

// RowsFrame is a frame of at most RowsPerFrame rows. It does not check the
// rows' width; SendResultSet does.
func RowsFrame(rows []driver.Row) (*pb.ResultFrame, error) {
	if len(rows) > RowsPerFrame {
		return nil, limitf("%d rows in one frame, over %d", len(rows), RowsPerFrame)
	}
	batch := &pb.RowBatch{Rows: make([]*pb.Row, len(rows))}
	for i, r := range rows {
		pr, err := RowToProto(r)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		batch.Rows[i] = pr
	}
	return &pb.ResultFrame{Frame: &pb.ResultFrame_Rows{Rows: batch}}, nil
}

// StatsFrame is the frame that closes a result set.
func StatsFrame(st driver.Stats) (*pb.ResultFrame, error) {
	ps, err := StatsToProto(st)
	if err != nil {
		return nil, err
	}
	return &pb.ResultFrame{Frame: &pb.ResultFrame_Stats{Stats: ps}}, nil
}

// SendResultSet drains rs into send as frames: for each result set a columns
// frame, rows frames of at most RowsPerFrame rows (flushed early to stay
// under FrameBytesBudget), and a stats frame. A driver.MultiResultSet sends
// every set in turn. It always closes rs, and returns the first error from
// rs, from encoding (ErrLimit, for a row over MaxRowBytes too, once the
// rows before it are sent; or ErrInvalid for a row of the wrong width), or
// from send.
func SendResultSet(ctx context.Context, rs driver.ResultSet, send func(*pb.ResultFrame) error) (err error) {
	defer func() {
		if cerr := rs.Close(); err == nil {
			err = cerr
		}
	}()
	for {
		if err := sendOne(ctx, rs, send); err != nil {
			return err
		}
		m, ok := rs.(driver.MultiResultSet)
		if !ok {
			return nil
		}
		more, err := m.NextResultSet(ctx)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// sendOne sends the current result set of rs.
func sendOne(ctx context.Context, rs driver.ResultSet, send func(*pb.ResultFrame) error) error {
	cols := rs.Columns()
	cf, err := ColumnsFrame(cols)
	if err != nil {
		return err
	}
	if err := send(cf); err != nil {
		return err
	}
	batch := &pb.RowBatch{}
	size := 0
	flush := func() error {
		if len(batch.Rows) == 0 {
			return nil
		}
		f := &pb.ResultFrame{Frame: &pb.ResultFrame_Rows{Rows: batch}}
		batch, size = &pb.RowBatch{}, 0
		return send(f)
	}
	for n := 0; ; n++ {
		row, ok, err := rs.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if len(row) != len(cols) {
			return invalidf("row %d has %d values for %d columns", n, len(row), len(cols))
		}
		pr, err := RowToProto(row)
		if err != nil {
			return fmt.Errorf("row %d: %w", n, err)
		}
		rowSize := proto.Size(pr)
		if rowSize > MaxRowBytes {
			// The rows before it still reach the host; the result ends
			// here with ErrLimit (driver.ErrLimit on the host).
			if err := flush(); err != nil {
				return err
			}
			return limitf("row %d is %d bytes, over the %d message cap", n, rowSize, MaxMessageBytes)
		}
		if len(batch.Rows) > 0 && size+rowSize > FrameBytesBudget {
			if err := flush(); err != nil {
				return err
			}
		}
		batch.Rows = append(batch.Rows, pr)
		size += rowSize
		if len(batch.Rows) == RowsPerFrame {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	sf, err := StatsFrame(rs.Stats())
	if err != nil {
		return err
	}
	return send(sf)
}

// frameResultSet reads a ResultFrame stream back as a driver.MultiResultSet.
// It is for a single consumer, like every ResultSet; only release may run on
// another goroutine (from context.AfterFunc), hence the sync.Once.
type frameResultSet struct {
	recv        func() (*pb.ResultFrame, error)
	releaseOnce sync.Once
	release     func()

	cols    []driver.Column
	pending []*pb.Row // the undelivered rows of the current batch
	stats   driver.Stats
	ended   bool // the current set's stats frame arrived
	eof     bool // the stream ended cleanly
	closed  bool
	err     error // sticky failure
}

// NewFrameResultSet reads a ResultFrame stream. recv returns the next frame,
// or io.EOF at the end of the stream; its other errors are returned as they
// are, so a caller can map transport errors before they reach here.
//
// release, if not nil, must end the stream so that a blocked recv returns:
// for a gRPC client stream, pass the cancel func of the stream's context. It
// is called exactly once, on the first of: Close; NextResultSet reaching the
// clean end of the stream; any failure; or the context of the call in
// progress (ctx here, or the ctx given to Next or NextResultSet) ending while
// recv is blocked. In that last case the call returns ctx.Err(). A consumer
// that reads one set to its stats and never calls NextResultSet releases the
// stream only on Close, so always Close.
//
// It reads the first frame before returning, which must open a result set.
// Every later frame is validated: the limits, rows of the right width, 1 to
// RowsPerFrame rows a frame, and frames in order. A stream that ends before a
// set's stats frame fails with io.ErrUnexpectedEOF.
func NewFrameResultSet(ctx context.Context, recv func() (*pb.ResultFrame, error), release func()) (driver.MultiResultSet, error) {
	r := &frameResultSet{recv: recv, release: release}
	f, err := r.read(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = invalidf("empty result stream")
		}
		return nil, r.fail(err)
	}
	c, ok := f.GetFrame().(*pb.ResultFrame_Columns)
	if !ok {
		return nil, r.fail(invalidf("result stream must open with columns, got %T", f.GetFrame()))
	}
	if err := r.open(c.Columns); err != nil {
		return nil, r.fail(err)
	}
	return r, nil
}

// read returns the next non-nil frame. If ctx ends while recv is blocked,
// the stream is released and read returns ctx.Err().
func (r *frameResultSet) read(ctx context.Context) (*pb.ResultFrame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, r.doRelease)
	f, err := r.recv()
	if !stop() {
		// ctx ended during recv and the stream was released: whatever recv
		// returned, the stream is gone.
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if f == nil || f.GetFrame() == nil {
		return nil, invalidf("empty result frame")
	}
	return f, nil
}

func (r *frameResultSet) open(pc *pb.Columns) error {
	cols, err := ColumnsFromProto(pc)
	if err != nil {
		return err
	}
	r.cols, r.pending, r.stats, r.ended = cols, nil, driver.Stats{}, false
	return nil
}

func (r *frameResultSet) doRelease() {
	r.releaseOnce.Do(func() {
		if r.release != nil {
			r.release()
		}
	})
}

// fail records a sticky error and releases the stream.
func (r *frameResultSet) fail(err error) error {
	r.err = err
	r.doRelease()
	return err
}

func (r *frameResultSet) Columns() []driver.Column { return r.cols }

func (r *frameResultSet) Stats() driver.Stats { return r.stats }

func (r *frameResultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	if r.closed {
		return nil, false, nil
	}
	if r.err != nil {
		return nil, false, r.err
	}
	if r.ended {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	for len(r.pending) == 0 {
		if err := r.advance(ctx); err != nil {
			return nil, false, r.fail(err)
		}
		if r.ended {
			return nil, false, nil
		}
	}
	row, err := RowFromProto(r.pending[0], len(r.cols))
	if err != nil {
		return nil, false, r.fail(err)
	}
	r.pending = r.pending[1:]
	return row, true, nil
}

// advance reads the next frame of the current set: a rows frame fills
// pending, a stats frame ends the set. Each call reads exactly one frame and
// rejects an empty rows frame, so a loop over advance always progresses.
func (r *frameResultSet) advance(ctx context.Context) error {
	f, err := r.read(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("convert: result stream ended before its stats: %w", io.ErrUnexpectedEOF)
		}
		return err
	}
	switch k := f.GetFrame().(type) {
	case *pb.ResultFrame_Rows:
		rows := k.Rows.GetRows()
		if len(rows) == 0 {
			return invalidf("empty rows frame")
		}
		if len(rows) > RowsPerFrame {
			return limitf("%d rows in one frame, over %d", len(rows), RowsPerFrame)
		}
		r.pending = rows
	case *pb.ResultFrame_Stats:
		st, err := StatsFromProto(k.Stats)
		if err != nil {
			return err
		}
		r.stats, r.ended, r.pending = st, true, nil
	default:
		return invalidf("unexpected %T frame inside a result set", k)
	}
	return nil
}

func (r *frameResultSet) NextResultSet(ctx context.Context) (bool, error) {
	if r.closed {
		return false, nil
	}
	if r.err != nil {
		return false, r.err
	}
	if r.eof {
		return false, nil
	}
	for !r.ended {
		r.pending = nil
		if err := r.advance(ctx); err != nil {
			return false, r.fail(err)
		}
	}
	f, err := r.read(ctx)
	if errors.Is(err, io.EOF) {
		r.eof = true
		r.doRelease()
		return false, nil
	}
	if err != nil {
		return false, r.fail(err)
	}
	c, ok := f.GetFrame().(*pb.ResultFrame_Columns)
	if !ok {
		return false, r.fail(invalidf("unexpected %T frame after a result set's stats", f.GetFrame()))
	}
	if err := r.open(c.Columns); err != nil {
		return false, r.fail(err)
	}
	return true, nil
}

func (r *frameResultSet) Close() error {
	if !r.closed {
		r.closed = true
		r.pending = nil
		r.doRelease()
	}
	return nil
}
