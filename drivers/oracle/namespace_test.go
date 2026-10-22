package oracle

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	go_ora "github.com/sijms/go-ora/v2"

	"github.com/xavidop/tobira/driver"
)

func TestCheckSchemaName(t *testing.T) {
	for _, ok := range []string{"TOBIRA", "tobira_lower", "A B", "ÄÖ", "x$#"} {
		if err := checkSchemaName("query", ok); err != nil {
			t.Errorf("checkSchemaName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", `x"; drop table t; --`, `"`, "a\x00b", "a\xffb"} {
		err := checkSchemaName("query", bad)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkSchemaName(%q) = %v, want ErrSyntax", bad, err)
		}
	}
}

// A namespace that could change the ALTER SESSION statement is refused
// before any session is taken, on every path that sets CURRENT_SCHEMA.
func TestNamespaceInjectionRefused(t *testing.T) {
	db := sql.OpenDB(go_ora.NewConnector("oracle://u:p@127.0.0.1:1/FREE"))
	c := &conn{db: db, version: "23.26.3.0.0", user: "TOBIRA"}
	t.Cleanup(func() { _ = db.Close() })
	ns := `TOBIRA" ; ALTER SESSION SET "X`
	ctx := context.Background()
	for _, ro := range []bool{false, true} {
		if _, err := c.Query(ctx, driver.Query{Text: "select 1 from dual", Namespace: ns, ReadOnly: ro}); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("Query (ro %v): want ErrSyntax, got %v", ro, err)
		}
		if _, err := c.Explain(ctx, driver.Query{Text: "select 1 from dual", Namespace: ns, ReadOnly: ro}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("Explain (ro %v): want ErrSyntax, got %v", ro, err)
		}
	}
	tx := &oraTx{c: c}
	if _, err := tx.Query(ctx, driver.Query{Text: "select 1 from dual", Namespace: ns}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Tx.Query: want ErrSyntax, got %v", err)
	}
	if _, err := tx.Explain(ctx, driver.Query{Text: "select 1 from dual", Namespace: ns}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Tx.Explain: want ErrSyntax, got %v", err)
	}
	if st := db.Stats(); st.OpenConnections != 0 {
		t.Errorf("a session was opened: %+v", st)
	}
}
