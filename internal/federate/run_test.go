package federate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// fakeConn answers every query with the same rows, or with err, and keeps
// the queries it got. As the scratch engine it refuses a read-only query
// that is not a select, like the sqlite driver's guard.
type fakeConn struct {
	mu      sync.Mutex
	cols    []driver.Column
	rows    []driver.Row
	err     error
	scratch bool
	queries []driver.Query
}

func (c *fakeConn) Query(_ context.Context, q driver.Query) (driver.ResultSet, error) {
	c.mu.Lock()
	c.queries = append(c.queries, q)
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if c.scratch {
		if q.ReadOnly && !strings.HasPrefix(strings.ToLower(q.Text), "select") {
			return nil, driver.Wrap(driver.ErrReadOnly, "sqlite", "query", errors.New("write refused"))
		}
		if !q.ReadOnly {
			return driver.NewSliceResultSet(nil, nil, driver.Stats{RowsAffected: 1}), nil
		}
	}
	return driver.NewSliceResultSet(c.cols, c.rows, driver.Stats{RowsAffected: -1}), nil
}

func (c *fakeConn) Ping(context.Context) error {
	return nil
}

func (c *fakeConn) Close() error {
	return nil
}

func (c *fakeConn) Namespaces(context.Context) ([]driver.Namespace, error) {
	return nil, nil
}

func (c *fakeConn) Collections(context.Context, driver.NamespaceRef) ([]driver.Collection, error) {
	return nil, nil
}

func (c *fakeConn) Describe(context.Context, driver.CollectionRef) (*driver.CollectionSchema, error) {
	return nil, driver.ErrNotFound
}

// got returns the queries whose text starts with prefix, ignoring case.
func (c *fakeConn) got(prefix string) []driver.Query {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []driver.Query
	for _, q := range c.queries {
		if strings.HasPrefix(strings.ToLower(q.Text), strings.ToLower(prefix)) {
			out = append(out, q)
		}
	}
	return out
}

// fakeOpener opens the connections it holds and redacts "secret".
type fakeOpener map[string]driver.Conn

func (o fakeOpener) Open(_ context.Context, name string) (driver.Conn, error) {
	c, ok := o[name]
	if !ok {
		return nil, fmt.Errorf("unknown connection %q", name)
	}
	return c, nil
}

func (o fakeOpener) Redact(_, text string) string { return strings.ReplaceAll(text, "secret", "***") }

// fakeScratchDriver is registered as "sqlite" and opens scratch.
type fakeScratchDriver struct{ scratch *fakeConn }

func (d *fakeScratchDriver) Info() driver.Info { return driver.Info{Name: scratchDriver} }

func (d *fakeScratchDriver) Open(_ context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	if !strings.HasPrefix(cfg.DSN, "file:") {
		return nil, fmt.Errorf("unexpected scratch DSN %q", cfg.DSN)
	}
	return d.scratch, nil
}

// registryWith returns a registry whose sqlite driver opens a scratch
// connection that answers the final select with cols and rows.
func registryWith(t *testing.T, cols []driver.Column, rows []driver.Row) (*driver.Registry, *fakeConn) {
	t.Helper()
	sc := &fakeConn{cols: cols, rows: rows, scratch: true}
	reg := driver.NewRegistry()
	if err := reg.Add(&fakeScratchDriver{scratch: sc}); err != nil {
		t.Fatal(err)
	}
	return reg, sc
}

func mustParse(t *testing.T, text string) *Statement {
	t.Helper()
	st, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

var when = time.Date(2026, 11, 5, 12, 30, 0, 0, time.FixedZone("CET", 3600))

// newOrders is a SQL-like source with every value kind the scratch converts.
func newOrders() *fakeConn {
	return &fakeConn{
		cols: []driver.Column{
			{Name: "id", Type: driver.TypeInt, Source: &driver.CollectionRef{Name: "orders"}, Key: true},
			{Name: "total", Type: driver.TypeDecimal},
			{Name: "paid", Type: driver.TypeBool},
			{Name: "at", Type: driver.TypeTime},
		},
		rows: []driver.Row{
			{driver.Int(1), driver.Decimal("12.50"), driver.Bool(true), driver.Time(when)},
			{driver.Int(2), driver.Decimal("3.00"), driver.Bool(false), driver.Null()},
			{driver.Int(3), driver.Decimal("7.25"), driver.Bool(true), driver.Time(when)},
		},
	}
}

// newIssues is an API-like source with a list value.
func newIssues() *fakeConn {
	return &fakeConn{
		cols: []driver.Column{{Name: "number", Type: driver.TypeInt}, {Name: "labels", Type: driver.TypeList}},
		rows: []driver.Row{{driver.Int(7), driver.List(driver.String("bug"))}},
	}
}

const twoSources = "-- @source orders = pg/public: select id, total, paid, at from orders\n" +
	"-- @source issues = gh: {\"collection\":\"issues\"}\n" +
	"select o.id, i.number from orders o join issues i on i.number = o.id"

func TestRunStagesSourcesReadOnlyAndJoins(t *testing.T) {
	orders, issues := newOrders(), newIssues()
	open := fakeOpener{"pg": orders, "gh": issues}
	finalCols := []driver.Column{{Name: "id", Type: driver.TypeInt, Source: &driver.CollectionRef{Name: "orders"}, Key: true}, {Name: "number", Type: driver.TypeInt}}
	finalRows := []driver.Row{{driver.Int(1), driver.Int(7)}}
	reg, sc := registryWith(t, finalCols, finalRows)

	res, err := Run(context.Background(), open, mustParse(t, twoSources), Options{Registry: reg, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}

	// Every source ran read-only, with its namespace and the staging cap.
	for name, c := range map[string]*fakeConn{"pg": orders, "gh": issues} {
		qs := c.got("")
		if len(qs) != 1 {
			t.Fatalf("%s: %d queries, want 1", name, len(qs))
		}
		if q := qs[0]; !q.ReadOnly || q.Limit != DefaultMaxSourceRows {
			t.Errorf("%s: query %+v, want read-only with the default cap", name, q)
		}
	}
	if ns := orders.got("")[0].Namespace; ns != "public" {
		t.Errorf("orders namespace: %q", ns)
	}

	// The scratch tables mirror the source columns and types.
	creates := sc.got("create table")
	if len(creates) != 2 {
		t.Fatalf("creates: %d, want 2", len(creates))
	}
	wantOrders := `CREATE TABLE "orders" ("id" INTEGER, "total" NUMERIC, "paid" INTEGER, "at" TEXT)`
	if creates[0].Text != wantOrders {
		t.Errorf("orders table:\n%s\nwant\n%s", creates[0].Text, wantOrders)
	}
	if want := `CREATE TABLE "issues" ("number" INTEGER, "labels" TEXT)`; creates[1].Text != want {
		t.Errorf("issues table:\n%s\nwant\n%s", creates[1].Text, want)
	}

	// The rows go in as bound parameters, converted for SQLite.
	inserts := sc.got(`insert into "orders"`)
	if len(inserts) != 1 {
		t.Fatalf("orders inserts: %d, want 1", len(inserts))
	}
	ins := inserts[0]
	if want := `INSERT INTO "orders" VALUES (?, ?, ?, ?), (?, ?, ?, ?), (?, ?, ?, ?)`; ins.Text != want {
		t.Errorf("insert:\n%s\nwant\n%s", ins.Text, want)
	}
	got := make([]any, len(ins.Params))
	for i, p := range ins.Params {
		got[i] = p.Any()
	}
	want := []any{
		int64(1), "12.50", int64(1), "2026-11-05T11:30:00Z",
		int64(2), "3.00", int64(0), nil,
		int64(3), "7.25", int64(1), "2026-11-05T11:30:00Z",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("params:\n%v\nwant\n%v", got, want)
	}
	if lab := sc.got(`insert into "issues"`)[0].Params[1].Any(); lab != `["bug"]` {
		t.Errorf("list param: %v", lab)
	}

	// The final statement ran read-only on the scratch database.
	finals := sc.got("select")
	if len(finals) != 1 || !finals[0].ReadOnly || finals[0].Limit != 100 {
		t.Fatalf("final queries: %+v", finals)
	}
	if finals[0].Text != "select o.id, i.number from orders o join issues i on i.number = o.id" {
		t.Errorf("final text: %q", finals[0].Text)
	}

	// The result is the scratch result, with nothing left to edit inline.
	if len(res.Rows) != 1 || res.Rows[0][1].Any() != int64(7) {
		t.Errorf("rows: %v", res.Rows)
	}
	for _, c := range res.Columns {
		if c.Source != nil || c.Key {
			t.Errorf("column %s still points at a source", c.Name)
		}
	}
	if finalCols[0].Source == nil {
		t.Error("the scratch columns were changed in place")
	}
	if res.Stats.RowsAffected != -1 || res.Stats.Duration <= 0 {
		t.Errorf("stats: %+v", res.Stats)
	}
	wantNotes := []string{"source orders (pg): 3 rows", "source issues (gh): 1 rows"}
	if !reflect.DeepEqual(res.Stats.Warnings, wantNotes) {
		t.Errorf("warnings: %v, want %v", res.Stats.Warnings, wantNotes)
	}
	if len(res.Sources) != 2 || res.Sources[0].Rows != 3 || res.Sources[0].Alias != "orders" || res.Sources[1].Rows != 1 {
		t.Errorf("sources: %+v", res.Sources)
	}
}

func TestRunCapsEachSource(t *testing.T) {
	orders := newOrders()
	reg, sc := registryWith(t, nil, nil)
	st := mustParse(t, "-- @source o = pg: select 1\nselect count(*) from o")

	res, err := Run(context.Background(), fakeOpener{"pg": orders}, st, Options{Registry: reg, MaxSourceRows: 2})
	if err != nil {
		t.Fatal(err)
	}
	if q := orders.got("")[0]; q.Limit != 2 {
		t.Errorf("source limit: %d, want 2", q.Limit)
	}
	if n := len(sc.got(`insert into "o"`)[0].Params); n != 2*4 {
		t.Errorf("inserted params: %d, want 8 (two rows)", n)
	}
	if s := res.Sources[0]; s.Rows != 2 || !s.Truncated {
		t.Errorf("source run: %+v", s)
	}
	if want := "source o (pg): 2 rows, more existed"; res.Stats.Warnings[0] != want {
		t.Errorf("warning: %q, want %q", res.Stats.Warnings[0], want)
	}
}

func TestRunRefusesAFinalWrite(t *testing.T) {
	reg, _ := registryWith(t, nil, nil)
	st := mustParse(t, "-- @source o = pg: select 1\ndelete from o")
	res, err := Run(context.Background(), fakeOpener{"pg": newOrders()}, st, Options{Registry: reg})
	if !errors.Is(err, errFinalWrite) {
		t.Fatalf("got %v, want the final-write refusal", err)
	}
	if res == nil || len(res.Sources) != 1 || res.Sources[0].Rows != 3 {
		t.Errorf("the staged sources are still reported: %+v", res)
	}
}

func TestRunRedactsASourceFailure(t *testing.T) {
	failing := &fakeConn{err: driver.Wrap(driver.ErrAuth, "pg", "query", errors.New("password secret rejected"))}
	reg, sc := registryWith(t, nil, nil)
	st := mustParse(t, "-- @source o = pg: select 1\n-- @source i = gh: x\nselect 1")
	res, err := Run(context.Background(), fakeOpener{"pg": failing, "gh": newIssues()}, st, Options{Registry: reg})
	var se *SourceError
	if !errors.As(err, &se) || se.Alias != "o" || se.Conn != "pg" {
		t.Fatalf("got %v, want a SourceError for o on pg", err)
	}
	if msg := err.Error(); strings.Contains(msg, "secret") || !strings.Contains(msg, "***") || !strings.HasPrefix(msg, "source o on pg: ") {
		t.Errorf("message: %q", msg)
	}
	if !errors.Is(err, driver.ErrAuth) {
		t.Error("the driver's classification is lost")
	}
	if res.Sources[0].Err == nil {
		t.Error("the failed source run has no error")
	}
	if n := len(sc.got("")); n != 0 {
		t.Errorf("the scratch database got %d queries after a source failed", n)
	}
}

func TestRunExplainsAReadOnlyRefusal(t *testing.T) {
	writer := &fakeConn{err: driver.Wrap(driver.ErrReadOnly, "pg", "query", errors.New("DELETE refused"))}
	reg, _ := registryWith(t, nil, nil)
	st := mustParse(t, "-- @source o = pg: delete from t\nselect 1")
	_, err := Run(context.Background(), fakeOpener{"pg": writer}, st, Options{Registry: reg})
	if err == nil || !strings.Contains(err.Error(), "sources are always read-only") || !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("got %v", err)
	}
}

func TestRunRefusesConnectionsOutsideTheAllowedSet(t *testing.T) {
	orders := newOrders()
	reg, sc := registryWith(t, nil, nil)
	st := mustParse(t, "-- @source o = pg: select 1\n-- @source i = gh: x\nselect 1")
	open := fakeOpener{"pg": orders, "gh": newIssues()}
	_, err := Run(context.Background(), open, st, Options{Registry: reg, Allowed: []string{"pg"}})
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line != 2 || !strings.Contains(pe.Msg, `unknown connection "gh"`) {
		t.Fatalf("got %v", err)
	}
	if len(sc.got("")) != 0 || len(orders.got("")) != 0 {
		t.Error("something ran before the check")
	}
	if _, err := Run(context.Background(), open, st, Options{Registry: reg, Allowed: []string{"pg", "gh"}}); err != nil {
		t.Errorf("both allowed: %v", err)
	}
}

func TestRunRefusesASinkForNow(t *testing.T) {
	reg, _ := registryWith(t, nil, nil)
	st := mustParse(t, "-- @source o = pg: select 1\n-- @sink my: t\nselect 1")
	_, err := Run(context.Background(), fakeOpener{"pg": newOrders()}, st, Options{Registry: reg})
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line != 2 {
		t.Fatalf("got %v", err)
	}
}

func TestRunNeedsTheSqliteDriver(t *testing.T) {
	st := mustParse(t, "-- @source o = pg: select 1\nselect 1")
	_, err := Run(context.Background(), fakeOpener{"pg": newOrders()}, st, Options{Registry: driver.NewRegistry()})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("got %v, want unsupported", err)
	}
}

func TestRunRefusesASourceWithoutColumns(t *testing.T) {
	reg, _ := registryWith(t, nil, nil)
	st := mustParse(t, "-- @source o = pg: select 1\nselect 1")
	_, err := Run(context.Background(), fakeOpener{"pg": &fakeConn{}}, st, Options{Registry: reg})
	if err == nil || !strings.Contains(err.Error(), "no columns") {
		t.Errorf("got %v", err)
	}
}

func TestColumnNamesAreDistinctAndNonEmpty(t *testing.T) {
	cols := []driver.Column{{Name: "id"}, {Name: ""}, {Name: "ID"}, {Name: "id"}, {Name: "id_2"}}
	got := columnNames(cols)
	want := []string{"id", "column2", "ID_2", "id_3", "id_2_2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestColumnTypesFallBackToTheFirstValue(t *testing.T) {
	cols := []driver.Column{{Name: "a", Type: driver.TypeUnknown}, {Name: "b"}, {Name: "c", Type: driver.TypeFloat}}
	rows := []driver.Row{{driver.Null(), driver.Null(), driver.Null()}, {driver.Decimal("1"), driver.Null(), driver.Null()}}
	got := columnTypes(cols, rows)
	if want := []string{"NUMERIC", "TEXT", "REAL"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestScratchValues(t *testing.T) {
	tests := []struct {
		in   driver.Value
		want any
	}{
		{driver.Null(), nil},
		{driver.Int(3), int64(3)},
		{driver.Float(1.5), 1.5},
		{driver.String("x"), "x"},
		{driver.Bytes([]byte{1}), []byte{1}},
		{driver.Bool(true), int64(1)},
		{driver.Bool(false), int64(0)},
		{driver.Decimal("12.50"), "12.50"},
		{driver.Time(when), "2026-11-05T11:30:00Z"},
		{driver.JSON([]byte(`{"a":1}`)), `{"a":1}`},
		{driver.Vector([]float32{0.5, 1}), "[0.5,1]"},
		{driver.List(driver.Int(1), driver.String("b")), `[1,"b"]`},
		{driver.ObjectValue(driver.Member{Key: "k", Value: driver.Bool(true)}), `{"k":true}`},
		{driver.Unknown("geometry", []byte{1, 2}, "POINT(1 2)"), "POINT(1 2)"},
	}
	for _, tc := range tests {
		if got := scratchValue(tc.in).Any(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: got %#v, want %#v", tc.in.Type(), got, tc.want)
		}
	}
}

func TestExplainNeverOpensAConnection(t *testing.T) {
	plan := Explain(mustParse(t, twoSources))
	for _, want := range []string{"source orders: pg/public, read-only", "select id, total, paid, at from orders", "source issues: gh, read-only", "final statement (sqlite, read-only)", "join issues i"} {
		if !strings.Contains(plan.Text, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan.Text)
		}
	}
	if plan.Root == nil || len(plan.Root.Children) != 3 {
		t.Errorf("plan tree: %+v", plan.Root)
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Errorf("got %s", got)
	}
}
