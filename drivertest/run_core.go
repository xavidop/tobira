package drivertest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

var coreSubtests = []subtest{
	{"Info", testInfo},
	{"Ping", testPing},
	{"Namespaces", testNamespaces},
	{"Collections", testCollections},
	{"Describe", testDescribe},
	{"DescribeNotFound", testDescribeNotFound},
	{"QueryRead", testQueryRead},
	{"QueryLimit", testQueryLimit},
	{"QueryParams", testQueryParams},
	{"QueryMissing", testQueryMissing},
	{"Cancellation", testCancellation},
	{"ReadOnly", testReadOnly},
	{"BadAuth", testBadAuth},
	{"Close", testClose},
	{"Concurrency", testConcurrency},
}

// openConn opens a Conn and closes it when the test ends.
func openConn(t *testing.T, open func(testing.TB) driver.Conn) driver.Conn {
	t.Helper()
	conn := open(t)
	if conn == nil {
		t.Fatal("Config.Open returned a nil Conn without failing the test")
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// runQuery runs q and drains it, reporting errors from either Query or Next.
func runQuery(ctx context.Context, conn driver.Querier, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats, error) {
	rs, err := conn.Query(ctx, q)
	if err != nil {
		return nil, nil, driver.Stats{}, err
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx, rs, 0)
	return cols, rows, st, err
}

func ctxTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testInfo(t *testing.T, c Config) {
	info := c.Driver.Info()
	if info.Name == "" {
		t.Error("Info().Name is empty: set the registry name")
	}
	if info.Kind == "" {
		t.Error("Info().Kind is empty: set a driver.Kind")
	}
	if info.Dialect == "" {
		t.Error("Info().Dialect is empty: name the native query language")
	}
	if len(info.Schemes) == 0 {
		t.Error("Info().Schemes is empty: list at least one DSN scheme")
	}
	for _, s := range info.Schemes {
		if s == "" {
			t.Error("Info().Schemes contains an empty scheme")
		}
	}
	conn := openConn(t, c.Open)
	got := driver.CapabilitiesOf(conn)
	for _, cp := range info.Capabilities {
		if cp == driver.CapReadOnly {
			continue // declared, not an interface: the ReadOnlyGuard subtest probes it
		}
		if !slices.Contains(got, cp) {
			t.Errorf("Info().Capabilities lists %q but CapabilitiesOf(conn) does not offer it: implement the interface or remove it from Info", cp)
		}
	}
	for _, cp := range got {
		if !slices.Contains(info.Capabilities, cp) {
			t.Errorf("CapabilitiesOf(conn) offers %q but Info().Capabilities does not list it: add it to Info", cp)
		}
	}
}

func testPing(t *testing.T, c Config) {
	conn := openConn(t, c.Open)
	if err := conn.Ping(ctxTimeout(t)); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func testNamespaces(t *testing.T, c Config) {
	conn := openConn(t, c.Open)
	nss, err := conn.Namespaces(ctxTimeout(t))
	if err != nil {
		t.Fatalf("Namespaces: %v", err)
	}
	var names []string
	for _, ns := range nss {
		names = append(names, ns.Name)
	}
	if !slices.Contains(names, c.Fixture.Namespace) {
		t.Fatalf("Namespaces: fixture namespace %q not found, got %q", c.Fixture.Namespace, names)
	}
}

func testCollections(t *testing.T, c Config) {
	conn := openConn(t, c.Open)
	cols, err := conn.Collections(ctxTimeout(t), driver.NamespaceRef{Name: c.Fixture.Namespace})
	if err != nil {
		t.Fatalf("Collections(%s): %v", c.Fixture.Namespace, err)
	}
	var names []string
	for _, col := range cols {
		names = append(names, col.Ref.Name)
		if col.Ref.Name != c.Fixture.Collection {
			continue
		}
		if !validCollectionType(col.Type) {
			t.Errorf("Collections(%s): %s has Type %q, want one of %q",
				c.Fixture.Namespace, col.Ref.Name, col.Type, collectionTypes)
		}
		return
	}
	t.Fatalf("Collections(%s): People collection %q not found, got %q", c.Fixture.Namespace, c.Fixture.Collection, names)
}

// collectionTypes lists every valid driver.CollectionType.
var collectionTypes = []driver.CollectionType{
	driver.CollectionTable, driver.CollectionView, driver.CollectionMaterializedView,
	driver.CollectionCollection, driver.CollectionMeasurement, driver.CollectionIndex,
	driver.CollectionKeyPattern, driver.CollectionOther,
}

func validCollectionType(t driver.CollectionType) bool { return slices.Contains(collectionTypes, t) }

// wantType is the Type Describe must report for a People column: the
// Fixture.TypeOverrides entry if any, else the People default.
func (f Fixture) wantType(col driver.Column) driver.Type {
	if t, ok := f.TypeOverrides[f.Collection+"."+col.Name]; ok {
		return t
	}
	if t, ok := f.TypeOverrides[col.Name]; ok {
		return t
	}
	return col.Type
}

// equalPeopleValue compares a People value, honouring a type override: the
// got value must then have the overridden Type, and match numerically within
// 1e-9 (numbers) or as equal JSON (JSON stored as String).
func (f Fixture) equalPeopleValue(col driver.Column, want, got driver.Value) bool {
	ot := f.wantType(col)
	if ot == col.Type || want.Type() == driver.TypeNull || got.Type() == driver.TypeNull {
		return EqualValue(want, got)
	}
	if got.Type() != ot {
		return false
	}
	if col.Type == driver.TypeTime {
		return equalTimeOverride(ot, want, got)
	}
	if col.Type == driver.TypeInt && ot == driver.TypeString {
		return equalIntText(want, got)
	}
	if ot == driver.TypeFloat && isNumeric(want.Type()) {
		w, okw := toRat(want)
		g, okg := got.AsFloat()
		if !okw || !okg {
			return false
		}
		wf, _ := w.Float64()
		return math.Abs(wf-g) <= 1e-9
	}
	return EqualValue(want, got)
}

// equalIntText compares an Int fixture value with the decimal text a store
// holds when its keys must be strings (Cosmos DB's id): "7" for 7, never
// "07" or "+7".
func equalIntText(want, got driver.Value) bool {
	n, ok := want.AsInt()
	s, _ := got.AsText()
	return ok && s == strconv.FormatInt(n, 10)
}

// equalTimeOverride compares a Time fixture value with its stored form: Int
// holds Unix nanoseconds and must equal UnixNano exactly; String holds
// RFC 3339 (fractional seconds accepted) and must denote the same instant.
func equalTimeOverride(ot driver.Type, want, got driver.Value) bool {
	wt, ok := want.AsTime()
	if !ok {
		return false
	}
	switch ot {
	case driver.TypeInt:
		n, ok := got.AsInt()
		return ok && n == wt.UnixNano()
	case driver.TypeString:
		s, ok := got.AsText()
		if !ok || got.Type() != driver.TypeString {
			return false
		}
		gt, err := time.Parse(time.RFC3339Nano, s)
		return err == nil && gt.Equal(wt)
	}
	return false
}

func (f Fixture) ref() driver.CollectionRef {
	return driver.CollectionRef{Namespace: f.Namespace, Name: f.Collection}
}

func testDescribe(t *testing.T, c Config) {
	conn := openConn(t, c.Open)
	name := c.Fixture.Collection
	sch, err := conn.Describe(ctxTimeout(t), c.Fixture.ref())
	if err != nil {
		t.Fatalf("Describe(%s): %v", name, err)
	}
	for _, want := range People.Columns {
		i := slices.IndexFunc(sch.Fields, func(f driver.Field) bool { return f.Name == want.Name })
		if i < 0 {
			t.Errorf("Describe(%s): field %q is missing", name, want.Name)
			continue
		}
		wt, got := c.Fixture.wantType(want), sch.Fields[i].Type
		if got != wt {
			t.Errorf("Describe(%s): field %q: want Type %s, got %s (native type %q)",
				name, want.Name, wt, got, sch.Fields[i].NativeType)
		}
	}
	if want := c.Fixture.AutoIncrementKey; want != nil {
		i := slices.IndexFunc(sch.Fields, func(f driver.Field) bool { return f.Name == "id" })
		if i >= 0 && sch.Fields[i].AutoIncrement != *want {
			t.Errorf("Describe(%s): field id: want AutoIncrement %v, got %v", name, *want, sch.Fields[i].AutoIncrement)
		}
	}
	if !c.Fixture.NoPrimaryKey && !slices.Equal(sch.PrimaryKey, People.Key) {
		t.Errorf("Describe(%s): want PrimaryKey %q, got %q", name, People.Key, sch.PrimaryKey)
	}
}

func testDescribeNotFound(t *testing.T, c Config) {
	conn := openConn(t, c.Open)
	ref := driver.CollectionRef{Namespace: c.Fixture.Namespace, Name: "tobira_no_such_collection"}
	_, err := conn.Describe(ctxTimeout(t), ref)
	if !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Describe(%s): want error matching driver.ErrNotFound, got %v", ref.Name, err)
	}
	checkDriverError(t, c, "Describe", err)
}

func testQueryRead(t *testing.T, c Config) {
	conn := openConn(t, c.Open)
	cols, rows, st, err := runQuery(ctxTimeout(t), conn, c.Fixture.SelectAll)
	if err != nil {
		t.Fatalf("Query(SelectAll): %v", err)
	}
	comparePeople(t, c.Fixture, "Query(SelectAll)", cols, rows)
	if st.Truncated {
		t.Error("Stats.Truncated is true for an unlimited query")
	}
	if st.Duration < 0 {
		t.Errorf("Stats.Duration = %v, want >= 0", st.Duration)
	}
}

// comparePeople checks a SelectAll result against People by column name.
// Failures report row id, column, want and got.
func comparePeople(t *testing.T, fx Fixture, what string, cols []driver.Column, rows []driver.Row) {
	t.Helper()
	for _, m := range peopleMismatches(fx, what, cols, rows) {
		t.Error(m)
	}
}

// peopleMismatches compares a SelectAll result with People and describes
// each difference (at most 10 value mismatches, then a total). Rows are
// compared in order, or, with fx.UnorderedSelectAll, matched by id first.
func peopleMismatches(fx Fixture, what string, cols []driver.Column, rows []driver.Row) []string {
	if len(rows) != len(People.Rows) {
		return []string{fmt.Sprintf("%s: want %d rows, got %d", what, len(People.Rows), len(rows))}
	}
	idx := make([]int, len(People.Columns)) // People column -> result column
	for i, pc := range People.Columns {
		idx[i] = slices.IndexFunc(cols, func(col driver.Column) bool { return col.Name == pc.Name })
		if idx[i] < 0 {
			return []string{fmt.Sprintf("%s: result has no column %q", what, pc.Name)}
		}
	}
	for r, got := range rows {
		if len(got) != len(cols) {
			return []string{fmt.Sprintf("%s: row %d has %d values for %d columns", what, r, len(got), len(cols))}
		}
	}
	if fx.UnorderedSelectAll {
		ordered, out := orderByID(fx, what, idx[0], rows)
		if len(out) > 0 {
			return out
		}
		rows = ordered
	}
	var out []string
	bad := 0
	for r, want := range People.Rows {
		got := rows[r]
		for i, pc := range People.Columns {
			if !fx.equalPeopleValue(pc, want[i], got[idx[i]]) {
				bad++
				if bad <= 10 {
					out = append(out, fmt.Sprintf("%s: row id %v, column %q: want %s (%s), got %s (%s)",
						what, want[0], pc.Name, want[i], want[i].Type(), got[idx[i]], got[idx[i]].Type()))
				}
			}
		}
	}
	if bad > 10 {
		out = append(out, fmt.Sprintf("%s: %d value mismatches in total (first 10 shown)", what, bad))
	}
	return out
}

// orderByID puts rows in People's order by matching their id column
// (result column idCol) to People's ids. Every People id must match
// exactly one row; otherwise it describes what is missing or duplicated.
func orderByID(fx Fixture, what string, idCol int, rows []driver.Row) ([]driver.Row, []string) {
	idType := peopleIDColumn()
	ordered := make([]driver.Row, len(People.Rows))
	var out []string
	for r, want := range People.Rows {
		var hits []int
		for g, got := range rows {
			if fx.equalPeopleValue(idType, want[0], got[idCol]) {
				hits = append(hits, g)
			}
		}
		switch len(hits) {
		case 0:
			out = append(out, fmt.Sprintf("%s: row id %v is missing", what, want[0]))
		case 1:
			ordered[r] = rows[hits[0]]
		default:
			out = append(out, fmt.Sprintf("%s: row id %v appears %d times", what, want[0], len(hits)))
		}
	}
	return ordered, out
}

func testQueryLimit(t *testing.T, c Config) {
	conn := openConn(t, c.Open)
	total := len(People.Rows)
	for _, tc := range []struct {
		limit, rows int
		truncated   bool
	}{{10, 10, true}, {total, total, false}, {total + 1, total, false}} {
		q := c.Fixture.SelectAll
		q.Limit = tc.limit
		_, rows, st, err := runQuery(ctxTimeout(t), conn, q)
		if err != nil {
			t.Errorf("Query(Limit %d): %v", tc.limit, err)
			continue
		}
		if len(rows) != tc.rows {
			t.Errorf("Query(Limit %d): want %d rows, got %d", tc.limit, tc.rows, len(rows))
		}
		if st.Truncated != tc.truncated {
			t.Errorf("Query(Limit %d) over %d rows: Stats.Truncated is %v, want %v", tc.limit, total, st.Truncated, tc.truncated)
		}
	}
}

func testQueryParams(t *testing.T, c Config) {
	if c.Fixture.ParamQuery.Text == "" {
		t.Skip("Fixture.ParamQuery not set: dialect has no query parameters")
	}
	conn := openConn(t, c.Open)
	cols, rows, _, err := runQuery(ctxTimeout(t), conn, c.Fixture.ParamQuery)
	if err != nil {
		t.Fatalf("Query(ParamQuery): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Query(ParamQuery): want 1 row for id 7, got %d", len(rows))
	}
	i := slices.IndexFunc(cols, func(col driver.Column) bool { return col.Name == "id" })
	if i < 0 {
		t.Fatal(`Query(ParamQuery): result has no column "id"`)
	}
	if !c.Fixture.equalPeopleValue(peopleIDColumn(), driver.Int(7), rows[0][i]) {
		t.Errorf("Query(ParamQuery): want id 7, got %s", rows[0][i])
	}
}

func testQueryMissing(t *testing.T, c Config) {
	conn := openConn(t, c.Open)
	_, _, _, err := runQuery(ctxTimeout(t), conn, c.Fixture.Missing)
	if !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Query(Missing %q): want error matching driver.ErrNotFound, got %v", c.Fixture.Missing.Text, err)
	}
	checkDriverError(t, c, "Query", err)
}

// checkDriverError asserts err is a *driver.Error carrying the driver's name.
func checkDriverError(t *testing.T, c Config, op string, err error) {
	t.Helper()
	var de *driver.Error
	if !errors.As(err, &de) {
		t.Errorf("%s: error is %T, want a *driver.Error built with driver.Wrap: %v", op, err, err)
		return
	}
	if want := c.Driver.Info().Name; de.Driver != want {
		t.Errorf("%s: driver.Error.Driver = %q, want %q (the name in Info)", op, de.Driver, want)
	}
}

func testCancellation(t *testing.T, c Config) {
	conn := openConn(t, c.Open)
	t.Run("PreCancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, _, err := runQuery(ctx, conn, c.Fixture.SelectAll)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Query with an already-cancelled context: want error matching context.Canceled, got %v", err)
		}
		live := ctxTimeout(t)
		if err := conn.Ping(live); err != nil {
			t.Errorf("Ping after a cancelled query: the Conn is no longer usable: %v", err)
		}
		if _, rows, _, err := runQuery(live, conn, c.Fixture.SelectAll); err != nil || len(rows) != len(People.Rows) {
			t.Errorf("SelectAll after a cancelled query: got %d rows, err %v; want %d rows", len(rows), err, len(People.Rows))
		}
	})
	t.Run("MidQuery", func(t *testing.T) {
		if c.Fixture.Slow.Text == "" {
			t.Skip("Fixture.Slow not set")
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(100*time.Millisecond, cancel)
		start := time.Now()
		rs, err := conn.Query(ctx, c.Fixture.Slow)
		if err == nil {
			defer func() { _ = rs.Close() }()
			for {
				var ok bool
				if _, ok, err = rs.Next(ctx); err != nil || !ok {
					break
				}
			}
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("cancelling a running query took %v to take effect, want < 2s", elapsed)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled slow query: want error matching context.Canceled, got %v", err)
		}
	})
}

func testReadOnly(t *testing.T, c Config) {
	verify := openConn(t, c.Open)
	check := func(t *testing.T) {
		t.Helper()
		cols, rows, _, err := runQuery(ctxTimeout(t), verify, c.Fixture.SelectAll)
		if err != nil {
			t.Fatalf("re-reading People after a refused write: %v", err)
		}
		comparePeople(t, c.Fixture, "People after a refused write", cols, rows)
	}
	t.Run("ConnReadOnly", func(t *testing.T) {
		ro := openConn(t, c.OpenReadOnly)
		_, _, _, err := runQuery(ctxTimeout(t), ro, c.Fixture.Write)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Write on a ReadOnly conn: want error matching driver.ErrReadOnly, got %v", err)
		}
		check(t)
	})
	t.Run("QueryReadOnly", func(t *testing.T) {
		q := c.Fixture.Write
		q.ReadOnly = true
		_, _, _, err := runQuery(ctxTimeout(t), verify, q)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Write with Query.ReadOnly on a normal conn: want error matching driver.ErrReadOnly, got %v", err)
		}
		check(t)
	})
}

func testBadAuth(t *testing.T, c Config) {
	if c.OpenBadAuth == nil {
		t.Skip("Config.OpenBadAuth not set: driver has no authentication")
	}
	err := c.OpenBadAuth(ctxTimeout(t))
	if !errors.Is(err, driver.ErrAuth) {
		t.Fatalf("open with wrong credentials: want error matching driver.ErrAuth, got %v", err)
	}
}

func testClose(t *testing.T, c Config) {
	conn := c.Open(t) // closed by this test, not by cleanup
	if conn == nil {
		t.Fatal("Config.Open returned a nil Conn without failing the test")
	}
	t.Cleanup(func() { _ = conn.Close() }) // safety net if the test fails early
	if err := conn.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("second Close panicked: %v (Close must be idempotent)", r)
			}
		}()
		_ = conn.Close() // nil or an error are both acceptable
	}()
	ctx := ctxTimeout(t)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Ping after Close panicked: %v (want an error)", r)
			}
		}()
		if err := conn.Ping(ctx); err == nil {
			t.Error("Ping after Close returned nil, want an error")
		}
	}()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Query after Close panicked: %v (want an error)", r)
			}
		}()
		_, _, _, err := runQuery(ctx, conn, c.Fixture.SelectAll)
		if err == nil {
			t.Error("Query after Close returned nil, want an error")
		}
	}()
}

func testConcurrency(t *testing.T, c Config) {
	const workers = 8
	conn := openConn(t, c.Open)
	ctx := ctxTimeout(t)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rows, _, err := runQuery(ctx, conn, c.Fixture.SelectAll)
			if err == nil && len(rows) != len(People.Rows) {
				err = fmt.Errorf("want %d rows, got %d", len(People.Rows), len(rows))
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent query %d of %d on one Conn: %v", i+1, workers, err)
		}
	}
}
