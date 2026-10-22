package oracle

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

const savepointExplain = "tobira_explain"

// planRowsSQL reads the plan rows EXPLAIN PLAN wrote, for Plan.Root.
const planRowsSQL = `
SELECT id, parent_id, operation, options, object_owner, object_name, cost, cardinality
FROM plan_table WHERE statement_id = :1 ORDER BY id`

// planTextSQL renders the plan as DBMS_XPLAN prints it, for Plan.Text.
const planTextSQL = `SELECT plan_table_output FROM TABLE(DBMS_XPLAN.DISPLAY('PLAN_TABLE', :1, 'TYPICAL'))`

// Explain returns the estimated plan of q. It runs EXPLAIN PLAN SET
// STATEMENT_ID … INTO PLAN_TABLE FOR q, reads the plan back (DBMS_XPLAN for
// Plan.Text, the PLAN_TABLE rows for Plan.Root) and rolls back, so the plan
// rows vanish. EXPLAIN PLAN only parses and optimizes the statement; it
// never runs it, so Analyze is ErrUnsupported.
//
// The target must pass explainTarget on every connection: one SELECT, WITH,
// INSERT, UPDATE, DELETE or MERGE statement, without PL/SQL, database links
// or side-effect packages. Oracle refuses EXPLAIN PLAN inside a read-only
// transaction (it inserts into PLAN_TABLE), so a read-only call (read-only
// connection or q.ReadOnly) still runs it in an ordinary transaction that is
// always rolled back; there the target check is what keeps it read-only,
// and its refusal is ErrReadOnly.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if opts.Analyze {
		return nil, errNoAnalyze()
	}
	if q.Namespace != "" {
		if err := checkSchemaName(op, q.Namespace); err != nil {
			return nil, err
		}
	}
	ro := c.readOnly || q.ReadOnly
	if ro && !guardVerified(c.version) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, unverifiedServer(c.version))
	}
	text, err := explainCheck(q.Text, ro)
	if err != nil {
		return nil, err
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	discard := true
	defer func() {
		if discard {
			discardConn(sc)
		}
		_ = sc.Close()
	}()
	tx, err := sc.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if q.Namespace != "" {
		if _, err := tx.ExecContext(ctx, "ALTER SESSION SET CURRENT_SCHEMA = "+quoteIdent(q.Namespace)); err != nil {
			_ = tx.Rollback()
			return nil, mapError(ctx, op, err)
		}
	}
	plan, err := explainIn(ctx, tx, text)
	rbErr := tx.Rollback()
	if err != nil {
		return nil, err
	}
	if rbErr == nil && q.Namespace != "" {
		_, rbErr = sc.ExecContext(ctx, "ALTER SESSION SET CURRENT_SCHEMA = "+quoteIdent(c.user))
	}
	discard = rbErr != nil
	return plan, nil
}

var _ driver.Explainer = (*oraTx)(nil)

// Explain returns the plan of q inside the transaction, on its session, so
// it sees what the transaction made. The plan rows are written under a
// savepoint and rolled back to it, which leaves the transaction as it was.
// Oracle cannot EXPLAIN PLAN in a read-only transaction, so a read-only Tx
// returns ErrUnsupported. A per-query ReadOnly cannot be honoured in a
// read-write Tx, as in Query.
func (t *oraTx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if opts.Analyze {
		return nil, errNoAnalyze()
	}
	if t.readOnly {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("an EXPLAIN PLAN cannot run inside an Oracle read-only transaction"))
	}
	if q.ReadOnly {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("a read-only query cannot run inside a read-write transaction: begin a read-only one"))
	}
	if q.Namespace != "" {
		if err := checkSchemaName(op, q.Namespace); err != nil {
			return nil, err
		}
	}
	text, err := explainCheck(q.Text, false)
	if err != nil {
		return nil, err
	}
	cctx, release, err := t.claim(ctx, op, false)
	if err != nil {
		return nil, err
	}
	defer release()
	if _, err := t.tx.ExecContext(cctx, "SAVEPOINT "+savepointExplain); err != nil {
		return nil, mapError(ctx, op, err)
	}
	schemaSet := q.Namespace != ""
	var plan *driver.Plan
	if schemaSet {
		_, err = t.tx.ExecContext(cctx, "ALTER SESSION SET CURRENT_SCHEMA = "+quoteIdent(q.Namespace))
		err = mapError(ctx, op, err)
	}
	if err == nil {
		plan, err = explainIn(cctx, t.tx, text)
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endTimeout)
	defer cancel()
	_, rbErr := t.tx.ExecContext(rctx, "ROLLBACK TO SAVEPOINT "+savepointExplain)
	if rbErr != nil {
		t.mu.Lock()
		t.dead = true
		t.mu.Unlock()
	}
	if derr := t.statementDone(ctx, schemaSet); err == nil {
		err = derr
	}
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func errNoAnalyze() error {
	return driver.Wrap(driver.ErrUnsupported, Name, "explain", errors.New("analyze is not supported: EXPLAIN PLAN only estimates, and tobira's oracle driver never runs the statement to explain it"))
}

// explainCheck checks the target with explainTarget, its refusal being
// ErrReadOnly on a read-only path and ErrUnsupported otherwise, and returns
// it without a trailing ';'.
func explainCheck(text string, ro bool) (string, error) {
	kind := driver.ErrUnsupported
	if ro {
		kind = driver.ErrReadOnly
	}
	if err := explainTarget(text); err != nil {
		return "", driver.Wrap(kind, Name, "explain", err)
	}
	return stripSemicolon(text), nil
}

// stripSemicolon drops a trailing ';' (and what follows it, which the
// target check proved is only space and comments).
func stripSemicolon(text string) string {
	end := len(text)
	sqlscan.ScanOracle(text, func(t sqlscan.Token) bool {
		if t.Kind == sqlscan.Semicolon {
			end = t.Start
			return false
		}
		return true
	})
	return text[:end]
}

// explainIn plans text on tx and reads the plan back.
func explainIn(ctx context.Context, tx *sql.Tx, text string) (*driver.Plan, error) {
	const op = "explain"
	id, err := statementID()
	if err != nil {
		return nil, driver.Wrap(nil, Name, op, err)
	}
	if _, err := tx.ExecContext(ctx, "EXPLAIN PLAN SET STATEMENT_ID = '"+id+"' INTO PLAN_TABLE FOR "+text); err != nil {
		return nil, mapError(ctx, op, err)
	}
	rows, err := tx.QueryContext(ctx, planTextSQL, id)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	var lines []string
	for rows.Next() {
		var line sql.NullString
		if err := rows.Scan(&line); err != nil {
			_ = rows.Close()
			return nil, mapError(ctx, op, err)
		}
		lines = append(lines, line.String)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	root, err := planTree(ctx, tx, id)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	return &driver.Plan{Format: "text", Text: strings.TrimRight(strings.Join(lines, "\n"), "\n "), Root: root}, nil
}

// statementID is a fresh EXPLAIN PLAN statement id.
func statementID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "tobira_" + hex.EncodeToString(b[:]), nil
}

// planTree builds the PlanNode tree from the PLAN_TABLE rows (id, parent_id).
func planTree(ctx context.Context, tx *sql.Tx, id string) (*driver.PlanNode, error) {
	rows, err := tx.QueryContext(ctx, planRowsSQL, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	nodes := map[int64]*driver.PlanNode{}
	var root *driver.PlanNode
	for rows.Next() {
		var nid int64
		var parent, cost, card sql.NullInt64
		var operation, options, owner, object sql.NullString
		if err := rows.Scan(&nid, &parent, &operation, &options, &owner, &object, &cost, &card); err != nil {
			return nil, err
		}
		n := &driver.PlanNode{Label: strings.TrimSpace(operation.String + " " + options.String), Rows: -1}
		if object.String != "" {
			n.Detail = object.String
			if owner.String != "" {
				n.Detail = owner.String + "." + object.String
			}
		}
		if cost.Valid {
			n.Cost = float64(cost.Int64)
		}
		if card.Valid {
			n.Rows = card.Int64
		}
		nodes[nid] = n
		if p, ok := nodes[parent.Int64]; parent.Valid && ok {
			p.Children = append(p.Children, n)
		} else if root == nil {
			root = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("EXPLAIN PLAN wrote no plan rows")
	}
	return root, nil
}
