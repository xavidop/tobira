package drivertest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// capSubtests run after the core subtests, in spec order. Each one opens its
// own Conn and skips when the driver does not offer the capability.
var capSubtests = []subtest{
	{"Browser", testBrowser},
	{"Explainer", testExplainer},
	{"Mutator", testMutator},
	{"Transactor", testTransactor},
	{"Completer", testCompleter},
	{"Exporter", testExporter},
	{"Watcher", testWatcher},
	{"VectorSearcher", testVectorSearcher},
	{"TimeSeriesReader", testTimeSeriesReader},
	{"GraphReader", testGraphReader},
	{"ReadOnlyGuard", testReadOnlyGuard},
}

// needCap opens a Conn and returns it as T, skipping the test when the driver
// does not offer capability cp.
func needCap[T any](t *testing.T, c Config, cp driver.Capability) T {
	t.Helper()
	conn := openConn(t, c.Open)
	v, ok := driver.As[T](conn)
	if !ok {
		if c.Strict && slices.Contains(c.Driver.Info().Capabilities, cp) {
			t.Fatalf("driver claims %s in Info().Capabilities but driver.As does not offer it", cp)
		}
		t.Skipf("driver does not support %s", cp)
	}
	return v
}

// missingFixture ends a capability subtest whose Fixture input is absent: a
// failure under Config.Strict, a skip otherwise.
func missingFixture(t *testing.T, c Config, field string) {
	t.Helper()
	if c.Strict {
		t.Fatalf("Config.Strict: capability is claimed but missing Fixture.%s", field)
	}
	t.Skipf("claimed but untested: missing Fixture.%s", field)
}

// atLeastOne treats a zero Min* fixture value as 1.
func atLeastOne(n int) int { return max(n, 1) }

func columnIndex(t *testing.T, cols []driver.Column, name string) int {
	t.Helper()
	i := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("result has no column %q", name)
	}
	return i
}

// selectAll runs Fixture.SelectAll and fails the test on error.
func selectAll(t *testing.T, conn driver.Querier, c Config) ([]driver.Column, []driver.Row) {
	t.Helper()
	cols, rows, _, err := runQuery(ctxTimeout(t), conn, c.Fixture.SelectAll)
	if err != nil {
		t.Fatalf("Query(SelectAll): %v", err)
	}
	return cols, rows
}

// matchesPeople reports whether a SelectAll result equals People, as
// comparePeople does, honouring fx.TypeOverrides and
// fx.UnorderedSelectAll.
func matchesPeople(fx Fixture, cols []driver.Column, rows []driver.Row) bool {
	return len(peopleMismatches(fx, "", cols, rows)) == 0
}

// browsePage runs one Browse call and drains it.
func browsePage(ctx context.Context, b driver.Browser, ref driver.CollectionRef, opts driver.BrowseOptions) ([]driver.Column, []driver.Row, driver.Stats, error) {
	rs, err := b.Browse(ctx, ref, opts)
	if err != nil {
		return nil, nil, driver.Stats{}, err
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx, rs, 0)
	return cols, rows, st, err
}

func testBrowser(t *testing.T, c Config) {
	b := needCap[driver.Browser](t, c, driver.CapBrowser)
	ctx, ref := ctxTimeout(t), c.Fixture.ref()

	t.Run("Paging", func(t *testing.T) {
		seen := map[int64]bool{}
		var sizes []int
		cursor := ""
		for page := 0; page < 10; page++ {
			cols, rows, st, err := browsePage(ctx, b, ref, driver.BrowseOptions{Limit: 20, Cursor: cursor})
			if err != nil {
				t.Fatalf("Browse(page %d): %v", page+1, err)
			}
			id := columnIndex(t, cols, "id")
			for _, r := range rows {
				n, ok := c.Fixture.peopleID(r[id])
				if !ok {
					t.Errorf("Browse: id %s (%s) is not a People id", r[id], r[id].Type())
					continue
				}
				if seen[n] {
					t.Errorf("Browse: id %d returned twice across pages", n)
				}
				seen[n] = true
			}
			sizes = append(sizes, len(rows))
			cursor = st.NextCursor
			if cursor == "" {
				break
			}
		}
		if cursor != "" {
			t.Fatalf("Browse: still had a NextCursor after 10 pages of 20 over %d rows", len(People.Rows))
		}
		if want := []int{20, 20, 17}; !slices.Equal(sizes, want) {
			t.Errorf("Browse: page sizes %v, want %v (the last page ends with NextCursor \"\")", sizes, want)
		}
		if len(seen) != len(People.Rows) {
			t.Errorf("Browse: saw %d distinct ids, want %d", len(seen), len(People.Rows))
		}
	})

	t.Run("Sort", func(t *testing.T) {
		var idSets [2][]int64
		for i, desc := range []bool{true, false} {
			cols, rows, _, err := browsePage(ctx, b, ref, driver.BrowseOptions{Limit: 100, Sort: []driver.SortKey{{Field: "age", Desc: desc}}})
			if err != nil {
				t.Fatalf("Browse(Sort age desc=%v): %v", desc, err)
			}
			if len(rows) != len(People.Rows) {
				t.Fatalf("Browse(Sort age desc=%v): got %d rows, want %d", desc, len(rows), len(People.Rows))
			}
			checkSortedByAge(t, desc, rows, columnIndex(t, cols, "age"))
			id := columnIndex(t, cols, "id")
			for _, r := range rows {
				n, ok := c.Fixture.peopleID(r[id])
				if !ok {
					t.Errorf("Browse(Sort age): id %s (%s) is not a People id", r[id], r[id].Type())
				}
				idSets[i] = append(idSets[i], n)
			}
			slices.Sort(idSets[i])
		}
		if !slices.Equal(idSets[0], idSets[1]) {
			t.Errorf("Browse(Sort age): desc and asc returned different id sets")
		}
	})

	t.Run("FilterEq", func(t *testing.T) {
		wantActive := 0
		ai := slices.IndexFunc(People.Columns, func(col driver.Column) bool { return col.Name == "active" })
		for _, r := range People.Rows {
			if on, _ := r[ai].AsBool(); on {
				wantActive++
			}
		}
		cols, rows, _, err := browsePage(ctx, b, ref, driver.BrowseOptions{
			Limit:  100,
			Filter: driver.Filter{Conditions: []driver.Condition{{Field: "active", Op: driver.OpEq, Value: driver.Bool(true)}}},
		})
		if err != nil {
			t.Fatalf("Browse(active eq true): %v", err)
		}
		active := columnIndex(t, cols, "active")
		for _, r := range rows {
			if !EqualValue(driver.Bool(true), r[active]) {
				t.Errorf("Browse(active eq true): returned a row with active = %s", r[active])
			}
		}
		if len(rows) != wantActive {
			t.Errorf("Browse(active eq true): got %d rows, want %d", len(rows), wantActive)
		}
	})

	t.Run("OpContains", func(t *testing.T) {
		cols, rows, _, err := browsePage(ctx, b, ref, driver.BrowseOptions{
			Limit:  100,
			Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpContains, Value: driver.String("1")}}},
		})
		if errors.Is(err, driver.ErrUnsupported) {
			checkDriverError(t, c, "Browse(id contains)", err)
			return
		}
		if err != nil {
			t.Fatalf("Browse(id contains \"1\"): want rows or ErrUnsupported, got %v", err)
		}
		want := 0
		pid := slices.IndexFunc(People.Columns, func(col driver.Column) bool { return col.Name == "id" })
		for _, r := range People.Rows {
			if n, _ := r[pid].AsInt(); strings.Contains(strconv.FormatInt(n, 10), "1") {
				want++
			}
		}
		id := columnIndex(t, cols, "id")
		for _, r := range rows {
			n, ok := c.Fixture.peopleID(r[id])
			if !ok {
				t.Errorf("Browse(id contains \"1\"): id %s (%s) is not a People id", r[id], r[id].Type())
				continue
			}
			if !strings.Contains(strconv.FormatInt(n, 10), "1") {
				t.Errorf("Browse(id contains \"1\"): returned id %d", n)
			}
		}
		if len(rows) != want {
			t.Errorf("Browse(id contains \"1\"): got %d rows, want %d", len(rows), want)
		}
	})

	t.Run("ReadOnlyFlag", func(t *testing.T) {
		_, rows, _, err := browsePage(ctx, b, ref, driver.BrowseOptions{Limit: 20, ReadOnly: true})
		if err != nil {
			t.Fatalf("Browse(ReadOnly): %v", err)
		}
		if len(rows) != 20 {
			t.Errorf("Browse(ReadOnly): got %d rows, want 20", len(rows))
		}
	})
}

// checkSortedByAge asserts rows are ordered by the age column, ignoring NULLs,
// and that the NULLs sit together at one end.
func checkSortedByAge(t *testing.T, desc bool, rows []driver.Row, age int) {
	t.Helper()
	dir := map[bool]string{true: "desc", false: "asc"}[desc]
	var nulls []int
	var prev *int64
	for i, r := range rows {
		if r[age].IsNull() {
			nulls = append(nulls, i)
			continue
		}
		n, ok := r[age].AsInt()
		if !ok {
			t.Fatalf("Browse(Sort age %s): row %d age is %s, want an integer", dir, i, r[age])
		}
		if prev != nil && ((desc && n > *prev) || (!desc && n < *prev)) {
			t.Errorf("Browse(Sort age %s): row %d age %d follows %d", dir, i, n, *prev)
		}
		prev = &n
	}
	if len(nulls) == 0 {
		return
	}
	atStart := nulls[len(nulls)-1] == len(nulls)-1
	atEnd := nulls[0] == len(rows)-len(nulls)
	if !atStart && !atEnd {
		t.Errorf("Browse(Sort age %s): NULL ages are scattered at rows %v, want them all at one end", dir, nulls)
	}
}

func testExplainer(t *testing.T, c Config) {
	e := needCap[driver.Explainer](t, c, driver.CapExplainer)
	plan, err := e.Explain(ctxTimeout(t), c.Fixture.SelectAll, driver.ExplainOptions{})
	if err != nil {
		t.Fatalf("Explain(SelectAll): %v", err)
	}
	if plan == nil || plan.Text == "" {
		t.Errorf("Explain(SelectAll): Plan.Text is empty, want the native plan output: %+v", plan)
	}
}

// peopleIDColumn is People's id column, found by name.
func peopleIDColumn() driver.Column {
	return People.Columns[slices.IndexFunc(People.Columns, func(c driver.Column) bool { return c.Name == "id" })]
}

// peopleID reads a People id from a result value: an Int, or its exact
// decimal text when Fixture.TypeOverrides stores id as a String.
func (f Fixture) peopleID(v driver.Value) (int64, bool) {
	if n, ok := v.AsInt(); ok {
		return n, true
	}
	if v.Type() != driver.TypeString || f.wantType(peopleIDColumn()) != driver.TypeString {
		return 0, false
	}
	s, _ := v.AsText()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != s {
		return 0, false
	}
	return n, true
}

// idValue is the value the kit writes for People id n: an Int, or its
// decimal text when Fixture.TypeOverrides stores id as a String.
func (f Fixture) idValue(n int64) driver.Value {
	if f.wantType(peopleIDColumn()) == driver.TypeString {
		return driver.String(strconv.FormatInt(n, 10))
	}
	return driver.Int(n)
}

// probeRow is the People-shaped row the Mutator suite inserts, followed by
// fx.InsertDefaults in key order.
func (fx Fixture) probeRow(id int64, name string) driver.Object {
	o := fx.peopleProbe(id, name)
	keys := make([]string, 0, len(fx.InsertDefaults))
	for k := range fx.InsertDefaults {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		o = append(o, driver.Member{Key: k, Value: fx.InsertDefaults[k]})
	}
	return o
}

// checkInsertDefaults refuses a default that names a People column.
func (fx Fixture) checkInsertDefaults() error {
	for k := range fx.InsertDefaults {
		if slices.ContainsFunc(People.Columns, func(c driver.Column) bool { return c.Name == k }) {
			return fmt.Errorf("Fixture.InsertDefaults names the People column %q: a default may only add a member the store needs, never replace a People value", k)
		}
	}
	return nil
}

// peopleProbe is the People-shaped probe row.
func (fx Fixture) peopleProbe(id int64, name string) driver.Object {
	vals := map[string]driver.Value{
		"id":         fx.idValue(id),
		"name":       driver.String(name),
		"email":      driver.String("probe" + strconv.FormatInt(id, 10) + "@example.com"),
		"age":        driver.Int(33),
		"score":      driver.Decimal("1.50"),
		"active":     driver.Bool(true),
		"created_at": driver.Time(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)),
		"tags":       driver.JSON([]byte(`[]`)),
	}
	var o driver.Object
	for _, col := range People.Columns {
		o = append(o, driver.Member{Key: col.Name, Value: vals[col.Name]})
	}
	return o
}

// findName returns the name of the row with the given id in a SelectAll result.
func findName(t *testing.T, fx Fixture, cols []driver.Column, rows []driver.Row, id int64) (string, bool) {
	t.Helper()
	ii, ni := columnIndex(t, cols, "id"), columnIndex(t, cols, "name")
	for _, r := range rows {
		if n, ok := fx.peopleID(r[ii]); ok && n == id {
			s, _ := r[ni].AsText()
			return s, true
		}
	}
	return "", false
}

func testMutator(t *testing.T, c Config) {
	m := needCap[driver.Mutator](t, c, driver.CapMutator)
	if err := c.Fixture.checkInsertDefaults(); err != nil {
		t.Fatal(err)
	}
	conn := openConn(t, c.Open)
	q := driver.Querier(conn)
	ctx, ref := ctxTimeout(t), c.Fixture.ref()
	const probeID = 1000
	key := driver.Object{{Key: "id", Value: c.Fixture.idValue(probeID)}}
	// Best-effort cleanup so a failed step cannot leave the probe behind.
	t.Cleanup(func() { _ = m.Delete(context.Background(), ref, key) })

	if _, err := m.Insert(ctx, ref, c.Fixture.probeRow(probeID, "Conformance Probe")); err != nil {
		t.Fatalf("Insert(id %d): %v", probeID, err)
	}
	cols, rows := selectAll(t, q, c)
	if name, ok := findName(t, c.Fixture, cols, rows, probeID); !ok || name != "Conformance Probe" {
		t.Errorf("after Insert: id %d has name %q (present %v), want \"Conformance Probe\"", probeID, name, ok)
	}

	changes := driver.Object{{Key: "name", Value: driver.String("Renamed Probe")}}
	if err := m.Update(ctx, ref, key, changes); err != nil {
		t.Fatalf("Update(id %d): %v", probeID, err)
	}
	cols, rows = selectAll(t, q, c)
	if name, ok := findName(t, c.Fixture, cols, rows, probeID); !ok || name != "Renamed Probe" {
		t.Errorf("after Update: id %d has name %q (present %v), want \"Renamed Probe\"", probeID, name, ok)
	}

	if err := m.Delete(ctx, ref, key); err != nil {
		t.Fatalf("Delete(id %d): %v", probeID, err)
	}
	cols, rows = selectAll(t, q, c)
	if _, ok := findName(t, c.Fixture, cols, rows, probeID); ok {
		t.Errorf("after Delete: id %d is still present", probeID)
	}

	missing := driver.Object{{Key: "id", Value: c.Fixture.idValue(999999)}}
	err := m.Update(ctx, ref, missing, changes)
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Update(missing key): want error matching driver.ErrNotFound, got %v", err)
	}
	checkDriverError(t, c, "Update(missing key)", err)
	err = m.Delete(ctx, ref, missing)
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Delete(missing key): want error matching driver.ErrNotFound, got %v", err)
	}
	checkDriverError(t, c, "Delete(missing key)", err)

	t.Run("ReadOnly", func(t *testing.T) {
		ro := needCap[driver.Mutator](t, Config{Driver: c.Driver, Open: c.OpenReadOnly, Strict: c.Strict}, driver.CapMutator)
		existing := driver.Object{{Key: "id", Value: c.Fixture.idValue(1)}}
		for name, err := range map[string]error{
			"Insert": func() error { _, err := ro.Insert(ctx, ref, c.Fixture.probeRow(probeID, "x")); return err }(),
			"Update": ro.Update(ctx, ref, existing, changes),
			"Delete": ro.Delete(ctx, ref, existing),
		} {
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%s on a read-only conn: want error matching driver.ErrReadOnly, got %v", name, err)
			}
			checkDriverError(t, c, name+" on a read-only conn", err)
		}
	})

	cols, rows = selectAll(t, q, c)
	comparePeople(t, c.Fixture, "People after the Mutator suite", cols, rows)
}

func testTransactor(t *testing.T, c Config) {
	tr := needCap[driver.Transactor](t, c, driver.CapTransactor)
	if c.Fixture.TxWrite.Text == "" {
		missingFixture(t, c, "TxWrite")
	}
	conn := openConn(t, c.Open)
	ctx := ctxTimeout(t)

	runInTx := func(commit bool) {
		t.Helper()
		tx, err := tr.Begin(ctx, driver.TxOptions{})
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, _, _, err := runQuery(ctx, tx, c.Fixture.TxWrite); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("Tx.Query(TxWrite): %v", err)
		}
		if !commit {
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("Rollback: %v", err)
			}
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Errorf("Rollback after Commit: want a no-op, got %v", err)
		}
	}

	runInTx(false)
	cols, rows := selectAll(t, conn, c)
	comparePeople(t, c.Fixture, "People after Rollback", cols, rows)
	testTxExplainer(t, tr, c)

	if c.Fixture.TxRestore.Text == "" {
		if c.Strict {
			t.Fatal("Config.Strict: Transactor is claimed but Fixture.TxRestore is missing, so the commit check cannot run")
		}
		t.Log("claimed but untested: missing Fixture.TxRestore, commit check skipped because the kit cannot undo it")
		return
	}
	runInTx(true)
	cols, rows = selectAll(t, conn, c)
	changed := !matchesPeople(c.Fixture, cols, rows)
	if _, _, _, err := runQuery(ctx, conn, c.Fixture.TxRestore); err != nil {
		t.Fatalf("Query(TxRestore): %v", err)
	}
	if !changed {
		t.Error("People unchanged after Commit: Commit must persist the TxWrite")
	}
	cols, rows = selectAll(t, conn, c)
	comparePeople(t, c.Fixture, "People after TxRestore", cols, rows)
}

// testTxExplainer checks a Tx that is also an Explainer (optional): it
// explains SelectAll inside the transaction, which stays usable afterwards.
func testTxExplainer(t *testing.T, tr driver.Transactor, c Config) {
	t.Helper()
	ctx := ctxTimeout(t)
	tx, err := tr.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, ok := driver.As[driver.Explainer](tx)
	if !ok {
		t.Log("the Tx is not an Explainer: explaining inside a transaction is not offered")
		return
	}
	plan, err := e.Explain(ctx, c.Fixture.SelectAll, driver.ExplainOptions{})
	if err != nil {
		t.Fatalf("Tx.Explain(SelectAll): %v", err)
	}
	if plan == nil || plan.Text == "" {
		t.Errorf("Tx.Explain(SelectAll): Plan.Text is empty, want the native plan output: %+v", plan)
	}
	if _, _, _, err := runQuery(ctx, tx, c.Fixture.SelectAll); err != nil {
		t.Errorf("Tx.Query(SelectAll) after Tx.Explain: %v", err)
	}
}

func testCompleter(t *testing.T, c Config) {
	comp := needCap[driver.Completer](t, c, driver.CapCompleter)
	if c.Fixture.CompleteAt.Text == "" {
		missingFixture(t, c, "CompleteAt")
	}
	got, err := comp.Complete(ctxTimeout(t), c.Fixture.CompleteAt)
	if err != nil {
		t.Fatalf("Complete(%q at %d): %v", c.Fixture.CompleteAt.Text, c.Fixture.CompleteAt.Cursor, err)
	}
	if !slices.ContainsFunc(got, func(x driver.Completion) bool { return x.Label == c.Fixture.Collection }) {
		t.Errorf("Complete(%q at %d): no completion labelled %q in %+v", c.Fixture.CompleteAt.Text, c.Fixture.CompleteAt.Cursor, c.Fixture.Collection, got)
	}
}
