package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// seedFile creates a seeded database file and returns its path.
func seedFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "people.db")
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "sqlite:" + path})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range drivertest.SQLSeed("sqlite") {
		rs, err := c.Query(context.Background(), driver.Query{Text: s})
		if err != nil {
			t.Fatalf("seed %.40s: %v", s, err)
		}
		_ = rs.Close()
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func openTemp(t *testing.T, readOnly bool) driver.Conn {
	t.Helper()
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "sqlite:" + seedFile(t), ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func run(t *testing.T, c driver.Conn, q driver.Query) ([]driver.Row, driver.Stats, []driver.Column) {
	t.Helper()
	rs, err := c.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("query %q: %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows, st, cols
}

func TestInfo(t *testing.T) {
	i := New().Info()
	if i.Name != "sqlite" || i.Kind != driver.KindSQL || i.Dialect != "sqlite" || len(i.Schemes) != 3 || len(i.Capabilities) != 6 || !driver.HasCapability(i, driver.CapReadOnly) {
		t.Fatalf("%+v", i)
	}
	if _, ok := driver.Default.Lookup("sqlite"); !ok {
		t.Error("not registered")
	}
}

func TestParseDSN(t *testing.T) {
	tests := []struct {
		dsn, want string
		ro        bool
		err       error
		msg       string
	}{
		{dsn: "sqlite:/tmp/a.db", want: "/tmp/a.db"},
		{dsn: "sqlite3:/tmp/a.db", want: "/tmp/a.db"},
		{dsn: "sqlite:///tmp/a.db", want: "/tmp/a.db"},
		{dsn: "file:/tmp/a.db?mode=rw", want: "/tmp/a.db"},
		{dsn: "file:/tmp/a.db?mode=rwc", want: "/tmp/a.db"},
		{dsn: "file:///tmp/a.db", want: "/tmp/a.db"},
		{dsn: "file:/data/app.db?mode=ro", want: "/data/app.db", ro: true},
		{dsn: "sqlite:/tmp/a.db?mode=ro", want: "/tmp/a.db", ro: true},
		{dsn: "sqlite:/tmp/a.db?cache=shared", err: driver.ErrUnsupported, msg: "cache"},
		{dsn: "file:/tmp/a.db?mode=ro&_pragma=x", err: driver.ErrUnsupported, msg: "_pragma"},
		{dsn: "file:/tmp/a.db?mode=bogus", err: driver.ErrUnsupported, msg: "bogus"},
		{dsn: "file:/tmp/a.db?mode=memory", err: driver.ErrUnsupported, msg: "in-memory"},
		{dsn: "sqlite::memory:", err: driver.ErrUnsupported, msg: "in-memory"},
		{dsn: "file::memory:", err: driver.ErrUnsupported, msg: "in-memory"},
		{dsn: "file::memory:?cache=shared", err: driver.ErrUnsupported},
		{dsn: "file:/tmp/a.db?mode=rw&mode=ro", err: driver.ErrUnsupported, msg: "mode"},
		{dsn: "sqlite:", err: driver.ErrSyntax},
		{dsn: "postgres://x", err: driver.ErrSyntax},
	}
	for _, tt := range tests {
		got, o, err := parseDSN(tt.dsn)
		if tt.err != nil {
			if !errors.Is(err, tt.err) || !strings.Contains(fmt.Sprint(err), tt.msg) {
				t.Errorf("%q: err = %v, want %v (%s)", tt.dsn, err, tt.err, tt.msg)
			}
			continue
		}
		if err != nil || got != tt.want || o.readOnly != tt.ro {
			t.Errorf("%q = %q, %+v, %v; want %q ro=%v", tt.dsn, got, o, err, tt.want, tt.ro)
		}
	}
	rel, _, err := parseDSN("sqlite:rel.db")
	if err != nil || !filepath.IsAbs(rel) {
		t.Errorf("relative -> %q %v", rel, err)
	}
}

func TestOpenBarePath(t *testing.T) {
	path := seedFile(t)
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rows, _, _ := run(t, c, driver.Query{Text: "select count(*) from tobira_people"})
	if n, _ := rows[0][0].AsInt(); n == 0 {
		t.Error("empty")
	}
}

func TestOpenModeROHasNoWriteHandle(t *testing.T) {
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "file:" + seedFile(t) + "?mode=ro"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if c.(*conn).rw != nil {
		t.Error("rw handle opened for mode=ro")
	}
}

func TestBusyTimeoutOption(t *testing.T) {
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "sqlite:" + seedFile(t), Options: map[string]string{"busy_timeout": "123"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rows, _, _ := run(t, c, driver.Query{Text: "pragma busy_timeout"})
	if n, _ := rows[0][0].AsInt(); n != 123 {
		t.Errorf("busy_timeout = %d", n)
	}
}

func TestOpenReadOnlyMissingFile(t *testing.T) {
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: "sqlite:" + filepath.Join(t.TempDir(), "none.db"), ReadOnly: true})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestOpenMemoryUnsupported(t *testing.T) {
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: "sqlite::memory:"})
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), "in-memory") {
		t.Fatalf("got %v", err)
	}
}

// globalPragmas are process-wide settings a read-only query must not change.
var globalPragmas = []string{"soft_heap_limit", "hard_heap_limit", "temp_store_directory"}

func TestReadOnlyCannotBeDisabled(t *testing.T) {
	const bom = "\xEF\xBB\xBF"
	for _, mode := range []string{"ro conn", "Query.ReadOnly"} {
		t.Run(mode, func(t *testing.T) {
			path := seedFile(t)
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "sqlite:" + path, ReadOnly: mode == "ro conn"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			ro := mode == "Query.ReadOnly"
			ctx := context.Background()
			dir := filepath.Dir(path)
			try := func(text string) error {
				rs, err := c.Query(ctx, driver.Query{Text: text, ReadOnly: ro})
				if err == nil {
					_, _, err = driver.Collect(ctx, rs, 0)
				}
				return err
			}
			globals := func() map[string]string {
				m := map[string]string{}
				for _, name := range globalPragmas {
					rows, _, _ := run(t, c, driver.Query{Text: "PRAGMA " + name, ReadOnly: ro})
					m[name] = fmt.Sprint(rows)
				}
				return m
			}
			before := globals()
			var files []string
			file := func(name string) string {
				f := filepath.Join(dir, name)
				files = append(files, f)
				return f
			}
			// Lifting query_only is allowed to fail or succeed; it must not matter.
			_ = try("PRAGMA query_only=0")
			denied := []string{
				// writes and DDL
				"insert into tobira_people(id,name,email) values (1000,'a','zz@x')",
				"drop table tobira_people",
				"create temp table tt(a)",
				"with c as (select 1) insert into tobira_people(id,name,email) select 1001,'b','b@x' from c",
				// file-creating statements
				"ATTACH '" + file("a1.db") + "' AS x",
				"  /* c */ attach database '" + file("a2.db") + "' as x",
				"/**/ATTACH '" + file("a3.db") + "' AS x",
				"-- c\n attach '" + file("a4.db") + "' as x",
				"(ATTACH '" + file("a5.db") + "' AS x)",
				"\vATTACH '" + file("a7.db") + "' AS x",
				bom + "ATTACH '" + file("a8.db") + "' AS x",
				bom + "EXPLAIN ATTACH '" + file("a9.db") + "' AS x",
				"EXPLAIN ATTACH '" + file("a10.db") + "' AS x",
				"EXPLAIN QUERY PLAN EXPLAIN ATTACH '" + file("a11.db") + "' AS x",
				"VACUUM INTO '" + file("v1.db") + "'",
				"\vVACUUM INTO '" + file("v2.db") + "'",
				bom + "VACUUM INTO '" + file("v3.db") + "'",
				"EXPLAIN VACUUM INTO '" + file("v4.db") + "'",
				"EXPLAIN--\nVACUUM INTO '" + file("v5.db") + "'",
				"VACUUM", "EXPLAIN REINDEX", "REINDEX", "ANALYZE", "DETACH x",
				// assigning pragmas, every lexical form
				"PRAGMA journal_mode=delete", "PRAGMA query_only=0", "PRAGMA query_only(0)", "pragma main.query_only = 0",
				`PRAGMA "query_only"=0`, `PRAGMA 'query_only'=0`, "PRAGMA `writable_schema`=1", "PRAGMA [journal_mode]='delete'",
				`PRAGMA "query""_only"=0`, `PRAGMA "soft""_heap_limit"=777`, `PRAGMA 'soft''_heap_limit'=778`,
				`PRAGMA main . "query_only" = 0`, `PRAGMA main . "soft_heap_limit" = 779`, `PRAGMA main."query_only"=0`, `PRAGMA "main"."query_only"=0`,
				`PRAGMA/**/soft_heap_limit=780`, `PRAGMA/**/"soft_heap_limit"=781`, "PRAGMA--x\nsoft_heap_limit=782",
				"PRAGMA main/**/./**/soft_heap_limit/**/=783", `PRAGMA SoFt_HeAp_LiMiT=785`, `PRAGMA [Soft_Heap_Limit]=787`,
				`PRAGMA "main".[soft_heap_limit]=788`, "PRAGMA `temp`.`soft_heap_limit`=789",
				"PRAGMA soft_heap_limit\v=790", "\vPRAGMA soft_heap_limit=791", `PRAGMA ma$in.soft_heap_limit=792`, `PRAGMA soft_heap_limit$=793`,
				`PRAGMA "ſoft_heap_limit"=794`, `PRAGMA soft_heap_limit＝795`, `PRAGMA ｓoft_heap_limit=796`,
				`PRAGMA "soft_heap_limit"=12345`, `PRAGMA "hard_heap_limit"=1`, `PRAGMA "temp_store_directory"='` + dir + `'`,
				`PRAGMA "table_info"=tobira_people`, `PRAGMA "table_ınfo"(tobira_people)`, "PRAGMA user_version(5)", `PRAGMA "user_version"=7`,
				"PRAGMA soft_heap_limit = 807;", ";PRAGMA soft_heap_limit=801", "\fPRAGMA soft_heap_limit=803",
				// EXPLAIN prefixes
				"EXPLAIN PRAGMA soft_heap_limit=797", "EXPLAIN PRAGMA hard_heap_limit=100000000",
				"EXPLAIN PRAGMA temp_store_directory='" + dir + "'", `explain query plan pragma "query_only"=0`,
				"/*c*/ EXPLAIN /*d*/ QUERY /*e*/ PLAN PRAGMA soft_heap_limit=797", "explain explain pragma soft_heap_limit=797",
				"EXPLAIN\fPRAGMA soft_heap_limit=804", "explain/*x*/pragma soft_heap_limit=805",
				"EXPLAIN QUERY/**/PLAN/**/PRAGMA soft_heap_limit=806", "EXPLAIN QUERY PRAGMA soft_heap_limit=808",
				"EXPLAIN\xc2\xa0PRAGMA soft_heap_limit=809", "EXPLAIN PLAN PRAGMA soft_heap_limit=810",
				"EXPLAIN QUERY PLAN QUERY PLAN PRAGMA soft_heap_limit=811",
				// UTF-8 BOM, which SQLite skips as whitespace
				bom + "PRAGMA soft_heap_limit=802", bom + "PRAGMA temp_store_directory='" + dir + "'", bom + "PRAGMA query_only=0",
				"EXPLAIN " + bom + "PRAGMA hard_heap_limit=100000000", "EXPLAIN QUERY PLAN " + bom + "PRAGMA soft_heap_limit=812",
				"EXPLAIN QUERY " + bom + "PLAN PRAGMA soft_heap_limit=813", "PRAGMA soft_heap_limit" + bom + "=901",
				"PRAGMA main" + bom + ".soft_heap_limit=902", "PRAGMA table_info" + bom + "=tobira_people",
				// argument-less pragmas outside the allow-list
				"PRAGMA optimize", "PRAGMA shrink_memory", `PRAGMA "shrink_memory"`, "PRAGMA wal_checkpoint", "PRAGMA incremental_vacuum",
				"PRAGMA integrity_check(tobira_people)", "PRAGMA database_list(x)", "PRAGMA table_info(tobira_people) x",
				// unparseable starts
				`PRAGMA "unterminated=0`, "PRAGMA =1", "PRAGMA", "", "   ", "-- only a comment", "\xc2\xa0SELECT 1", "1",
			}
			for _, text := range denied {
				if err := try(text); !errors.Is(err, driver.ErrReadOnly) {
					t.Errorf("%q: got %v, want ErrReadOnly", text, err)
				}
			}
			// WITH passes the gate; SQLite itself must refuse what follows.
			for _, text := range []string{
				"WITH x AS (SELECT 1) ATTACH '" + file("a6.db") + "' AS y",
				"select 1; PRAGMA soft_heap_limit=814",
			} {
				if err := try(text); err == nil {
					t.Errorf("%q: got nil, want an error", text)
				}
			}
			for _, f := range files {
				if _, err := os.Stat(f); err == nil {
					t.Errorf("%s was created", f)
				}
			}
			if after := globals(); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Errorf("process-global pragmas changed: before %v, after %v", before, after)
			}
			allowed := []string{
				"SELECT 1", "select * from tobira_people order by id", "  /* c */ select count(*) from tobira_people;",
				"WITH c AS (SELECT 1 AS n) SELECT n FROM c", "VALUES (1), (2)", "EXPLAIN SELECT 1",
				"EXPLAIN QUERY PLAN SELECT * FROM tobira_people", bom + "SELECT 1", "EXPLAIN " + bom + "SELECT 1",
				"PRAGMA table_info(tobira_people)", `PRAGMA "table_info"(tobira_people)`, `PRAGMA main."table_info"(tobira_people)`,
				"PRAGMA main.table_info('tobira_people');", "PRAGMA index_list(tobira_people)", "PRAGMA foreign_key_list(tobira_people)",
				"PRAGMA database_list", "PRAGMA query_only", "PRAGMA soft_heap_limit", bom + "PRAGMA user_version",
			}
			for _, text := range allowed {
				if err := try(text); err != nil {
					t.Errorf("%q: %v", text, err)
				}
			}
			rows, _, _ := run(t, c, driver.Query{Text: "select name from tobira_people where id = ?", Params: []driver.Value{driver.Int(1)}, ReadOnly: ro})
			if len(rows) != 1 {
				t.Errorf("parameterised select rows = %v", rows)
			}
			rows, _, _ = run(t, c, driver.Query{Text: "select count(*) from tobira_people", ReadOnly: ro})
			if n, _ := rows[0][0].AsInt(); int(n) != len(drivertest.People.Rows) {
				t.Errorf("count = %d", n)
			}
		})
	}
}

func TestQueryReadOnlyOnWritableConn(t *testing.T) {
	c := openTemp(t, false)
	_, err := c.Query(context.Background(), driver.Query{Text: "delete from tobira_people", ReadOnly: true})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("got %v", err)
	}
	rows, _, _ := run(t, c, driver.Query{Text: "select count(*) from tobira_people"})
	if n, _ := rows[0][0].AsInt(); int(n) != len(drivertest.People.Rows) {
		t.Errorf("count = %d", n)
	}
}

func TestMultipleStatementsRejected(t *testing.T) {
	c := openTemp(t, false)
	tests := []struct {
		text string
		ok   bool
	}{
		{"select 1; select 2", false},
		{"select 1;", true},
		{"select 1; -- trailing", true},
		{"select 1; /* c */ ", true},
		{"select ';' ; ", true},
		{`select ";", 'a;b' as [;]`, true},
		{"select 1 /* ; select 2 */", true},
		{"select 1 -- ; select\n", true},
		{"select 1;\nselect 2", false},
		{"select 1; ;", false},
	}
	for _, tt := range tests {
		rs, err := c.Query(context.Background(), driver.Query{Text: tt.text})
		if tt.ok {
			if err != nil {
				t.Errorf("%q: %v", tt.text, err)
			} else {
				_ = rs.Close()
			}
			continue
		}
		if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), "multiple statements") {
			t.Errorf("%q: got %v", tt.text, err)
		}
	}
}

func TestQueryPeopleTypes(t *testing.T) {
	c := openTemp(t, true)
	rows, st, cols := run(t, c, driver.Query{Text: "select id, name, score, active, created_at, tags, age from tobira_people order by id"})
	if len(rows) != len(drivertest.People.Rows) || st.RowsAffected != -1 {
		t.Fatalf("rows=%d stats=%+v", len(rows), st)
	}
	want := []driver.Type{driver.TypeInt, driver.TypeString, driver.TypeDecimal, driver.TypeBool, driver.TypeTime, driver.TypeJSON, driver.TypeInt}
	for i, w := range want {
		if cols[i].Type != w {
			t.Errorf("col %s type = %v want %v", cols[i].Name, cols[i].Type, w)
		}
	}
	if cols[2].NativeType != "DECIMAL(10,2)" {
		t.Errorf("native = %q", cols[2].NativeType)
	}
	for _, r := range rows {
		for i, w := range want {
			if !r[i].IsNull() && r[i].Type() != w {
				t.Fatalf("row %v col %d: %v want %v", r[0], i, r[i].Type(), w)
			}
		}
	}
}

func TestQueryLimitTruncates(t *testing.T) {
	c := openTemp(t, true)
	rows, st, _ := run(t, c, driver.Query{Text: "select id from tobira_people order by id", Limit: 5})
	if len(rows) != 5 || !st.Truncated {
		t.Fatalf("rows=%d truncated=%v", len(rows), st.Truncated)
	}
	rows, st, _ = run(t, c, driver.Query{Text: "select id from tobira_people limit 3", Limit: 5})
	if len(rows) != 3 || st.Truncated {
		t.Fatalf("rows=%d truncated=%v", len(rows), st.Truncated)
	}
}

func TestQueryParams(t *testing.T) {
	c := openTemp(t, true)
	rows, _, _ := run(t, c, driver.Query{Text: "select name from tobira_people where id = ?", Params: []driver.Value{driver.Int(1)}})
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
}

func TestQueryRowsAffected(t *testing.T) {
	c := openTemp(t, false)
	_, st, cols := run(t, c, driver.Query{Text: "update tobira_people set age = 1 where id <= 3"})
	if len(cols) != 0 || st.RowsAffected != 3 {
		t.Fatalf("cols=%d stats=%+v", len(cols), st)
	}
}

func TestQueryCancel(t *testing.T) {
	c := openTemp(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	rs, err := c.Query(ctx, driver.Query{Text: "select id from tobira_people"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	cancel()
	if _, _, err := rs.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: "select 1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("query: %v", err)
	}
}

func TestCatalog(t *testing.T) {
	c := openTemp(t, true)
	ctx := context.Background()
	nss, err := c.Namespaces(ctx)
	if err != nil || len(nss) == 0 || nss[0].Name != "main" || !nss[0].Default || nss[0].Label != "database" {
		t.Fatalf("%+v %v", nss, err)
	}
	cols, err := c.Collections(ctx, driver.NamespaceRef{Name: "main"})
	if err != nil || len(cols) != 1 || cols[0].Ref.Name != "tobira_people" || cols[0].Type != driver.CollectionTable || cols[0].RowEstimate != -1 {
		t.Fatalf("%+v %v", cols, err)
	}
	s, err := c.Describe(ctx, driver.CollectionRef{Namespace: "main", Name: "tobira_people"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Fields) != 8 || len(s.PrimaryKey) != 1 || s.PrimaryKey[0] != "id" || !strings.HasPrefix(s.DDL, "CREATE TABLE") {
		t.Fatalf("%+v", s)
	}
	if f := s.Fields[2]; f.Name != "email" || f.Nullable || f.Type != driver.TypeString {
		t.Errorf("email field %+v", f)
	}
	if f := s.Fields[4]; f.Type != driver.TypeDecimal || f.NativeType != "DECIMAL(10,2)" || !f.Nullable {
		t.Errorf("score field %+v", f)
	}
	var unique bool
	for _, ix := range s.Indexes {
		if ix.Unique && len(ix.Fields) == 1 && ix.Fields[0] == "email" {
			unique = true
		}
	}
	if !unique {
		t.Errorf("no unique email index: %+v", s.Indexes)
	}
	if _, err := c.Describe(ctx, driver.CollectionRef{Name: "missing"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := c.Collections(ctx, driver.NamespaceRef{Name: "nope"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing ns: %v", err)
	}
}

func TestDescribeForeignKeysAndView(t *testing.T) {
	c := openTemp(t, false)
	for _, s := range []string{
		`create table "we""ird" (id integer primary key)`,
		`create table child (a integer, b integer, pid integer references "we""ird"(id), primary key (b, a))`,
		`create view v as select * from child`,
	} {
		run(t, c, driver.Query{Text: s})
	}
	s, err := c.Describe(context.Background(), driver.CollectionRef{Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ForeignKeys) != 1 || s.ForeignKeys[0].References.Name != `we"ird` || s.ForeignKeys[0].RefFields[0] != "id" {
		t.Errorf("fks %+v", s.ForeignKeys)
	}
	if len(s.PrimaryKey) != 2 || s.PrimaryKey[0] != "b" || s.PrimaryKey[1] != "a" {
		t.Errorf("pk %v", s.PrimaryKey)
	}
	cols, _ := c.Collections(context.Background(), driver.NamespaceRef{Name: "main"})
	var view bool
	for _, col := range cols {
		view = view || col.Ref.Name == "v" && col.Type == driver.CollectionView
	}
	if !view {
		t.Errorf("view missing: %+v", cols)
	}
	if _, err := c.Describe(context.Background(), driver.CollectionRef{Name: `we"ird`}); err != nil {
		t.Errorf("quoted name: %v", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	c := openTemp(t, false)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Query(ctx, driver.Query{Text: "select 1"}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("query: %v", err)
	}
	if err := c.Ping(ctx); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("ping: %v", err)
	}
	if _, err := c.Namespaces(ctx); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("ns: %v", err)
	}
}

func TestExpressionColumnPerValueType(t *testing.T) {
	c := openTemp(t, true)
	rows, _, _ := run(t, c, driver.Query{Text: "select nullif(column2,0) as b from (values (1,0),(2,x'ff00'),(3,2.5))"})
	if len(rows) != 3 || !rows[0][0].IsNull() || rows[1][0].Type() != driver.TypeBytes || rows[2][0].Type() != driver.TypeFloat {
		t.Fatalf("%v", rows)
	}
}

func TestCatalogUsesReadOnlyHandle(t *testing.T) {
	c := openTemp(t, false)
	ctx := context.Background()
	other := filepath.Join(t.TempDir(), "other.db")
	run(t, c, driver.Query{Text: "ATTACH '" + other + "' AS other"})
	run(t, c, driver.Query{Text: "create table other.t (a)"})
	nss, err := c.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range nss {
		if ns.Name == "other" {
			t.Fatalf("Namespaces lists a database attached by an earlier query: %+v", nss)
		}
	}
	if _, err := c.Collections(ctx, driver.NamespaceRef{Name: "other"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Collections(other) err = %v, want ErrNotFound", err)
	}
	if _, err := c.Describe(ctx, driver.CollectionRef{Namespace: "other", Name: "t"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Describe(other.t) err = %v, want ErrNotFound", err)
	}
}
