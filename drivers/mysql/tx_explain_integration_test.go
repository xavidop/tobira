//go:build integration

package mysql

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func myTxExplainer(t *testing.T, tx driver.Tx) driver.Explainer {
	t.Helper()
	e, ok := driver.As[driver.Explainer](tx)
	if !ok {
		t.Fatal("the Tx is not an Explainer")
	}
	return e
}

// EXPLAIN inside a Tx runs on the Tx's connection (it waits its turn behind
// an open result set), behind the connection Explain's checks.
func TestTxExplain(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	before := tableState(t, db, name)
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	e := myTxExplainer(t, tx)
	plan, err := e.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM " + name + " WHERE age > ?", Params: []driver.Value{driver.Int(1)}}, driver.ExplainOptions{})
	if err != nil {
		t.Fatalf("Explain in Tx: %v", err)
	}
	if plan.Format != "text" || !strings.Contains(plan.Text, name) || plan.Root == nil {
		t.Errorf("plan = %+v", plan)
	}
	rs, err := tx.Query(ctxT(t), driver.Query{Text: "SELECT * FROM " + name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Explain(ctxT(t), driver.Query{Text: "SELECT 1"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), "result sets is open") {
		t.Errorf("Explain while a Tx result set is open: want the Tx busy error, got %v", err)
	}
	_ = rs.Close()
	plan, err = e.Explain(ctxT(t), driver.Query{Text: "DELETE FROM " + name + " WHERE id = 1"}, driver.ExplainOptions{})
	if err != nil || !strings.Contains(plan.Text, "DELETE") {
		t.Errorf("Explain(DELETE): %v %+v", err, plan)
	}
	if _, err := e.Explain(ctxT(t), driver.Query{Text: "SELECT 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Analyze: want ErrUnsupported, got %v", err)
	}
	if _, err := e.Explain(ctxT(t), driver.Query{Text: "SELECT 1", ReadOnly: true}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("ReadOnly in a read-write Tx: want ErrUnsupported, got %v", err)
	}
	for _, text := range []string{"ANALYZE SELECT 1", "DROP TABLE " + name, "SELECT 1; DELETE FROM " + name, "SELECT /*! 1 */", "COMMIT"} {
		if _, err := e.Explain(ctxT(t), driver.Query{Text: text}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("Explain(%q): want ErrUnsupported, got %v", text, err)
		}
	}
	// The Tx is still open and usable.
	if err := queryErr(t, tx, driver.Query{Text: "DELETE FROM " + name + " WHERE id = 1"}); err != nil {
		t.Fatalf("Tx unusable after Explain: %v", err)
	}
	if err := tx.Rollback(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Explain(ctxT(t), driver.Query{Text: "SELECT 1"}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Explain after Rollback: want ErrConnection, got %v", err)
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("table changed: %s, was %s", got, before)
	}
}

// A read-only Tx explains a SELECT; explaining DML is ErrReadOnly from the
// server (1792), as on a read-only connection.
func TestReadOnlyTxExplain(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	before := tableState(t, db, name)
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{ReadOnly: true})
	e := myTxExplainer(t, tx)
	if plan, err := e.Explain(ctxT(t), driver.Query{Text: "SELECT * FROM " + name}, driver.ExplainOptions{}); err != nil || plan.Text == "" {
		t.Fatalf("Explain(SELECT): %v %+v", err, plan)
	}
	for _, text := range []string{"DELETE FROM " + name, "UPDATE " + name + " SET age = 1"} {
		_, err := e.Explain(ctxT(t), driver.Query{Text: text}, driver.ExplainOptions{})
		var de *driver.Error
		if !errors.Is(err, driver.ErrReadOnly) || !errors.As(err, &de) || de.Code != "1792" {
			t.Errorf("Explain(%q): want ErrReadOnly from the server (1792), got %v", text, err)
		}
	}
	if _, err := e.Explain(ctxT(t), driver.Query{Text: "DROP TABLE " + name}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Explain(DROP): want ErrReadOnly, got %v", err)
	}
	if err := queryErr(t, tx, driver.Query{Text: "SELECT 1"}); err != nil {
		t.Errorf("Tx unusable after refused EXPLAINs: %v", err)
	}
	if got := tableState(t, db, name); got != before {
		t.Errorf("table changed: %s, was %s", got, before)
	}
}
