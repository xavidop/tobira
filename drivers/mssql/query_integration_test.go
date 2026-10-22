//go:build integration

package mssql

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// The guard's lexer rests on how SQL Server reads text. These probes are
// harmless queries whose result shows the server's reading, so a server
// that reads differently fails here rather than silently opening a bypass.
func TestServerLexingMatchesGuard(t *testing.T) {
	c := openMS(t, false)
	for _, tc := range []struct {
		name, text string
		cols       []string
	}{
		{"carriage return ends a line comment", "SELECT 1 AS a --x\r, 2 AS b", []string{"a", "b"}},
		{"line feed ends a line comment", "SELECT 1 AS a --x\n, 2 AS b", []string{"a", "b"}},
		{"form feed does not", "SELECT 1 AS a --x\f, 2 AS b", []string{"a"}},
		{"U+2028 does not", "SELECT 1 AS a --x , 2 AS b", []string{"a"}},
		{"NEL does not", "SELECT 1 AS a --x\u0085, 2 AS b", []string{"a"}},
		{"block comments nest", "SELECT 1 AS a /* /* */ , 2 AS b */", []string{"a"}},
		{"dashes inside a block comment are text", "SELECT 1 AS a /* -- */ , 2 AS b", []string{"a", "b"}},
		{"0x stops at the first non-hex byte", "SELECT 0x12abz", []string{"z"}},
		{"an exponent stops at a letter", "SELECT 1ex", []string{"x"}},
		{"a number stops at a letter", "SELECT 1x", []string{"x"}},
		{"brackets double their closer", "SELECT 1 AS [a]]b]", []string{"a]b"}},
		{"double quotes double", `SELECT 1 AS "a""b"`, []string{`a"b`}},
		{"no-break space separates words", "SELECT 1 AS a , 2 AS b", []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cols, _, _, err := collect(t, c, driver.Query{Text: tc.text})
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, col := range cols {
				names = append(names, col.Name)
			}
			if !slices.Equal(names, tc.cols) {
				t.Errorf("columns %q, want %q", names, tc.cols)
			}
		})
	}
	// Two statements without ';' are a batch of two.
	db := admin(t)
	rows, err := db.Query("SELECT 1 SELECT 2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
	}
	if !rows.NextResultSet() {
		t.Error("SELECT 1 SELECT 2 returned one result set; the guard assumes two statements")
	}
}

// The guard's second layer: a write that reaches the read-only path
// without the allow-list (check nil) is rolled back, and a statement that
// ends the transaction is caught by the post-check.
func TestReadOnlyTransactionLayer(t *testing.T) {
	c := openMS(t, false)
	before := scalar(t, "SELECT COUNT(*) FROM tobira_big")
	if _, _, _, err := collectRun(t, c, driver.Query{Text: "DELETE FROM tobira_big WHERE n <= 10; SELECT 1 AS x"}, nil); err != nil {
		t.Fatalf("unchecked read-only write: %v", err)
	}
	if after := scalar(t, "SELECT COUNT(*) FROM tobira_big"); after != before {
		t.Fatalf("the read-only transaction kept a write: %d rows, want %d", after, before)
	}
	_, _, _, err := collectRun(t, c, driver.Query{Text: "COMMIT; SELECT 1 AS x"}, nil)
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("a statement ending the read-only transaction: want ErrReadOnly, got %v", err)
	}
	// The connection still works.
	if _, rows, _, err := collect(t, c, driver.Query{Text: "SELECT 1 AS x", ReadOnly: true}); err != nil || len(rows) != 1 {
		t.Errorf("after the post-check failure: %v", err)
	}
}

func collectRun(t *testing.T, c *conn, q driver.Query, check checker) ([]driver.Column, []driver.Row, driver.Stats, error) {
	t.Helper()
	rs, err := c.run(ctx30(t), q, true, check)
	if err != nil {
		return nil, nil, driver.Stats{}, err
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx30(t), rs, 0)
	return cols, rows, st, err
}

func TestReadOnlyRefusals(t *testing.T) {
	c := openMS(t, true)
	seq := scalar(t, "SELECT CAST(current_value AS bigint) FROM sys.sequences WHERE name = 'tobira_seq'")
	for _, text := range []string{
		"SELECT NEXT VALUE FOR tobira_seq",
		"EXEC xp_cmdshell 'dir'",
		"SELECT * INTO tobira_copy2 FROM tobira_people",
		"DELETE FROM tobira_big",
		"SELECT * FROM OPENROWSET('SQLNCLI', 'Server=x;Trusted_Connection=yes;', 'SELECT 1')",
	} {
		if _, _, _, err := collect(t, c, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q on a read-only conn: want ErrReadOnly, got %v", text, err)
		}
	}
	if got := scalar(t, "SELECT CAST(current_value AS bigint) FROM sys.sequences WHERE name = 'tobira_seq'"); got != seq {
		t.Errorf("sequence advanced: %d, want %d", got, seq)
	}
	_, rows, _, err := collect(t, c, driver.Query{Text: "EXEC sp_help 'dbo.tobira_people'"})
	if err != nil || len(rows) == 0 {
		t.Errorf("sp_help on a read-only conn: %d rows, %v", len(rows), err)
	}
	_, rows, _, err = collect(t, c, driver.Query{Text: "SELECT * FROM tobira_people ORDER BY id OFFSET 5 ROWS FETCH NEXT 3 ROWS ONLY"})
	if err != nil || len(rows) != 3 {
		t.Errorf("OFFSET … FETCH on a read-only conn: %d rows, %v", len(rows), err)
	}
}

func TestQueryTypes(t *testing.T) {
	c := openMS(t, false)
	cols, rows, _, err := collect(t, c, driver.Query{Text: "SELECT * FROM tobira_types"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("%d rows, %v", len(rows), err)
	}
	want := map[string]driver.Value{
		"id": driver.Int(1), "g": driver.String("01234567-89AB-CDEF-0123-456789ABCDEF"), "m": driver.Decimal("12.3456"),
		"r": driver.Float(0.1), "f": driver.Float(2.5), "t": driver.String("13:04:05.12"),
		"d":   driver.Time(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
		"dto": driver.Time(time.Date(2026, 1, 2, 2, 4, 5, 678000000, time.UTC)),
		"b":   driver.Bytes([]byte{1, 2}), "x": driver.String("<a/>"), "v": driver.Int(42), "tiny": driver.Int(255),
	}
	for i, col := range cols {
		w, ok := want[col.Name]
		if !ok {
			continue
		}
		if !drivertest.EqualValue(w, rows[0][i]) {
			t.Errorf("%s (%s): got %s (%s), want %s", col.Name, col.NativeType, rows[0][i], rows[0][i].Type(), w)
		}
	}
}

func TestQueryRowsAffectedAndCleanup(t *testing.T) {
	c := openMS(t, false)
	db := admin(t)
	mustExec(t, db, "CREATE TABLE tobira_affected (n INT)")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE tobira_affected") })
	mustExec(t, db, "INSERT INTO tobira_affected VALUES (1), (2), (3)")
	_, _, st, err := collect(t, c, driver.Query{Text: "DELETE FROM tobira_affected WHERE n < @p1", Params: []driver.Value{driver.Int(3)}})
	if err != nil || st.RowsAffected != 2 {
		t.Errorf("DELETE: rows affected %d, %v; want 2", st.RowsAffected, err)
	}
	// A transaction a statement leaves open is rolled back at its end.
	if _, _, _, err := collect(t, c, driver.Query{Text: "BEGIN TRAN; INSERT INTO tobira_affected VALUES (9)"}); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, "SELECT COUNT(*) FROM tobira_affected WITH (READPAST) WHERE n = 9"); n != 0 {
		t.Errorf("an open transaction's insert survived: %d", n)
	}
}

func TestQueryNamespaces(t *testing.T) {
	c := openMS(t, false)
	_, rows, _, err := collect(t, c, driver.Query{Text: "SELECT label FROM things", Namespace: "tobira_ns.dbo"})
	if err != nil || len(rows) != 1 || rows[0][0].String() != "in-ns" {
		t.Errorf("namespaced query: %v %v", rows, err)
	}
	_, _, _, err = collect(t, c, driver.Query{Text: "SELECT label FROM things", Namespace: "tobira_ns.dbo", ReadOnly: true})
	if err != nil {
		t.Errorf("read-only namespaced query: %v", err)
	}
	// The USE does not outlive the call.
	if _, _, _, err := collect(t, c, driver.Query{Text: "SELECT label FROM things"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("after a namespaced call: want ErrNotFound for things in the login database, got %v", err)
	}
}

func TestEarlyCloseIsQuick(t *testing.T) {
	c := openMS(t, false)
	for _, ro := range []bool{false, true} {
		start := time.Now()
		rs, err := c.Query(ctx30(t), driver.Query{Text: "SELECT a.n, REPLICATE('x', 4000) AS pad FROM tobira_big a CROSS JOIN tobira_big b", ReadOnly: ro})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := rs.Next(ctx30(t)); err != nil {
			t.Fatal(err)
		}
		if err := rs.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("closing a huge result early took %v", d)
		}
	}
	if _, rows, _, err := collect(t, c, driver.Query{Text: "SELECT 1 AS x"}); err != nil || len(rows) != 1 {
		t.Errorf("after early closes: %v", err)
	}
}

func TestCatalog(t *testing.T) {
	c := openMS(t, false)
	nss, err := c.Namespaces(ctx30(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ns := range nss {
		names = append(names, ns.Name)
		if ns.Name == "dbo" && !ns.Default {
			t.Error("dbo is not the default namespace")
		}
	}
	for _, want := range []string{"dbo", "sales", "tobira_ns.dbo", "[we.ird].[s.x]"} {
		if !slices.Contains(names, want) {
			t.Errorf("Namespaces lacks %q: %q", want, names)
		}
	}
	for _, unwanted := range []string{"sys", "INFORMATION_SCHEMA", "guest", "db_owner", "master.dbo"} {
		if slices.Contains(names, unwanted) {
			t.Errorf("Namespaces lists %q", unwanted)
		}
	}
	colls, err := c.Collections(ctx30(t), driver.NamespaceRef{Name: "dbo"})
	if err != nil {
		t.Fatal(err)
	}
	var view, people bool
	for _, col := range colls {
		switch col.Ref.Name {
		case "tobira_people_view":
			view = col.Type == driver.CollectionView && col.RowEstimate == -1
		case "tobira_people":
			people = col.Type == driver.CollectionTable && col.RowEstimate == 57
		}
	}
	if !view || !people {
		t.Errorf("Collections(dbo): view %v people %v: %+v", view, people, colls)
	}
	weird, err := c.Collections(ctx30(t), driver.NamespaceRef{Name: "[we.ird].[s.x]"})
	if err != nil || len(weird) != 1 || weird[0].Ref.Name != "t]1" || weird[0].Ref.Namespace != "[we.ird].[s.x]" {
		t.Errorf("Collections(we.ird.s.x) = %+v, %v", weird, err)
	}
	if _, rows, _, err := collectBrowse(t, c, weird[0].Ref); err != nil || len(rows) != 1 {
		t.Errorf("Browse of a weirdly named table: %v %v", rows, err)
	}

	s, err := c.Describe(ctx30(t), driver.CollectionRef{Namespace: "sales", Name: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref.Namespace != "sales" || s.Comment != "orders table" || !slices.Equal(s.PrimaryKey, []string{"id"}) || s.RowEstimate < 0 || s.SizeBytes < 0 {
		t.Errorf("schema = %+v", s)
	}
	fields := map[string]driver.Field{}
	for _, f := range s.Fields {
		fields[f.Name] = f
	}
	if f := fields["id"]; !f.AutoIncrement || f.Type != driver.TypeInt || f.Nullable {
		t.Errorf("id = %+v", f)
	}
	if f := fields["doubled"]; !f.Generated || !strings.Contains(f.Default, "[total]") {
		t.Errorf("doubled = %+v", f)
	}
	if f := fields["total"]; f.NativeType != "decimal(8,2)" || f.Type != driver.TypeDecimal || f.Default != "((0))" {
		t.Errorf("total = %+v", f)
	}
	if f := fields["note"]; f.NativeType != "nvarchar(50)" || f.Comment != "free text" || !f.Nullable {
		t.Errorf("note = %+v", f)
	}
	if f := fields["created"]; f.Type != driver.TypeTime || f.NativeType != "datetime2(7)" {
		t.Errorf("created = %+v", f)
	}
	if len(s.ForeignKeys) != 1 || s.ForeignKeys[0].Name != "fk_orders_person" || s.ForeignKeys[0].References != (driver.CollectionRef{Namespace: "dbo", Name: "tobira_people"}) {
		t.Errorf("foreign keys = %+v", s.ForeignKeys)
	}
	var ix []string
	for _, i := range s.Indexes {
		ix = append(ix, i.Name+":"+strings.Join(i.Fields, ",")+":"+i.Method)
	}
	if len(s.Indexes) != 3 || !s.Indexes[0].Primary || !slices.Contains(ix, "ix_orders_person_total:person_id,total:nonclustered") {
		t.Errorf("indexes = %q", ix)
	}
	if _, err := c.Describe(ctx30(t), driver.CollectionRef{Namespace: "nodb.dbo", Name: "x"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Describe in a missing database: %v", err)
	}
	other, err := c.Describe(ctx30(t), driver.CollectionRef{Namespace: "tobira_ns.dbo", Name: "things"})
	if err != nil || other.Ref.Namespace != "tobira_ns.dbo" || len(other.Fields) != 2 {
		t.Errorf("Describe in another database: %+v %v", other, err)
	}
}

func collectBrowse(t *testing.T, c *conn, ref driver.CollectionRef) ([]driver.Column, []driver.Row, driver.Stats, error) {
	t.Helper()
	rs, err := c.Browse(ctx30(t), ref, driver.BrowseOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, driver.Stats{}, err
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx30(t), rs, 0)
	return cols, rows, st, err
}

func TestBrowseRawFilterAndCompositeKeyset(t *testing.T) {
	c := openMS(t, false)
	db := admin(t)
	mustExec(t, db, "CREATE TABLE tobira_ck (a INT, b NVARCHAR(10), v INT, PRIMARY KEY (a, b))")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE tobira_ck") })
	mustExec(t, db, "INSERT INTO tobira_ck VALUES (1, N'a', 1), (1, N'b', 2), (2, N'a', 3), (2, N'c', 4), (3, N'a', 5)")
	ref := driver.CollectionRef{Name: "tobira_ck"}
	var seen []int64
	cursor := ""
	for range 5 {
		rs, err := c.Browse(ctx30(t), ref, driver.BrowseOptions{Limit: 2, Cursor: cursor, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		rows, st, err := driver.Collect(ctx30(t), rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			n, _ := r[2].AsInt()
			seen = append(seen, n)
		}
		if cursor = st.NextCursor; cursor == "" {
			break
		}
	}
	if !slices.Equal(seen, []int64{1, 2, 3, 4, 5}) {
		t.Errorf("composite keyset pages = %v", seen)
	}
	rs, err := c.Browse(ctx30(t), ref, driver.BrowseOptions{Filter: driver.Filter{Raw: "v > 2 -- trailing"}, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctx30(t), rs, 0)
	if err != nil || len(rows) != 3 {
		t.Errorf("raw filter: %d rows, %v", len(rows), err)
	}
	for _, raw := range []string{"1 = 1) DELETE FROM tobira_ck WHERE (1 = 1", "v > 0 --x\r) OR (1 = 1"} {
		if _, err := c.Browse(ctx30(t), ref, driver.BrowseOptions{Filter: driver.Filter{Raw: raw}}); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("raw %q: want ErrSyntax, got %v", raw, err)
		}
	}
	rs, err = c.Browse(ctx30(t), ref, driver.BrowseOptions{Filter: driver.Filter{Raw: "v IN (SELECT 1 UNION SELECT NEXT VALUE FOR tobira_seq)"}, ReadOnly: true})
	if err == nil {
		_ = rs.Close()
	}
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only browse with a sequence in the raw filter: want ErrReadOnly, got %v", err)
	}
}

func TestExplain(t *testing.T) {
	c := openMS(t, false)
	plan, err := c.Explain(ctx30(t), driver.Query{Text: "SELECT * FROM tobira_people WHERE id > @p1", Params: []driver.Value{driver.Int(3)}}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Text, "tobira_people") || plan.Root == nil || len(plan.Root.Children) == 0 {
		t.Errorf("plan = %q", plan.Text)
	}
	before := scalar(t, "SELECT COUNT(*) FROM tobira_big")
	plan, err = c.Explain(ctx30(t), driver.Query{Text: "DELETE FROM tobira_big", ReadOnly: true}, driver.ExplainOptions{})
	if err != nil || !strings.Contains(plan.Text, "Delete") {
		t.Errorf("explaining a DELETE: %v %v", plan, err)
	}
	if after := scalar(t, "SELECT COUNT(*) FROM tobira_big"); after != before {
		t.Fatalf("explaining a DELETE deleted rows: %d, want %d", after, before)
	}
	if _, err := c.Explain(ctx30(t), driver.Query{Text: "SELECT 1 SET SHOWPLAN_XML OFF DELETE FROM tobira_big", ReadOnly: true}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("explain smuggling SET: want ErrReadOnly, got %v", err)
	}
	if _, err := c.Explain(ctx30(t), driver.Query{Text: "SELECT 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("analyze: %v", err)
	}
	if _, err := c.Explain(ctx30(t), driver.Query{Text: "SELECT label FROM things", Namespace: "tobira_ns.dbo"}, driver.ExplainOptions{}); err != nil {
		t.Errorf("namespaced explain: %v", err)
	}
	// The connections used for plans are not left in SHOWPLAN mode.
	for range 4 {
		if _, rows, _, err := collect(t, c, driver.Query{Text: "SELECT 7 AS x"}); err != nil || len(rows) != 1 || rows[0][0].String() != "7" {
			t.Fatalf("after explain: %v %v", rows, err)
		}
	}
}
