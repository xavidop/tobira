package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func txExplainer(t *testing.T, tx driver.Tx) driver.Explainer {
	t.Helper()
	e, ok := driver.As[driver.Explainer](tx)
	if !ok {
		t.Fatal("the Tx is not an Explainer")
	}
	return e
}

// EXPLAIN inside a Tx runs on the Tx's connection, so it sees what the Tx
// made (a temp table here), and goes through Explain's guards.
func TestTxExplain(t *testing.T) {
	ctx := context.Background()
	c := openTemp(t, false)
	tx := begin(t, c, driver.TxOptions{})
	defer func() { _ = tx.Rollback(ctx) }()
	runTx(t, tx, driver.Query{Text: "CREATE TEMP TABLE tx_only (id INTEGER PRIMARY KEY, v TEXT)"})
	e := txExplainer(t, tx)

	plan, err := e.Explain(ctx, driver.Query{Text: "select * from tx_only where id = ?", Params: []driver.Value{driver.Int(1)}}, driver.ExplainOptions{})
	if err != nil {
		t.Fatalf("Explain in Tx: %v", err)
	}
	if !strings.Contains(plan.Text, "tx_only") || plan.Root == nil {
		t.Errorf("plan = %+v", plan)
	}
	if plan, err := e.Explain(ctx, driver.Query{Text: "insert into tx_only (v) values ('x')"}, driver.ExplainOptions{}); err != nil || plan.Text == "" {
		t.Errorf("Explain(insert): %v %+v", err, plan)
	}

	for text, kind := range map[string]error{
		"PRAGMA user_version = 7":     driver.ErrReadOnly,
		"COMMIT":                      driver.ErrReadOnly,
		"select 1; select 2":          driver.ErrSyntax,
		"create table nope (a)":       driver.ErrReadOnly,
		"attach ':memory:' as other2": driver.ErrReadOnly,
	} {
		if _, err := e.Explain(ctx, driver.Query{Text: text}, driver.ExplainOptions{}); !errors.Is(err, kind) {
			t.Errorf("Explain(%q): err = %v, want %v", text, err, kind)
		}
	}
	if _, err := e.Explain(ctx, driver.Query{Text: "select 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Explain(Analyze): err = %v, want ErrUnsupported", err)
	}
	// Nothing the guards refused ran, and the Tx is still usable.
	rows, _, _ := run(t, c, driver.Query{Text: "PRAGMA user_version"})
	if v, _ := rows[0][0].AsInt(); v != 0 {
		t.Errorf("user_version = %d: an explained PRAGMA ran", v)
	}
	runTx(t, tx, driver.Query{Text: "insert into tx_only (v) values ('y')"})

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Explain(ctx, driver.Query{Text: "select 1"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Explain after Rollback: err = %v, want ErrConnection", err)
	}
}

func TestReadOnlyTxExplain(t *testing.T) {
	ctx := context.Background()
	c := openTemp(t, false)
	tx := begin(t, c, driver.TxOptions{ReadOnly: true})
	defer func() { _ = tx.Rollback(ctx) }()
	e := txExplainer(t, tx)
	if plan, err := e.Explain(ctx, driver.Query{Text: "select * from tobira_people"}, driver.ExplainOptions{}); err != nil || plan.Text == "" {
		t.Fatalf("Explain(select): %v %+v", err, plan)
	}
	// As on the connection, a write is only compiled, never run.
	if _, err := e.Explain(ctx, driver.Query{Text: "delete from tobira_people"}, driver.ExplainOptions{}); err != nil {
		t.Errorf("Explain(delete): %v", err)
	}
	if _, err := e.Explain(ctx, driver.Query{Text: "PRAGMA query_only = 0"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Explain(pragma): err = %v, want ErrReadOnly", err)
	}
	rows, _, _ := run(t, c, driver.Query{Text: "select count(*) from tobira_people"})
	if n, _ := rows[0][0].AsInt(); n == 0 {
		t.Error("an explained delete ran")
	}
}
