//go:build integration

package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func pgTxExplainer(t *testing.T, tx driver.Tx) driver.Explainer {
	t.Helper()
	e, ok := driver.As[driver.Explainer](tx)
	if !ok {
		t.Fatal("the Tx is not an Explainer")
	}
	return e
}

// EXPLAIN inside a Tx runs on the Tx's connection (it sees a table the Tx
// created), never persists what ANALYZE executed, and a failed EXPLAIN does
// not abort the Tx.
func TestTxExplain(t *testing.T) {
	c := openPG(t, false)
	tbl := scratch(t, c)
	total := count(t, c, tbl)
	ctx := ctxT(t)
	tx, err := c.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e := pgTxExplainer(t, tx)
	if err := queryErr(t, tx, driver.Query{Text: "create temp table tx_only (id int)"}); err != nil {
		t.Fatal(err)
	}

	plan, err := e.Explain(ctx, driver.Query{Text: "select * from tx_only where id = $1", Params: []driver.Value{driver.Int(1)}}, driver.ExplainOptions{})
	if err != nil {
		t.Fatalf("Explain in Tx: %v", err)
	}
	if !strings.Contains(plan.Text, "tx_only") || plan.Root == nil {
		t.Errorf("plan = %+v", plan)
	}

	plan, err = e.Explain(ctx, driver.Query{Text: "delete from " + tbl}, driver.ExplainOptions{Analyze: true})
	if err != nil || plan.Format != "json" {
		t.Fatalf("Explain(Analyze delete): %v %+v", err, plan)
	}
	if n := txCount(t, tx, tbl); n != total {
		t.Errorf("rows inside the Tx = %d, want %d: EXPLAIN ANALYZE persisted its delete", n, total)
	}

	if _, err := e.Explain(ctx, driver.Query{Text: "select * from tobira_nope"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Explain(missing table): err = %v, want ErrNotFound", err)
	}
	if _, err := e.Explain(ctx, driver.Query{Text: "select 1", ReadOnly: true}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Explain(ReadOnly in a read-write Tx): err = %v, want ErrUnsupported", err)
	}
	// The Tx is still usable and still sees its temp table.
	if err := queryErr(t, tx, driver.Query{Text: "insert into tx_only values (1)"}); err != nil {
		t.Fatalf("Tx unusable after a failed EXPLAIN: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, c, tbl); n != total {
		t.Errorf("rows = %d, want %d", n, total)
	}
	if _, err := e.Explain(ctx, driver.Query{Text: "select 1"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Explain after Commit: err = %v, want ErrConnection", err)
	}
	requireIdle(t, c)
}

// A read-only Tx explains through the read-only guards.
func TestReadOnlyTxExplain(t *testing.T) {
	c := openPG(t, false)
	tbl := scratch(t, c)
	total := count(t, c, tbl)
	ctx := ctxT(t)
	tx, err := c.Begin(ctx, driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e := pgTxExplainer(t, tx)
	if plan, err := e.Explain(ctx, driver.Query{Text: "select * from " + tbl}, driver.ExplainOptions{Analyze: true}); err != nil || plan.Root == nil {
		t.Fatalf("Explain(Analyze select): %v %+v", err, plan)
	}
	if _, err := e.Explain(ctx, driver.Query{Text: "delete from " + tbl}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Explain(Analyze delete): err = %v, want ErrReadOnly", err)
	}
	path := "/tmp/tobira_txexplain_lo"
	if _, err := e.Explain(ctx, driver.Query{Text: "select lo_export(1, '" + path + "')"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Explain(Analyze lo_export): err = %v, want ErrReadOnly", err)
	}
	if fileExists(t, c, path) {
		t.Errorf("%s exists", path)
	}
	if err := queryErr(t, tx, driver.Query{Text: "select 1"}); err != nil {
		t.Errorf("Tx unusable after refused EXPLAINs: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, c, tbl); n != total {
		t.Errorf("rows = %d, want %d", n, total)
	}
}

func txCount(t *testing.T, q driver.Querier, table string) int64 {
	t.Helper()
	rs, err := q.Query(ctxT(t), driver.Query{Text: "select count(*) from " + table})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := rows[0][0].AsInt()
	return n
}
