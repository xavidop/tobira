package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivers/sqlite"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

// sqliteToolset is a Toolset over a temp SQLite file seeded with People and
// an orders table that references it.
func sqliteToolset(t *testing.T) (*Toolset, *session.Manager) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.db")
	c, err := sqlite.New().Open(context.Background(), driver.ConnConfig{DSN: "sqlite:" + path})
	if err != nil {
		t.Fatal(err)
	}
	stmts := append(drivertest.SQLSeed("sqlite"),
		"create table orders (id integer primary key, person_id integer references tobira_people(id), status text default 'open')",
		"insert into orders (person_id, status) values (3, 'open'), (4, 'paid')")
	for _, s := range stmts {
		rs, err := c.Query(context.Background(), driver.Query{Text: s})
		if err != nil {
			t.Fatalf("seed %.40s: %v", s, err)
		}
		_ = rs.Close()
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reg := driver.NewRegistry()
	if err := reg.Add(sqlite.New()); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "lite", Driver: "sqlite", DSN: "sqlite:" + path}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	return NewToolset(ToolDeps{Manager: mgr, Policy: RowsAlways}), mgr
}

func TestSQLiteWriteViaQueryIsRefused(t *testing.T) {
	ts, mgr := sqliteToolset(t)
	for _, q := range []string{
		"delete from tobira_people",
		"update orders set status = 'x'",
		"select 1; delete from orders",
		"with d as (delete from orders returning id) select * from d",
		"drop table orders",
	} {
		out, err := ts.Query(ctx, QueryInput{Connection: "lite", Query: q})
		if err != nil {
			t.Fatal(err)
		}
		if out.Error == "" || out.Rows != nil {
			t.Errorf("write %q via query was not refused: %+v", q, out)
		}
	}
	c, err := mgr.Open(ctx, "lite")
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"tobira_people": 57, "orders": 2} {
		rs, err := c.Query(ctx, driver.Query{Text: "select * from " + table})
		if err != nil {
			t.Fatal(err)
		}
		rows, _, err := driver.Collect(ctx, rs, 0)
		if err != nil || len(rows) != want {
			t.Fatalf("%s has %d rows (%v), want %d", table, len(rows), err, want)
		}
	}
	// The plain read-only refusal names read-only.
	out, _ := ts.Query(ctx, QueryInput{Connection: "lite", Query: "delete from orders"})
	if !strings.Contains(strings.ToLower(out.Error), "read-only") && !strings.Contains(strings.ToLower(out.Error), "read only") {
		t.Errorf("refusal = %q", out.Error)
	}
}

func TestSQLiteReadTools(t *testing.T) {
	ts, _ := sqliteToolset(t)
	d, err := ts.Describe(ctx, DescribeInput{Connection: "lite", Collection: "orders"})
	if err != nil || d.Error != "" {
		t.Fatal(err, d.Error)
	}
	if len(d.ForeignKeys) != 1 || d.ForeignKeys[0].References != "main.tobira_people" {
		t.Fatalf("foreign keys = %+v", d.ForeignKeys)
	}
	if strings.Contains(string(mustJSON(t, d)), "CREATE TABLE") || strings.Contains(string(mustJSON(t, d)), "create table") {
		t.Fatal("describe carries DDL")
	}
	s, _ := ts.Sample(ctx, SampleInput{Connection: "lite", Collection: "orders", Limit: 5})
	if s.Error != "" || string(s.Rows) != `[[1,3,"open"],[2,4,"paid"]]` {
		t.Fatalf("sample = %+v %s", s, s.Rows)
	}
	x, _ := ts.Explain(ctx, ExplainInput{Connection: "lite", Query: "select * from orders where id = 1"})
	if x.Error != "" || x.Plan == "" {
		t.Fatalf("explain = %+v", x)
	}
}
