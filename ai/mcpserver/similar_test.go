package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/xavidop/tobira/ai/tools"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivers/sqlite"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/history"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

// newVecEnv is a session over "vec", a Fake with the vector fixture and a
// fake embedder, and "lite", a SQLite database (no VectorSearcher).
func newVecEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	f := drivertest.NewFake(drivertest.WithVectors())
	reg := driver.NewRegistry()
	for _, d := range []driver.Driver{sqlite.New(), f.Driver()} {
		if err := reg.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "vec", Driver: "fake", DSN: "fake://x"},
		{Name: "lite", Driver: "sqlite", DSN: "sqlite:" + db},
	}}
	mgr := session.New(cfg, reg, secrets.Resolver{}, false, session.WithEmbedder(drivertest.HashEmbedder{Dims: 4}))
	t.Cleanup(func() { _ = mgr.CloseAll() })
	hreg := driver.NewRegistry()
	if err := hreg.Add(sqlite.New()); err != nil {
		t.Fatal(err)
	}
	hist, err := history.Open(ctx, hreg, filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hist.Close() })
	return &env{mgr: mgr, hist: hist, db: db}
}

// similar is registered only when an exposed connection's driver is a
// VectorSearcher.
func TestSimilarRegisteredOnlyForVectorSearchers(t *testing.T) {
	for _, tc := range []struct {
		expose []string
		want   bool
	}{
		{[]string{"lite"}, false},
		{[]string{"vec"}, true},
		{[]string{"lite", "vec"}, true},
	} {
		c := newVecEnv(t).serve(t, config.MCP{Expose: tc.expose}, "unit")
		if got := slices.Contains(toolNames(t, c), tools.NameSimilar); got != tc.want {
			t.Errorf("expose %v: similar listed = %v, want %v", tc.expose, got, tc.want)
		}
	}
}

// similar's input schema names its arguments and the metrics.
func TestSimilarInputSchema(t *testing.T) {
	c := newVecEnv(t).serve(t, config.MCP{Expose: []string{"vec"}}, "unit")
	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name != tools.NameSimilar {
			continue
		}
		b, _ := json.Marshal(tl.InputSchema)
		for _, w := range []string{`"vector"`, `"text"`, `"k"`, `"metric"`, `"filter"`, `"cosine"`, `"ip"`} {
			if !strings.Contains(string(b), w) {
				t.Errorf("schema lacks %s: %s", w, b)
			}
		}
		return
	}
	t.Fatal("no similar tool")
}

type similarOut struct {
	Columns []struct {
		Name string `json:"name"`
	} `json:"columns"`
	Rows         [][]any `json:"rows"`
	Truncated    bool    `json:"truncated"`
	RowsWithheld int     `json:"rows_withheld"`
}

// A call returns the neighbours, capped at max_rows, and lands in history
// with source mcp and the similar text.
func TestSimilarCall(t *testing.T) {
	e := newVecEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"vec", "lite"}, MaxRows: 2}, "claude-code")
	var out similarOut
	okJSON(t, call(t, c, tools.NameSimilar, map[string]any{"connection": "vec", "collection": "vecs", "vector": []float64{1, 0, 0, 0}, "k": 5, "metric": "l2"}), &out)
	if len(out.Rows) != 2 || !out.Truncated || out.Rows[0][0] != float64(1) {
		t.Fatalf("result = %+v, want 2 rows nearest id 1, truncated", out)
	}
	okJSON(t, call(t, c, tools.NameSimilar, map[string]any{"connection": "vec", "collection": "vecs", "text": "red shoes"}), &out)
	if len(out.Rows) != 2 {
		t.Fatalf("text result = %+v", out)
	}
	res := call(t, c, tools.NameSimilar, map[string]any{"connection": "lite", "collection": "t", "vector": []float64{1}})
	if !res.IsError || !strings.Contains(text(t, res), "similarity search is not supported by sqlite") {
		t.Fatalf("lite: %s", text(t, res))
	}
	got := historyMCP(t, e)
	var queries []string
	for _, h := range got {
		if h.Client != "claude-code/1.0.0" {
			t.Errorf("entry %+v, want the client", h)
		}
		queries = append(queries, h.Query)
	}
	if len(got) != 3 || got[0].Query != "similar vecs k=2 vector dim=4" || got[0].Conn != "vec" || !got[0].OK ||
		got[1].Query != `similar vecs k=2 "red shoes"` || got[2].OK || got[2].Conn != "lite" {
		t.Fatalf("history = %q", queries)
	}
}

// send_rows: never withholds the rows of similar too.
func TestSimilarSendRowsNever(t *testing.T) {
	c := newVecEnv(t).serve(t, config.MCP{Expose: []string{"vec"}, SendRows: "never"}, "unit")
	var out similarOut
	res := call(t, c, tools.NameSimilar, map[string]any{"connection": "vec", "collection": "vecs", "vector": []float64{1, 0, 0, 0}, "k": 3})
	okJSON(t, res, &out)
	if out.Rows != nil || out.RowsWithheld != 3 || strings.Contains(text(t, res), "near") {
		t.Fatalf("result = %s, want rows withheld", text(t, res))
	}
}

// A similar call that fails before it searches is recorded with its query
// vector summarised, not as thousands of numbers.
func TestSimilarFailureRecordsASummary(t *testing.T) {
	e := newVecEnv(t)
	c := e.serve(t, config.MCP{Expose: []string{"vec"}}, "unit")
	vec := make([]float64, 1536)
	res := call(t, c, tools.NameSimilar, map[string]any{"connection": "nope", "collection": "vecs", "vector": vec})
	if !res.IsError {
		t.Fatalf("result = %s, want an error", text(t, res))
	}
	got := historyMCP(t, e)
	if len(got) != 1 || !strings.Contains(got[0].Query, `"<vector dim=1536>"`) || strings.Contains(got[0].Query, "0,0,0") {
		for _, h := range got {
			t.Logf("query %.200s", h.Query)
		}
		t.Fatalf("history has %d entries, want one with the vector summarised", len(got))
	}
}

// An unavailable connection (its driver plugin's Info failed) is listed
// with the reason, refused by the tools, and never makes similar appear.
func TestUnavailableConnectionIsListedAndRefused(t *testing.T) {
	const why = `driver plugin "fake" is unavailable: the handshake failed`
	e := newVecEnv(t)
	e.unavailable = map[string]string{"vec": why}
	c := e.serve(t, config.MCP{Expose: []string{"lite", "vec"}}, "unit")
	if slices.Contains(toolNames(t, c), tools.NameSimilar) {
		t.Error("similar is offered for an unavailable connection")
	}
	var conns []tools.Connection
	okJSON(t, call(t, c, tools.NameListConnections, nil), &conns)
	var vec *tools.Connection
	for i := range conns {
		if conns[i].Name == "vec" {
			vec = &conns[i]
		}
	}
	if vec == nil || vec.Status != "unavailable" || vec.Error != why {
		t.Errorf("list_connections = %+v, want vec unavailable with the reason", conns)
	}
	res := call(t, c, tools.NameQuery, map[string]any{"connection": "vec", "query": "select 1"})
	if got := text(t, res); !strings.Contains(got, `connection "vec" is unavailable: `+why) {
		t.Errorf("query on vec = %s, want the unavailable reason", got)
	}
}
