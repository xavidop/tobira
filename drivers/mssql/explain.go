package mssql

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Explain returns the estimated plan of q: the driver turns on SET
// SHOWPLAN_XML, under which SQL Server compiles each statement and returns
// its plan instead of running it, and parses the XML into the plan tree.
// Plan.Text is that tree rendered as indented text. It never runs the
// statement, so Analyze (an actual plan) is ErrUnsupported.
//
// The statement must pass explainTarget on every connection: one SELECT,
// WITH, INSERT, UPDATE, DELETE or MERGE statement without SET (which still
// runs under SHOWPLAN and could turn it off), GO, EXEC or transaction
// control. As a second layer the plan is taken inside a transaction that
// is always rolled back, on a dedicated connection that is discarded if
// SHOWPLAN cannot be turned off again. Data changes can be explained on
// read-only calls too, since they are only planned.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if opts.Analyze {
		return nil, errNoAnalyze()
	}
	if err := explainCheck(c.readOnly || q.ReadOnly)(q.Text); err != nil {
		return nil, err
	}
	args, err := toArgs(q.Params)
	if err != nil {
		return nil, err
	}
	use, err := c.useSQL(q.Namespace)
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
	if _, err := sc.ExecContext(ctx, prefixSQL(use, beginWritableSQL, "BEGIN TRANSACTION")); err != nil {
		return nil, mapError(ctx, op, err)
	}
	docs, err := showplan(ctx, sc, q.Text, args)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	var n int64
	if err := sc.QueryRowContext(ectx, endReadOnlySQL).Scan(&n); err != nil || n != 1 {
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errors.New("cannot verify the explain transaction; the connection was discarded"))
	}
	discard = false
	return buildPlan(docs)
}

var _ driver.Explainer = (*msTx)(nil)

// Explain returns the estimated plan of q inside the transaction, on its
// connection, so it sees what the transaction made (temporary tables,
// uncommitted rows). The statement passes the same explainTarget check as on
// the connection and is only planned, never run: Analyze is ErrUnsupported.
// If SHOWPLAN cannot be turned off again, the Tx is dead.
func (t *msTx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if opts.Analyze {
		return nil, errNoAnalyze()
	}
	if err := explainCheck(t.readOnly || q.ReadOnly)(q.Text); err != nil {
		return nil, err
	}
	args, err := toArgs(q.Params)
	if err != nil {
		return nil, err
	}
	use, err := t.c.useSQL(q.Namespace)
	if err != nil {
		return nil, err
	}
	sc, ctx, release, err := t.use(ctx, op, false)
	if err != nil {
		return nil, err
	}
	defer release()
	if use != "" {
		if _, err := sc.ExecContext(ctx, use); err != nil {
			return nil, mapError(ctx, op, err)
		}
	}
	docs, err := showplan(ctx, sc, q.Text, args)
	// Whatever showplan returned, prove the transaction is still open: a
	// failed plan can have aborted it.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	openErr := t.proveOpen(pctx)
	cancel()
	var off *showplanOffError
	if errors.As(err, &off) || openErr != nil {
		t.mu.Lock()
		t.dead = true
		t.mu.Unlock()
	}
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if openErr != nil {
		return nil, openErr
	}
	return buildPlan(docs)
}

func errNoAnalyze() error {
	return driver.Wrap(driver.ErrUnsupported, Name, "explain", errors.New("an actual plan runs the statement; tobira's mssql driver only shows the estimated plan"))
}

// explainCheck is explainTarget with a kind: ErrReadOnly on a read-only
// path, ErrUnsupported otherwise.
func explainCheck(ro bool) checker {
	kind := driver.ErrUnsupported
	if ro {
		kind = driver.ErrReadOnly
	}
	return func(text string) error {
		if err := explainTarget(text); err != nil {
			return driver.Wrap(kind, Name, "explain", err)
		}
		return nil
	}
}

// showplanOffError is a failure to turn SHOWPLAN_XML off again: the
// session would keep planning instead of running statements.
type showplanOffError struct{ err error }

func (e *showplanOffError) Error() string {
	return "cannot turn SHOWPLAN_XML off: " + e.err.Error()
}
func (e *showplanOffError) Unwrap() error { return e.err }

// showplan runs text under SET SHOWPLAN_XML ON on sc and returns the plan
// documents, one per result row. SHOWPLAN is always turned off again; that
// runs on a bounded context of its own, so a cancelled caller cannot leave
// it on.
func showplan(ctx context.Context, sc *sql.Conn, text string, args []any) (docs []string, err error) {
	if _, err := sc.ExecContext(ctx, "SET SHOWPLAN_XML ON"); err != nil {
		return nil, err
	}
	defer func() {
		octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		if _, offErr := sc.ExecContext(octx, "SET SHOWPLAN_XML OFF"); offErr != nil {
			docs, err = nil, &showplanOffError{offErr}
		}
	}()
	// Under SHOWPLAN an sp_executesql call returns no plan, so parameters
	// become declared variables of the batch: the plan is compiled for
	// unknown values, which is what a parameterised statement gets anyway.
	rows, err := sc.QueryContext(ctx, declareParams(args)+text)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for {
		for rows.Next() {
			var doc string
			if err := rows.Scan(&doc); err != nil {
				return nil, err
			}
			docs = append(docs, doc)
		}
		if !rows.NextResultSet() {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, errors.New("SQL Server returned no plan")
	}
	return docs, nil
}

// xmlNode is a generic element of a showplan document.
type xmlNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Children []xmlNode  `xml:",any"`
}

func (n *xmlNode) attr(name string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// buildPlan parses the showplan documents into one tree: a Statement node
// per statement (StmtSimple), each with its RelOp operator tree.
func buildPlan(docs []string) (*driver.Plan, error) {
	root := &driver.PlanNode{Label: "plan", Rows: -1}
	for _, doc := range docs {
		var n xmlNode
		dec := xml.NewDecoder(strings.NewReader(doc))
		// go-mssqldb has already decoded the server's UTF-16 into a Go
		// string; a declared encoding="utf-16" no longer applies.
		dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
		if err := dec.Decode(&n); err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "explain", fmt.Errorf("cannot parse the plan: %w", err))
		}
		collectStatements(&n, root)
	}
	if len(root.Children) == 0 {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "explain", errors.New("cannot parse the plan: no statements"))
	}
	if len(root.Children) == 1 {
		root = root.Children[0]
	}
	var b strings.Builder
	renderPlan(&b, root, 0)
	return &driver.Plan{Format: "text", Text: strings.TrimRight(b.String(), "\n"), Root: root}, nil
}

// collectStatements adds a node for every statement element under n.
func collectStatements(n *xmlNode, parent *driver.PlanNode) {
	if strings.HasPrefix(n.XMLName.Local, "Stmt") {
		st := &driver.PlanNode{
			Label:  "Statement",
			Detail: strings.Join(strings.Fields(n.attr("StatementText")), " "),
			Cost:   parseFloat(n.attr("StatementSubTreeCost")),
			Rows:   parseRows(n.attr("StatementEstRows")),
		}
		for i := range n.Children {
			collectRelOps(&n.Children[i], st)
		}
		parent.Children = append(parent.Children, st)
		return
	}
	for i := range n.Children {
		collectStatements(&n.Children[i], parent)
	}
}

// collectRelOps adds a node for every top-most RelOp under n; a RelOp's own
// children are the RelOps nested in it.
func collectRelOps(n *xmlNode, parent *driver.PlanNode) {
	if n.XMLName.Local != "RelOp" {
		for i := range n.Children {
			collectRelOps(&n.Children[i], parent)
		}
		return
	}
	op := &driver.PlanNode{
		Label: n.attr("PhysicalOp"),
		Cost:  parseFloat(n.attr("EstimatedTotalSubtreeCost")),
		Rows:  parseRows(n.attr("EstimateRows")),
	}
	var detail []string
	if logical := n.attr("LogicalOp"); logical != "" && logical != op.Label {
		detail = append(detail, logical)
	}
	if obj := relOpObject(n); obj != "" {
		detail = append(detail, obj)
	}
	op.Detail = strings.Join(detail, " ")
	for i := range n.Children {
		collectRelOps(&n.Children[i], op)
	}
	parent.Children = append(parent.Children, op)
}

// relOpObject names the table or index a RelOp reads: the first Object
// element of its operator, without descending into nested RelOps.
func relOpObject(n *xmlNode) string {
	for i := range n.Children {
		ch := &n.Children[i]
		if ch.XMLName.Local == "RelOp" {
			continue
		}
		if ch.XMLName.Local == "Object" {
			var parts []string
			for _, a := range []string{"Database", "Schema", "Table", "Index"} {
				if v := ch.attr(a); v != "" {
					parts = append(parts, v)
				}
			}
			return strings.Join(parts, ".")
		}
		if obj := relOpObject(ch); obj != "" {
			return obj
		}
	}
	return ""
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func parseRows(s string) int64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return -1
	}
	return int64(math.Round(f))
}

// renderPlan writes n and its children as an indented tree.
func renderPlan(b *strings.Builder, n *driver.PlanNode, depth int) {
	b.WriteString(strings.Repeat("  ", depth))
	if depth > 0 {
		b.WriteString("-> ")
	}
	b.WriteString(n.Label)
	if n.Detail != "" {
		b.WriteString("  " + n.Detail)
	}
	var est []string
	if n.Cost > 0 {
		est = append(est, "cost="+strconv.FormatFloat(n.Cost, 'g', 6, 64))
	}
	if n.Rows >= 0 {
		est = append(est, "rows="+strconv.FormatInt(n.Rows, 10))
	}
	if len(est) > 0 {
		b.WriteString("  (" + strings.Join(est, " ") + ")")
	}
	b.WriteByte('\n')
	for _, ch := range n.Children {
		renderPlan(b, ch, depth+1)
	}
}

// declareParams declares @p1, @p2 … with the SQL type of each argument, for
// a statement planned without its parameter values.
func declareParams(args []any) string {
	if len(args) == 0 {
		return ""
	}
	decl := make([]string, len(args))
	for i, a := range args {
		typ := "nvarchar(max)"
		switch a.(type) {
		case int64:
			typ = "bigint"
		case float64:
			typ = "float"
		case bool:
			typ = "bit"
		case []byte:
			typ = "varbinary(max)"
		case time.Time:
			typ = "datetimeoffset"
		}
		decl[i] = "@p" + strconv.Itoa(i+1) + " " + typ
	}
	return "DECLARE " + strings.Join(decl, ", ") + "; "
}
