package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

// vecEnv is a Toolset over a Fake with the vector fixture (main.vecs),
// through the connection "vec", plus "plain", a Fake driver without
// VectorSearcher.
type vecEnv struct {
	*env
}

type vecOptions struct {
	policy   RowPolicy
	maxRows  int
	rowCap   int
	embedder driver.Embedder
}

func newVecEnv(t *testing.T, o vecOptions) *vecEnv {
	t.Helper()
	f := drivertest.NewFake(drivertest.WithVectors())
	plain := drivertest.NewFake(drivertest.WithVectors(), drivertest.WithoutCapabilities(driver.CapVectorSearcher))
	reg := driver.NewRegistry()
	if err := reg.Add(f.Driver()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(renamed{plain.Driver(), "plainfake"}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "vec", Driver: "fake", DSN: "fake://admin:" + dsnPassword + "@db.local/vec"},
		{Name: "plain", Driver: "plainfake", DSN: "fake://admin:" + dsnPassword + "@db.local/plain"},
	}}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false, session.WithEmbedder(o.embedder))
	t.Cleanup(func() { _ = mgr.CloseAll() })
	if o.policy == "" {
		o.policy = RowsAlways
	}
	e := &env{t: t, fake: f, mgr: mgr, hist: &fakeHistory{}}
	e.ts = NewToolset(ToolDeps{Manager: mgr, History: e.hist, Policy: o.policy, MaxRows: o.maxRows, RowCap: o.rowCap})
	e.ts.fallbackEvents = e.currentEvents
	return &vecEnv{e}
}

// renamed is a driver registered under another name, so two Fakes can
// share a registry.
type renamed struct {
	driver.Driver
	name string
}

func (r renamed) Info() driver.Info {
	i := r.Driver.Info()
	i.Name = r.name
	i.Schemes = []string{r.name}
	return i
}

func similarRows(t *testing.T, out RowsOutput) [][]any {
	t.Helper()
	if out.Error != "" {
		t.Fatalf("tool error: %s", out.Error)
	}
	var rows [][]any
	if err := json.Unmarshal(out.Rows, &rows); err != nil {
		t.Fatalf("rows %s: %v", out.Rows, err)
	}
	return rows
}

func colIndex(t *testing.T, out RowsOutput, name string) int {
	t.Helper()
	for i, c := range out.Columns {
		if c.Name == name {
			return i
		}
	}
	t.Fatalf("no column %q in %+v", name, out.Columns)
	return -1
}

// A vector query returns the nearest rows first with their _distance, and
// is recorded in history as "similar <collection> k=<k> vector dim=N".
func TestSimilarVector(t *testing.T) {
	e := newVecEnv(t, vecOptions{})
	out, err := e.ts.Similar(ctx, SimilarInput{Connection: "vec", Namespace: "main", Collection: drivertest.VectorCollection, Vector: []float32{1, 0, 0, 0}, K: 3, Metric: "l2"})
	if err != nil {
		t.Fatal(err)
	}
	rows := similarRows(t, out)
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3: %s", len(rows), out.Rows)
	}
	id, dist := colIndex(t, out, "id"), colIndex(t, out, "_distance")
	if rows[0][id] != float64(1) {
		t.Errorf("nearest id = %v, want 1 under l2", rows[0][id])
	}
	if d0, d1 := rows[0][dist].(float64), rows[1][dist].(float64); d0 > d1 {
		t.Errorf("_distance not ascending: %v then %v", d0, d1)
	}
	if v := rows[0][colIndex(t, out, "vec")]; v != "[0.9, 0.3, 0, 0]" {
		t.Errorf("vec = %v, want the display form", v)
	}
	h := e.hist.all()
	if len(h) != 1 || h[0].Source != history.SourceAI || h[0].Query != "similar main.vecs k=3 vector dim=4" || !h[0].OK || h[0].Rows != 3 || h[0].Conn != "vec" {
		t.Fatalf("history = %+v", h)
	}
	calls := e.toolCalls()
	if len(calls) != 1 || calls[0].Name != NameSimilar || calls[0].Rows != 3 || calls[0].Conn != "vec" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if in := calls[0].Input; !strings.Contains(in, "<vector dim=4>") || strings.Contains(in, "[1,0,0,0]") {
		t.Errorf("reported input = %s, want the vector summarised", in)
	}
	// The metric reaches the driver: id 3 wins under inner product.
	out, err = e.ts.Similar(ctx, SimilarInput{Connection: "vec", Collection: drivertest.VectorCollection, Vector: []float32{1, 0, 0, 0}, Metric: "ip"})
	if err != nil {
		t.Fatal(err)
	}
	if rows := similarRows(t, out); rows[0][colIndex(t, out, "id")] != float64(3) {
		t.Errorf("nearest under ip = %v, want 3", rows[0])
	}
}

// A text query is embedded with the session's embedder.
func TestSimilarTextWithEmbedder(t *testing.T) {
	e := newVecEnv(t, vecOptions{embedder: drivertest.HashEmbedder{Dims: 4}})
	out, err := e.ts.Similar(ctx, SimilarInput{Connection: "vec", Collection: drivertest.VectorCollection, Text: "red shoes"})
	if err != nil {
		t.Fatal(err)
	}
	if rows := similarRows(t, out); len(rows) != 6 {
		t.Fatalf("%d rows, want the 6 fixture rows (k defaults to 10)", len(rows))
	}
	if h := e.hist.all(); len(h) != 1 || h[0].Query != `similar vecs k=10 "red shoes"` {
		t.Fatalf("history = %+v", h)
	}
}

// Without an embedder a text query fails with the hint, in every row
// policy: the hint is our own text, not row data.
func TestSimilarTextWithoutEmbedder(t *testing.T) {
	for _, p := range []RowPolicy{RowsAlways, RowsNever, RowsAsk} {
		e := newVecEnv(t, vecOptions{policy: p})
		out, err := e.ts.Similar(ctx, SimilarInput{Connection: "vec", Collection: drivertest.VectorCollection, Text: "red shoes"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.Error, driver.NeedEmbedderHint) {
			t.Errorf("%s: error = %q, want the embedder hint", p, out.Error)
		}
	}
}

// Exactly one of vector and text; the metric must be known; the driver must
// be a VectorSearcher.
func TestSimilarRefusals(t *testing.T) {
	e := newVecEnv(t, vecOptions{})
	for _, tc := range []struct {
		name string
		in   SimilarInput
		want string
	}{
		{"neither", SimilarInput{Connection: "vec", Collection: "vecs"}, "exactly one of vector or text"},
		{"both", SimilarInput{Connection: "vec", Collection: "vecs", Text: "x", Vector: []float32{1}}, "exactly one of vector or text"},
		{"metric", SimilarInput{Connection: "vec", Collection: "vecs", Vector: []float32{1, 0, 0, 0}, Metric: "manhattan"}, "metric must be l2, cosine or ip"},
		{"no-collection", SimilarInput{Connection: "vec", Vector: []float32{1, 0, 0, 0}}, "collection is required"},
		{"not-a-searcher", SimilarInput{Connection: "plain", Collection: "vecs", Vector: []float32{1, 0, 0, 0}}, "similarity search is not supported by plainfake"},
		{"unknown", SimilarInput{Connection: "nope", Collection: "vecs", Vector: []float32{1, 0, 0, 0}}, "unknown connection"},
		{"bad-op", SimilarInput{Connection: "vec", Collection: "vecs", Vector: []float32{1, 0, 0, 0}, Filter: &SimilarFilter{Conditions: []FilterCondition{{Field: "id", Op: "like"}}}}, `filter op "like"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := e.ts.Similar(ctx, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.Error, tc.want) {
				t.Errorf("error = %q, want it to contain %q", out.Error, tc.want)
			}
		})
	}
}

// A filter narrows the candidates; whole JSON numbers compare as integers.
func TestSimilarFilter(t *testing.T) {
	e := newVecEnv(t, vecOptions{})
	out, err := e.ts.Similar(ctx, SimilarInput{Connection: "vec", Collection: "vecs", Vector: []float32{1, 0, 0, 0}, Metric: "l2",
		Filter: &SimilarFilter{Conditions: []FilterCondition{{Field: "id", Op: "gt", Value: float64(1)}, {Field: "id", Op: "ne", Value: float64(2)}}}})
	if err != nil {
		t.Fatal(err)
	}
	rows := similarRows(t, out)
	id := colIndex(t, out, "id")
	for _, r := range rows {
		if r[id] == float64(1) || r[id] == float64(2) {
			t.Errorf("filtered row %v returned", r)
		}
	}
	if len(rows) != 4 {
		t.Errorf("%d rows, want 4", len(rows))
	}
}

// k defaults to 10 and is capped at MaxRows, which is capped at the row cap
// (200 for the assistant, 1000 for MCP).
func TestSimilarKCap(t *testing.T) {
	for _, tc := range []struct {
		name            string
		o               vecOptions
		k               int
		wantK, wantRows int
		truncated       bool
	}{
		{"default", vecOptions{}, 0, 10, 6, false},
		{"max-rows", vecOptions{maxRows: 2}, 5, 2, 2, true},
		{"default-over-max-rows", vecOptions{maxRows: 3}, 0, 3, 3, true},
		{"assistant-cap", vecOptions{maxRows: 1000}, 500, 200, 6, true},
		{"mcp-cap", vecOptions{maxRows: 5000, rowCap: config.MCPMaxRowsCap}, 5000, 1000, 6, true},
		{"mcp-under-cap", vecOptions{maxRows: 1000, rowCap: config.MCPMaxRowsCap}, 500, 500, 6, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVecEnv(t, tc.o)
			out, err := e.ts.Similar(ctx, SimilarInput{Connection: "vec", Collection: "vecs", Vector: []float32{1, 0, 0, 0}, K: tc.k})
			if err != nil {
				t.Fatal(err)
			}
			if rows := similarRows(t, out); len(rows) != tc.wantRows || out.Truncated != tc.truncated {
				t.Errorf("%d rows, truncated %v; want %d, %v", len(rows), out.Truncated, tc.wantRows, tc.truncated)
			}
			// The history text carries the k sent to the driver.
			if h := e.hist.all(); len(h) != 1 || !strings.Contains(h[0].Query, fmt.Sprintf(" k=%d ", tc.wantK)) {
				t.Errorf("history = %+v, want k=%d", h, tc.wantK)
			}
		})
	}
}

// The row policy applies as for query: never withholds, ask asks once with
// the statement and withholds on no, always sends. Vectors are cut to
// their first 8 components when sent.
func TestSimilarRowPolicy(t *testing.T) {
	in := SimilarInput{Connection: "vec", Collection: "vecs", Vector: []float32{1, 0, 0, 0}, K: 2}
	t.Run("never", func(t *testing.T) {
		e := newVecEnv(t, vecOptions{policy: RowsNever})
		out, err := e.ts.Similar(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if out.Rows != nil || out.RowsWithheld != 2 || out.Note != noteNever || len(out.Columns) == 0 {
			t.Fatalf("out = %+v, want rows withheld with columns", out)
		}
		if h := e.hist.all(); len(h) != 1 || !h[0].OK || h[0].Rows != 2 {
			t.Fatalf("history = %+v", h)
		}
	})
	for _, tc := range []struct {
		answer assistant.Consent
		sent   bool
	}{{assistant.ConsentNo, false}, {assistant.ConsentOnce, true}} {
		t.Run(fmt.Sprintf("ask-%v", tc.answer), func(t *testing.T) {
			e := newVecEnv(t, vecOptions{policy: RowsAsk})
			var reqs []assistant.ConsentRequest
			e.setEvents(assistant.Events{ConsentRows: func(_ context.Context, r assistant.ConsentRequest) (assistant.Consent, error) {
				reqs = append(reqs, r)
				return tc.answer, nil
			}})
			out, err := e.ts.Similar(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			if len(reqs) != 1 || reqs[0].Conn != "vec" || reqs[0].Rows != 2 || !strings.Contains(reqs[0].What, "similar vecs k=2 vector dim=4") {
				t.Fatalf("consent requests = %+v", reqs)
			}
			if sent := out.Rows != nil; sent != tc.sent || (!tc.sent && out.Note != noteDeclined) {
				t.Fatalf("out = %+v, want sent=%v", out, tc.sent)
			}
		})
	}
	t.Run("always", func(t *testing.T) {
		e := newVecEnv(t, vecOptions{policy: RowsAlways})
		out, err := e.ts.Similar(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if len(similarRows(t, out)) != 2 {
			t.Fatalf("out = %+v", out)
		}
	})
}

// A sent vector shows its first 8 components and "…"; nothing redacted
// leaks through the error path either.
func TestVectorValuesAreCut(t *testing.T) {
	e := newEnv(t)
	v := make([]float32, 1536)
	for i := range v {
		v[i] = float32(i)
	}
	got := e.ts.encodeValue(driver.Vector(v), func(s string) string { return s })
	if got != "[0, 1, 2, 3, 4, 5, 6, 7, …]" {
		t.Errorf("encoded vector = %v", got)
	}
}

// VectorSearchable reports whether any connection the tools may reach
// (ToolDeps.Connections, narrowed by the turn) is a VectorSearcher.
func TestVectorSearchable(t *testing.T) {
	e := newVecEnv(t, vecOptions{})
	if !e.ts.VectorSearchable(ctx) {
		t.Error("vec is a VectorSearcher: want true")
	}
	if e.ts.VectorSearchable(WithConnections(ctx, "plain")) {
		t.Error("turn bound to plain: want false")
	}
	if !e.ts.VectorSearchable(WithConnections(ctx, "vec")) {
		t.Error("turn bound to vec: want true")
	}
	only := NewToolset(ToolDeps{Manager: e.mgr, Connections: []string{"plain"}})
	if only.VectorSearchable(ctx) {
		t.Error("deps limited to plain: want false")
	}
	if got := e.ts.SimilarTool().Name(); got != NameSimilar {
		t.Errorf("SimilarTool() = %q", got)
	}
}
