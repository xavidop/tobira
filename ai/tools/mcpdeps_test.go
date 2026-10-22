package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/history"
)

// rows250 is a read-only query returning 250 rows.
const rows250 = "with recursive c(x) as (select 1 union all select x + 1 from c where x < 250) select x from c"

func rowsOf(t *testing.T, out RowsOutput) int {
	t.Helper()
	if out.Error != "" {
		t.Fatalf("tool error: %s", out.Error)
	}
	var rows [][]any
	if err := json.Unmarshal(out.Rows, &rows); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return len(rows)
}

// TestRowCapRaisesTheRowLimit: the MCP server allows up to 1000 rows
// (mcp.max_rows), above the assistant's 200.
func TestRowCapRaisesTheRowLimit(t *testing.T) {
	_, mgr := sqliteToolset(t)

	raised := NewToolset(ToolDeps{Manager: mgr, Policy: RowsAlways, MaxRows: 300, RowCap: 1000})
	out, err := raised.Query(ctx, QueryInput{Connection: "lite", Query: rows250})
	if err != nil {
		t.Fatal(err)
	}
	if n := rowsOf(t, out); n != 250 || out.Truncated {
		t.Errorf("RowCap 1000, MaxRows 300: %d rows, truncated %v; want 250, false", n, out.Truncated)
	}

	def := NewToolset(ToolDeps{Manager: mgr, Policy: RowsAlways, MaxRows: 300})
	out, err = def.Query(ctx, QueryInput{Connection: "lite", Query: rows250})
	if err != nil {
		t.Fatal(err)
	}
	if n := rowsOf(t, out); n != MaxRowsCap || !out.Truncated {
		t.Errorf("no RowCap: %d rows, truncated %v; want %d, true", n, out.Truncated, MaxRowsCap)
	}
}

type ctxProbeKey struct{}

// ctxRecorder records the probe value of every context it gets.
type ctxRecorder struct{ seen []any }

func (r *ctxRecorder) Record(ctx context.Context, _ history.Entry, _ driver.ConnConfig) error {
	r.seen = append(r.seen, ctx.Value(ctxProbeKey{}))
	return nil
}

// TestRecordGetsTheCallContext: the history recorder gets the values of the
// call's context (the MCP server puts the client name there), detached from
// its cancellation.
func TestRecordGetsTheCallContext(t *testing.T) {
	_, mgr := sqliteToolset(t)
	rec := &ctxRecorder{}
	ts := NewToolset(ToolDeps{Manager: mgr, Policy: RowsAlways, History: rec})
	c := context.WithValue(ctx, ctxProbeKey{}, "probe")
	if _, err := ts.Query(c, QueryInput{Connection: "lite", Query: "select 1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Explain(c, ExplainInput{Connection: "lite", Query: "select 1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Sample(c, SampleInput{Connection: "lite", Collection: "orders"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.seen) != 3 {
		t.Fatalf("recorded %d calls, want 3", len(rec.seen))
	}
	for i, v := range rec.seen {
		if v != "probe" {
			t.Errorf("record %d saw %v, want the call context's value", i, v)
		}
	}
}
