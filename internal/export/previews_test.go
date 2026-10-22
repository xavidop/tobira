package export_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/export"
	"github.com/xavidop/tobira/internal/query"
)

var notesRef = driver.CollectionRef{Namespace: "main", Name: "notes"}

// previewFake holds notes: id, value and a _truncated marker, true on two
// of four rows. generated says whether Describe lists the marker as
// Generated (a driver's preview marker) or not (a real column).
func previewFake(generated bool) *drivertest.Fake {
	f := drivertest.NewFake()
	cols := []driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "value", Type: driver.TypeString}, {Name: "_truncated", Type: driver.TypeBool}}
	f.AddCollection("main", "notes", cols, []driver.Row{
		{driver.Int(1), driver.String("whole"), driver.Bool(false)},
		{driver.Int(2), driver.String("cut…"), driver.Bool(true)},
		{driver.Int(3), driver.String("cut…"), driver.Bool(true)},
		{driver.Int(4), driver.String("whole"), driver.Bool(false)},
	}, "id")
	f.OnDescribe("main", "notes", func(s *driver.CollectionSchema) {
		for i := range s.Fields {
			if s.Fields[i].Name == "_truncated" {
				s.Fields[i].Generated = generated
			}
		}
	})
	f.OnQuery("q", func(driver.Query) (driver.ResultSet, error) {
		src := notesRef
		return driver.NewSliceResultSet([]driver.Column{{Name: "value", Type: driver.TypeString, Source: &src}, {Name: "_truncated", Type: driver.TypeBool, Source: &src}},
			[]driver.Row{{driver.String("cut…"), driver.Bool(true)}}, driver.Stats{RowsAffected: -1}), nil
	})
	return f
}

// An export counts the values that were previews (rows whose _truncated
// marker is true), in every format and across pages, but only when Describe
// lists _truncated as Generated.
func TestToFileCountsPreviews(t *testing.T) {
	for _, f := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		for _, gen := range []bool{true, false} {
			c := open(t, previewFake(gen))
			p := filepath.Join(t.TempDir(), "out."+string(f))
			res, err := export.ToFile(context.Background(), export.CollectionSource{Conn: c, Ref: notesRef, Opts: driver.BrowseOptions{Limit: 3}},
				export.Options{Format: f, Path: p})
			if err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if gen {
				want = 2
			}
			if res.Previews != want || res.Rows != 4 {
				t.Errorf("%s, generated=%v: result %+v, want %d previews of 4 rows", f, gen, res, want)
			}
		}
	}
	c := open(t, previewFake(true))
	// json: csv would take the Fake's native export, whose rows are never seen.
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: c, Query: driver.Query{Text: "q"}},
		export.Options{Format: query.FormatJSON, Path: filepath.Join(t.TempDir(), "q.json")})
	if err != nil || res.Previews != 1 {
		t.Errorf("query whose columns come from notes: %+v, %v, want 1 preview", res, err)
	}
	res, err = export.ToFile(context.Background(), export.QuerySource{Conn: open(t, drivertest.NewFake(drivertest.WithPeople())), Query: driver.Query{Text: peopleSQL}},
		export.Options{Format: query.FormatCSV, Path: filepath.Join(t.TempDir(), "p.csv")})
	if err != nil || res.Previews != 0 {
		t.Errorf("people: %+v, %v, want no previews", res, err)
	}
}

func TestPreviewWarning(t *testing.T) {
	for n, want := range map[int64]string{
		0: "",
		1: "1 value was a preview, not a full value",
		3: "3 values were previews, not full values",
	} {
		if got := export.PreviewWarning(n); got != want {
			t.Errorf("PreviewWarning(%d) = %q, want %q", n, got, want)
		}
	}
}

// openTracker wraps a Conn and fails the test when Describe runs while a
// result set from Query or Browse is still open: on a pool of one
// connection that Describe would wait for the page to close.
type openTracker struct {
	driver.Conn
	t         *testing.T
	open      atomic.Int32
	describes atomic.Int32
}

func (o *openTracker) track(rs driver.ResultSet, err error) (driver.ResultSet, error) {
	if err != nil {
		return nil, err
	}
	o.open.Add(1)
	return &trackedRS{ResultSet: rs, o: o}, nil
}

func (o *openTracker) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return o.track(o.Conn.Query(ctx, q))
}

func (o *openTracker) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	return o.track(o.Conn.(driver.Browser).Browse(ctx, ref, opts))
}

func (o *openTracker) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if n := o.open.Load(); n > 0 {
		o.t.Errorf("Describe(%v) ran with %d result sets open", ref, n)
	}
	o.describes.Add(1)
	return o.Conn.Describe(ctx, ref)
}

type trackedRS struct {
	driver.ResultSet
	o    *openTracker
	once sync.Once
}

func (r *trackedRS) Close() error {
	r.once.Do(func() { r.o.open.Add(-1) })
	return r.ResultSet.Close()
}

// The marker's Describe runs only once no page is open, and the marker's
// name matches case-insensitively, as in the TUI.
func TestPreviewDescribeAfterPageClosed(t *testing.T) {
	for _, f := range []query.Format{query.FormatCSV, query.FormatJSON} {
		o := &openTracker{Conn: open(t, previewFake(true)), t: t}
		res, err := export.ToFile(context.Background(), export.CollectionSource{Conn: o, Ref: notesRef, Opts: driver.BrowseOptions{Limit: 3}},
			export.Options{Format: f, Path: filepath.Join(t.TempDir(), "out."+string(f))})
		if err != nil || res.Previews != 2 || res.Rows != 4 {
			t.Errorf("%s collection: %+v, %v, want 2 previews of 4 rows", f, res, err)
		}
		if n := o.describes.Load(); n != 1 {
			t.Errorf("%s collection: %d Describe calls, want 1", f, n)
		}
	}
	fk := previewFake(true)
	fk.OnQuery("upper", func(driver.Query) (driver.ResultSet, error) {
		src := notesRef
		return driver.NewSliceResultSet([]driver.Column{{Name: "value", Type: driver.TypeString, Source: &src}, {Name: "_TRUNCATED", Type: driver.TypeBool, Source: &src}},
			[]driver.Row{{driver.String("cut…"), driver.Bool(true)}, {driver.String("cut…"), driver.Bool(true)}, {driver.String("whole"), driver.Bool(false)}},
			driver.Stats{RowsAffected: -1}), nil
	})
	o := &openTracker{Conn: open(t, fk), t: t}
	res, err := export.ToFile(context.Background(), export.QuerySource{Conn: o, Query: driver.Query{Text: "upper"}},
		export.Options{Format: query.FormatJSON, Path: filepath.Join(t.TempDir(), "q.json")})
	if err != nil || res.Previews != 2 {
		t.Errorf("query with _TRUNCATED: %+v, %v, want 2 previews", res, err)
	}
	if n := o.describes.Load(); n != 1 {
		t.Errorf("query: %d Describe calls, want 1", n)
	}
}
