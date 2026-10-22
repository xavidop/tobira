package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
)

// SampleInput is the input of sample.
type SampleInput struct {
	Connection string `json:"connection" jsonschema:"description=Connection name from list_connections"`
	Namespace  string `json:"namespace,omitempty" jsonschema:"description=Namespace; empty means the connection default"`
	Collection string `json:"collection" jsonschema:"description=Collection (table or view) name"`
	Limit      int    `json:"limit,omitempty" jsonschema:"description=Rows to read; capped at the configured maximum"`
}

// QueryInput is the input of query.
type QueryInput struct {
	Connection string `json:"connection" jsonschema:"description=Connection name from list_connections"`
	Query      string `json:"query" jsonschema:"description=A read-only query in the connection's dialect"`
	Limit      int    `json:"limit,omitempty" jsonschema:"description=Rows to return; capped at the configured maximum"`
}

// ExplainInput is the input of explain.
type ExplainInput struct {
	Connection string `json:"connection" jsonschema:"description=Connection name from list_connections"`
	Query      string `json:"query" jsonschema:"description=The query to explain; it is not run"`
}

// ExecuteInput is the input of execute.
type ExecuteInput struct {
	Connection string `json:"connection" jsonschema:"description=Connection name from list_connections"`
	Statement  string `json:"statement" jsonschema:"description=The statement to run after the user confirms it"`
}

// Column is one result column; Type is the native type when the driver
// knows it.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// RowsOutput is the result of sample, query and execute. When the row
// policy withholds the rows, Rows is absent and RowsWithheld and Note say
// how many and why.
type RowsOutput struct {
	outcome
	Columns      []Column        `json:"columns,omitempty"`
	Rows         json.RawMessage `json:"rows,omitempty"`
	Truncated    bool            `json:"truncated"`
	RowsWithheld int             `json:"rows_withheld,omitempty"`
	Note         string          `json:"note,omitempty"`
	// RowsAffected is set when the statement returned no columns and the
	// driver reports the rows it affected (execute).
	RowsAffected *int64 `json:"rows_affected,omitempty"`

	count int // rows the call returned, sent or not
}

func (o *RowsOutput) rowCount() int {
	if o.Error != "" {
		return -1
	}
	return o.count
}

// ExplainOutput is the result of explain. The plan counts as one row for
// the row policy, since a plan may include literals.
type ExplainOutput struct {
	outcome
	Format       string `json:"format,omitempty"`
	Plan         string `json:"plan,omitempty"`
	RowsWithheld int    `json:"rows_withheld,omitempty"`
	Note         string `json:"note,omitempty"`
}

func (o *ExplainOutput) rowCount() int {
	if o.Error != "" {
		return -1
	}
	return 1
}

// Sample reads the first rows of a collection through Browse, read-only.
func (t *Toolset) Sample(ctx context.Context, in SampleInput) (RowsOutput, error) {
	in.Connection = t.scopedConn(ctx, in.Connection)
	return run(t, ctx, NameSample, in.Connection, in, func(call, turn context.Context) (RowsOutput, error) {
		c, err := t.openReadable(call, in.Connection)
		if err != nil {
			return RowsOutput{}, err
		}
		b, ok := driver.As[driver.Browser](c)
		if !ok {
			return RowsOutput{}, fmt.Errorf("connection %q cannot browse collections: use query", in.Connection)
		}
		lim := t.limit(in.Limit)
		ref := driver.CollectionRef{Namespace: in.Namespace, Name: in.Collection}
		text := fmt.Sprintf("sample %s limit %d", qualified(ref), lim)
		return t.rows(call, turn, in.Connection, text, t.consentWhat(in.Connection, NameSample, qualified(ref)), lim, func(ctx context.Context) (driver.ResultSet, error) {
			return b.Browse(ctx, ref, driver.BrowseOptions{Limit: lim, ReadOnly: true})
		})
	})
}

// Query runs a read-only query.
func (t *Toolset) Query(ctx context.Context, in QueryInput) (RowsOutput, error) {
	in.Connection = t.scopedConn(ctx, in.Connection)
	return run(t, ctx, NameQuery, in.Connection, in, func(call, turn context.Context) (RowsOutput, error) {
		if strings.TrimSpace(in.Query) == "" {
			return RowsOutput{}, errors.New("query is empty")
		}
		c, err := t.openReadable(call, in.Connection)
		if err != nil {
			return RowsOutput{}, err
		}
		lim := t.limit(in.Limit)
		return t.rows(call, turn, in.Connection, in.Query, t.consentWhat(in.Connection, NameQuery, in.Query), lim, func(ctx context.Context) (driver.ResultSet, error) {
			return c.Query(ctx, driver.Query{Text: in.Query, Limit: lim, ReadOnly: true})
		})
	})
}

// Explain returns the plan of a read-only query, without ANALYZE.
func (t *Toolset) Explain(ctx context.Context, in ExplainInput) (ExplainOutput, error) {
	in.Connection = t.scopedConn(ctx, in.Connection)
	return run(t, ctx, NameExplain, in.Connection, in, func(call, turn context.Context) (ExplainOutput, error) {
		if strings.TrimSpace(in.Query) == "" {
			return ExplainOutput{}, errors.New("query is empty")
		}
		c, err := t.openReadable(call, in.Connection)
		if err != nil {
			return ExplainOutput{}, err
		}
		x, ok := driver.As[driver.Explainer](c)
		if !ok {
			return ExplainOutput{}, fmt.Errorf("connection %q cannot explain queries", in.Connection)
		}
		start := time.Now()
		plan, err := x.Explain(call, driver.Query{Text: in.Query, ReadOnly: true}, driver.ExplainOptions{})
		if err == nil && plan == nil {
			err = errors.New("the driver returned no plan")
		}
		err = rowData(err)
		var n int64
		if err == nil {
			n = 1
		}
		t.record(call, in.Connection, "explain "+in.Query, err == nil, n, time.Since(start))
		if err != nil {
			return ExplainOutput{}, err
		}
		out := ExplainOutput{Format: plan.Format}
		if out.Format == "" {
			out.Format = "text"
		}
		send, note, err := t.allowRows(turn, in.Connection, 1, t.consentWhat(in.Connection, NameExplain, in.Query))
		if err != nil {
			return ExplainOutput{}, err
		}
		if send {
			out.Plan = truncate(t.redact(in.Connection, plan.Text), maxPlanRunes)
		} else {
			out.RowsWithheld, out.Note = 1, note
		}
		return out, nil
	})
}

// Exec runs a statement that may write. It refuses unless the turn's
// context came from WithExecute, the connection is not read-only, and the
// user confirms this very statement through Events.ConfirmExecute.
func (t *Toolset) Exec(ctx context.Context, in ExecuteInput) (RowsOutput, error) {
	in.Connection = t.scopedConn(ctx, in.Connection)
	return run(t, ctx, NameExecute, in.Connection, in, func(call, turn context.Context) (RowsOutput, error) {
		if !executeAllowed(turn) {
			return RowsOutput{}, errors.New("execute is not enabled for this request")
		}
		if strings.TrimSpace(in.Statement) == "" {
			return RowsOutput{}, errors.New("statement is empty")
		}
		c, err := t.open(call, in.Connection)
		if err != nil {
			return RowsOutput{}, err
		}
		if t.deps.Manager.State(in.Connection).ReadOnly {
			return RowsOutput{}, fmt.Errorf("connection %q is read-only: execute is refused", in.Connection)
		}
		ok, err := t.events(turn).ConfirmExec(turn, in.Connection, in.Statement)
		if err != nil {
			if cerr := turn.Err(); cerr != nil {
				return RowsOutput{}, cerr
			}
			if refusal, isRefusal := errors.AsType[*assistant.ExecuteRefusal](err); isRefusal {
				return RowsOutput{}, errors.New(refusal.Reason)
			}
			return RowsOutput{}, errors.New("the user declined to run the statement (confirmation failed)")
		}
		if !ok {
			return RowsOutput{}, errors.New("the user declined to run the statement")
		}
		// The read-only switch may have flipped while the dialog was open.
		ro := t.deps.Manager.State(in.Connection).ReadOnly
		if ro {
			return RowsOutput{}, fmt.Errorf("connection %q became read-only: execute is refused", in.Connection)
		}
		lim := t.maxRows
		return t.rows(call, turn, in.Connection, in.Statement, t.consentWhat(in.Connection, NameExecute, in.Statement), lim, func(ctx context.Context) (driver.ResultSet, error) {
			// ReadOnly is State.ReadOnly as just checked: the per-call
			// contract of the session, should the check above ever loosen.
			return c.Query(ctx, driver.Query{Text: in.Statement, Limit: lim, ReadOnly: ro})
		})
	})
}

// ResultRows prepares rows the user already has (a result tab, for
// explain_results) as a tool result would carry them: at most MaxRows rows,
// encoded, redacted and cut like tool output, and subject to the row policy
// and consent like any tool call, though it is not reported as one. stmt is
// the statement that produced them, named in the consent prompt; truncated
// says the rows are already a prefix of the result. The only error is the
// turn's cancellation.
func (t *Toolset) ResultRows(ctx context.Context, conn, stmt string, cols []driver.Column, rows []driver.Row, truncated bool) (RowsOutput, error) {
	if err := ctx.Err(); err != nil {
		return RowsOutput{}, err
	}
	if len(rows) > t.maxRows {
		rows, truncated = rows[:t.maxRows], true
	}
	out := RowsOutput{Truncated: truncated, count: len(rows), Columns: t.columns(conn, cols)}
	send, note, err := t.allowRows(ctx, conn, len(rows), t.consentWhat(conn, "explain_results", stmt))
	if err != nil {
		return RowsOutput{}, err
	}
	if send {
		out.Rows = t.encodeRows(conn, rows)
	} else {
		out.RowsWithheld, out.Note = len(rows), note
	}
	return out, nil
}

// ErrorText prepares the error of a statement the user ran (for fix_error)
// as the row policy allows, since its text may quote row values. When rows
// may be sent (always, or ask and the user consents to one row), it is the
// full redacted text. Otherwise err gets the form a tool error would get
// without consent (kind, fixed text, code), and text alone, with no driver
// error, is cut at its first quote with long digit runs masked. err nil
// means text is all there is. The result is cut like a tool error. The only
// error is the turn's cancellation.
func (t *Toolset) ErrorText(ctx context.Context, conn string, err error, text string) (string, error) {
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	full := text
	if err != nil {
		full = err.Error()
	}
	send, _, cerr := t.allowRows(ctx, conn, 1, whatErrorText)
	if cerr != nil {
		return "", cerr
	}
	switch {
	case send:
		return truncate(t.redact(conn, full), maxErrorRunes), nil
	case err != nil:
		return truncate(scrubDriverError(err, t.redactor(conn)), maxErrorRunes), nil
	default:
		return truncate(scrubError(t.redact(conn, text)), maxErrorRunes), nil
	}
}

// columns describes cols for the model, redacted.
func (t *Toolset) columns(conn string, cols []driver.Column) []Column {
	var out []Column
	for _, col := range cols {
		typ := col.NativeType
		if typ == "" {
			typ = col.Type.String()
		}
		out = append(out, Column{Name: t.text(conn, col.Name), Type: t.text(conn, typ)})
	}
	return out
}

// rows runs one statement, records it as text, and applies the row policy
// to what it returned, asking with what.
func (t *Toolset) rows(call, turn context.Context, conn, text, what string, lim int, exec func(context.Context) (driver.ResultSet, error)) (RowsOutput, error) {
	start := time.Now()
	var (
		cols []driver.Column
		rows []driver.Row
		st   driver.Stats
	)
	rs, err := exec(call)
	if err == nil {
		cols = rs.Columns()
		rows, st, err = driver.Collect(call, rs, lim)
	}
	err = rowData(err)
	n := int64(len(rows))
	if err == nil && len(cols) == 0 && st.RowsAffected >= 0 {
		n = st.RowsAffected
	}
	if err != nil {
		n = 0
	}
	t.record(call, conn, text, err == nil, n, time.Since(start))
	if err != nil {
		return RowsOutput{}, err
	}
	out := RowsOutput{Truncated: st.Truncated, count: len(rows), Columns: t.columns(conn, cols)}
	if len(cols) == 0 && st.RowsAffected >= 0 {
		ra := st.RowsAffected
		out.RowsAffected = &ra
	}
	send, note, err := t.allowRows(turn, conn, len(rows), what)
	if err != nil {
		return RowsOutput{}, err
	}
	if send {
		out.Rows = t.encodeRows(conn, rows)
	} else {
		out.RowsWithheld, out.Note = len(rows), note
	}
	return out, nil
}
