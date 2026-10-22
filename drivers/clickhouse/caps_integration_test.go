//go:build integration

package clickhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
)

func TestNamespacesAndCollections(t *testing.T) {
	protocols(t, func(t *testing.T, dsn string) {
		c := openCH(t, dsn, false)
		nss, err := c.Namespaces(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, ns := range nss {
			names = append(names, ns.Name)
			if ns.Label != "database" || ns.Default != (ns.Name == testDB) {
				t.Errorf("namespace %+v", ns)
			}
		}
		for _, hidden := range []string{"system", "INFORMATION_SCHEMA", "information_schema"} {
			if slices.Contains(names, hidden) {
				t.Errorf("%s must be hidden: %v", hidden, names)
			}
		}
		if !slices.Contains(names, "tobira_ns") || !slices.Contains(names, testDB) {
			t.Errorf("namespaces = %v", names)
		}
		colls, err := c.Collections(ctxT(t), driver.NamespaceRef{})
		if err != nil {
			t.Fatal(err)
		}
		types := map[string]driver.CollectionType{}
		for _, col := range colls {
			types[col.Ref.Name] = col.Type
			if col.Ref.Namespace != testDB {
				t.Errorf("collection %+v: namespace", col)
			}
			if col.Ref.Name == "tobira_big" && col.RowEstimate != 1000 {
				t.Errorf("tobira_big row estimate = %d", col.RowEstimate)
			}
		}
		want := map[string]driver.CollectionType{
			"tobira_people": driver.CollectionTable, "tobira_people_view": driver.CollectionView,
			"tobira_mv": driver.CollectionMaterializedView, "tobira_memory": driver.CollectionTable,
		}
		for name, typ := range want {
			if types[name] != typ {
				t.Errorf("%s type = %q, want %q", name, types[name], typ)
			}
		}
		if _, err := c.Collections(ctxT(t), driver.NamespaceRef{Name: "tobira_no_such_db"}); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("missing database: %v", err)
		}
	})
}

func TestDescribe(t *testing.T) {
	c := openCH(t, testDSN, false)
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_events"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref.Namespace != testDB || s.Type != driver.CollectionTable || s.Comment != "events table" || !strings.Contains(s.DDL, "CREATE TABLE") {
		t.Errorf("schema header = %+v", s)
	}
	if s.PrimaryKey != nil {
		t.Errorf("a sorting key with an expression has no primary key columns, got %v", s.PrimaryKey)
	}
	byName := map[string]driver.Field{}
	for _, f := range s.Fields {
		byName[f.Name] = f
	}
	if f := byName["ts"]; f.Type != driver.TypeTime || f.NativeType != "DateTime64(3, 'UTC')" || f.Nullable {
		t.Errorf("ts = %+v", f)
	}
	if f := byName["kind"]; f.Type != driver.TypeString {
		t.Errorf("kind = %+v", f)
	}
	if f := byName["n"]; f.Default != "0" || f.Generated {
		t.Errorf("n = %+v", f)
	}
	if f := byName["doubled"]; !f.Generated || f.Default != "n * 2" {
		t.Errorf("doubled = %+v", f)
	}
	if f := byName["note"]; !f.Nullable || f.Comment != "free text" {
		t.Errorf("note = %+v", f)
	}
	var idx []string
	for _, i := range s.Indexes {
		idx = append(idx, i.Name+":"+strings.Join(i.Fields, "|")+":"+i.Method)
	}
	if want := []string{"primary:toDate(ts)|kind:sparse", "idx_kind:kind:set"}; !slices.Equal(idx, want) {
		t.Errorf("indexes = %v, want %v", idx, want)
	}
	s, err = c.Describe(ctxT(t), driver.CollectionRef{Namespace: "tobira_ns", Name: "things"})
	if err != nil || !slices.Equal(s.PrimaryKey, []string{"id"}) {
		t.Fatalf("things: %+v, %v", s, err)
	}
	// AI context: the schema must stay JSON-serialisable.
	if _, err := json.Marshal(s); err != nil {
		t.Errorf("schema is not JSON-serialisable: %v", err)
	}
}

func TestQueryTypes(t *testing.T) {
	protocols(t, func(t *testing.T, dsn string) {
		c := openCH(t, dsn, true)
		rows, _, cols := collect(t, c, driver.Query{Text: `SELECT
			toUInt64(18446744073709551615) AS u64, toInt128('-170141183460469231731687303715884105728') AS i128,
			toDecimal64(12.5, 2) AS dec, toUUID('6ba7b810-9dad-11d1-80b4-00c04fd430c8') AS id,
			toIPv4('10.0.0.1') AS ip, ['a', 'b'] AS arr, map('k', 1) AS m, tuple(1, 'x') AS tup,
			CAST(NULL AS Nullable(Int32)) AS nul, toLowCardinality('lc') AS lc,
			CAST('b', 'Enum8(\'a\' = 1, \'b\' = 2)') AS e, toDateTime64('2026-01-02 03:04:05.123', 3, 'UTC') AS dt,
			toDate('2026-01-02') AS d, true AS b, 1.5::Float32 AS f, '{"a":[1,2]}'::JSON AS j, unhex('ff00') AS raw`})
		if len(rows) != 1 {
			t.Fatalf("rows = %d", len(rows))
		}
		got := map[string]driver.Value{}
		for i, col := range cols {
			got[col.Name] = rows[0][i]
			if col.Type != columnType(col.NativeType) {
				t.Errorf("%s column type %v for %s", col.Name, col.Type, col.NativeType)
			}
		}
		want := map[string]string{
			"u64": "decimal 18446744073709551615", "i128": "decimal -170141183460469231731687303715884105728",
			"dec": "decimal 12.50", "id": "string 6ba7b810-9dad-11d1-80b4-00c04fd430c8", "ip": "string 10.0.0.1",
			"arr": `json ["a","b"]`, "m": `json {"k":1}`, "nul": "null NULL", "lc": "string lc", "e": "string b",
			"dt": "time 2026-01-02T03:04:05.123Z", "d": "time 2026-01-02T00:00:00Z", "b": "bool true", "f": "float 1.5",
			"j": `json {"a":[1,2]}`, "raw": `bytes \xff00`,
		}
		for name, w := range want {
			v := got[name]
			if s := v.Type().String() + " " + v.String(); s != w {
				t.Errorf("%s = %s, want %s", name, s, w)
			}
		}
		if v := got["tup"]; v.Type() != driver.TypeJSON {
			t.Errorf("tuple = %s %s, want JSON", v.Type(), v)
		}
		// AI tools: every value must stay JSON-serialisable.
		for name, v := range got {
			if _, err := json.Marshal(v.String()); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

func TestQueryNamespaceLimitAndParams(t *testing.T) {
	protocols(t, func(t *testing.T, dsn string) {
		c := openCH(t, dsn, true)
		rows, _, _ := collect(t, c, driver.Query{Text: "SELECT label FROM things", Namespace: "tobira_ns"})
		if len(rows) != 1 || rows[0][0].String() != "in-ns" {
			t.Errorf("namespaced query = %v", rows)
		}
		rows, st, _ := collect(t, c, driver.Query{Text: "SELECT n FROM tobira_big ORDER BY n", Limit: 10})
		if len(rows) != 10 || !st.Truncated {
			t.Errorf("limit: %d rows, truncated %v", len(rows), st.Truncated)
		}
		rows, st, _ = collect(t, c, driver.Query{Text: "SELECT n FROM tobira_big WHERE n < 3", Limit: 3})
		if len(rows) != 3 || st.Truncated {
			t.Errorf("exact limit: %d rows, truncated %v", len(rows), st.Truncated)
		}
		rows, _, _ = collect(t, c, driver.Query{Text: "SELECT name FROM tobira_people WHERE id = ? AND name != ?",
			Params: []driver.Value{driver.Int(1), driver.String("it's; DROP")}})
		if len(rows) != 1 {
			t.Errorf("params: %v", rows)
		}
		if err := queryErr(t, c, driver.Query{Text: "SELECT * FROM tobira_nope"}); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("missing table: %v", err)
		}
		if err := queryErr(t, c, driver.Query{Text: "SELECT FROM WHERE"}); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("syntax: %v", err)
		}
	})
}

func TestEarlyCloseCancels(t *testing.T) {
	protocols(t, func(t *testing.T, dsn string) {
		c := openCH(t, dsn, true)
		rs, err := c.Query(ctxT(t), driver.Query{Text: "SELECT number FROM system.numbers"})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := rs.Next(ctxT(t)); !ok || err != nil {
			t.Fatalf("first row: %v %v", ok, err)
		}
		start := time.Now()
		if err := rs.Close(); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("closing an endless result took %v", d)
		}
		if err := c.Ping(ctxT(t)); err != nil {
			t.Errorf("ping after early close: %v", err)
		}
	})
}

func TestBrowse(t *testing.T) {
	c := openCH(t, testDSN, false)
	ref := driver.CollectionRef{Name: "tobira_people"}
	page := func(opts driver.BrowseOptions) ([]driver.Row, driver.Stats, error) {
		rs, err := c.Browse(ctxT(t), ref, opts)
		if err != nil {
			return nil, driver.Stats{}, err
		}
		return driver.Collect(ctxT(t), rs, 0)
	}
	rows, st, err := page(driver.BrowseOptions{Limit: 5, Filter: driver.Filter{Conditions: []driver.Condition{
		{Field: "name", Op: driver.OpPrefix, Value: driver.String("")},
		{Field: "ID", Op: driver.OpIn, Value: driver.List(driver.Int(3), driver.Int(4), driver.Int(5), driver.Int(6), driver.Int(7), driver.Int(8))},
	}}})
	if err != nil || len(rows) != 5 || st.NextCursor != "o:5" {
		t.Fatalf("filtered page: %d rows, cursor %q, %v", len(rows), st.NextCursor, err)
	}
	rows, st, err = page(driver.BrowseOptions{Limit: 5, Cursor: st.NextCursor, Filter: driver.Filter{Conditions: []driver.Condition{
		{Field: "id", Op: driver.OpIn, Value: driver.List(driver.Int(3), driver.Int(4), driver.Int(5), driver.Int(6), driver.Int(7), driver.Int(8))},
	}}})
	if err != nil || len(rows) != 1 || st.NextCursor != "" {
		t.Fatalf("last page: %d rows, cursor %q, %v", len(rows), st.NextCursor, err)
	}
	if id, _ := rows[0][0].AsInt(); id != 8 {
		t.Errorf("last page row id = %d, want 8 (ordered by the sorting key)", id)
	}
	rows, _, err = page(driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "age", Op: driver.OpIsNull}}}})
	if err != nil || len(rows) != 5 {
		t.Errorf("age is null: %d rows, %v", len(rows), err)
	}
	for _, bad := range []driver.BrowseOptions{
		{Cursor: "nope"},
		{Cursor: "o:-1"},
		{Filter: driver.Filter{Raw: "1) OR (1"}},
		{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: driver.OpIn, Value: driver.Int(1)}}}},
	} {
		if _, _, err := page(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("Browse(%+v): want ErrSyntax, got %v", bad, err)
		}
	}
	if _, _, err := page(driver.BrowseOptions{Sort: []driver.SortKey{{Field: "nope"}}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown sort field: %v", err)
	}
	if _, _, err := page(driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "id", Op: "regex", Value: driver.Int(1)}}}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("unknown op: %v", err)
	}
	// A table without a sorting key pages by every orderable column.
	rs, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_memory"}, driver.BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err = driver.Collect(ctxT(t), rs, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("memory table: %v %v", rows, err)
	}
	if a, _ := rows[0][0].AsInt(); a != 1 {
		t.Errorf("memory table not ordered by a: %v", rows)
	}
	if _, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "tobira_nope"}, driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing table: %v", err)
	}
}

func TestExplain(t *testing.T) {
	protocols(t, func(t *testing.T, dsn string) {
		c := openCH(t, dsn, false)
		plan, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM tobira_people WHERE id = 3", ReadOnly: true}, driver.ExplainOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Format != "text" || !strings.Contains(plan.Text, "ReadFromMergeTree") || !strings.Contains(plan.Text, "PrimaryKey") {
			t.Errorf("plan = %q", plan.Text)
		}
		_, err = c.Explain(ctxT(t), driver.Query{Text: "SELECT 1"}, driver.ExplainOptions{Analyze: true})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("analyze: %v", err)
		}
		_, err = c.Explain(ctxT(t), driver.Query{Text: "DROP TABLE tobira_people", ReadOnly: true}, driver.ExplainOptions{Analyze: true})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only analyze of a write: %v", err)
		}
		if _, err := c.Explain(ctxT(t), driver.Query{Text: "SELECT 1", Namespace: "tobira_ns"}, driver.ExplainOptions{}); err != nil {
			t.Errorf("namespaced explain: %v", err)
		}
	})
}

func TestExport(t *testing.T) {
	protocols(t, func(t *testing.T, dsn string) {
		c := openCH(t, dsn, false)
		var buf bytes.Buffer
		err := c.Export(ctxT(t), driver.Query{Text: "SELECT id, name, age, tags FROM tobira_people WHERE id IN (1, 11) ORDER BY id"}, "csv", &buf)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		if len(lines) != 3 || strings.Trim(lines[0], `"`) != `id","name","age","tags` && lines[0] != "id,name,age,tags" {
			t.Fatalf("export = %q", buf.String())
		}
		if !strings.Contains(lines[2], `\N`) {
			t.Errorf("NULL is not \\N: %q", lines[2])
		}
		for _, bad := range []struct {
			q      driver.Query
			format string
			kind   error
		}{
			{driver.Query{Text: "SELECT 1"}, "parquet", driver.ErrUnsupported},
			{driver.Query{Text: "SELECT 1", Limit: 5}, "csv", driver.ErrUnsupported},
			{driver.Query{Text: "SELECT 1 FORMAT JSON"}, "csv", driver.ErrUnsupported},
			{driver.Query{Text: "INSERT INTO tobira_people (id) VALUES (950)"}, "csv", driver.ErrReadOnly},
			{driver.Query{Text: "SELECT * FROM url('http://127.0.0.1:1/x', 'CSV', 'a Int32')"}, "csv", driver.ErrReadOnly},
		} {
			buf.Reset()
			if err := c.Export(ctxT(t), bad.q, bad.format, &buf); !errors.Is(err, bad.kind) {
				t.Errorf("Export(%q, %s): want %v, got %v", bad.q.Text, bad.format, bad.kind, err)
			}
		}
	})
}

func TestComplete(t *testing.T) {
	c := openCH(t, testDSN, true)
	got, err := c.Complete(ctxT(t), driver.CompletionRequest{Text: "select na from tobira_people", Cursor: 9})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got, func(x driver.Completion) bool { return x.Label == "name" && x.Kind == "field" }) {
		t.Errorf("completions = %+v", got)
	}
	c.Invalidate()
}

// TestOpenBadAuthHTTPDoesNotLeak covers clickhouse-go keeping the HTTP
// client of a connection whose hello failed.
func TestOpenBadAuthHTTPDoesNotLeak(t *testing.T) {
	before := goleak.IgnoreCurrent()
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testHTTPDSN, Password: driver.NewSecret("wrong-password")})
	if err == nil {
		_ = c.Close()
	}
	if !errors.Is(err, driver.ErrAuth) || strings.Contains(err.Error(), "wrong-password") {
		t.Errorf("bad auth = %v", err)
	}
	if err := goleak.Find(before); err != nil {
		t.Errorf("leak after a failed HTTP open: %v", err)
	}
}

func TestClosedConn(t *testing.T) {
	c := openCH(t, testDSN, false)
	_ = c.Close()
	if _, err := c.Query(ctxT(t), driver.Query{Text: "SELECT 1", Namespace: "tobira_ns"}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("query after close: %v", err)
	}
}
