package libsql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// Only the Hrana value shapes decode: a null without a value, an integer as
// decimal text, a float as a number, text as a string, a blob as base64.
func TestValueAcceptsOnlyHranaShapes(t *testing.T) {
	good := []string{
		`{"type":"null"}`, `{"type":"null","value":null}`, `{"type":"integer","value":"-12"}`,
		`{"type":"float","value":1.5}`, `{"type":"float","value":-3e10}`, `{"type":"text","value":"héllo"}`,
		`{"type":"blob","base64":"AAEC"}`,
	}
	for _, in := range good {
		var v hranaValue
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
	bad := []string{
		`{"type":"null","value":[[],[]]}`, `{"type":"null","value":"x"}`, `{"type":"integer","value":{}}`,
		`{"type":"integer","value":12}`, `{"type":"integer","value":"1.5"}`, `{"type":"float","value":"1.5"}`,
		`{"type":"float","value":[1]}`, `{"type":"text","value":true}`, `{"type":"text","value":7}`,
		`{"type":"text"}`, `{"type":"blob","base64":7}`, `{"type":"blob"}`, `{"type":"blob","base64":"!!"}`,
		`{"type":"bogus","value":"x"}`, `{"value":"x"}`, `[]`, `"text"`, `{"type":["text"],"value":"x"}`,
	}
	for _, in := range bad {
		var v hranaValue
		if err := json.Unmarshal([]byte(in), &v); err == nil {
			t.Errorf("%s decoded to %+v", in, v)
		}
	}
}

// hostileFake serves body for every request after the first (the Open ping).
func hostileFake(t *testing.T, body []byte) (*conn, *fakeServer) {
	t.Helper()
	f := newFake(t, okReply)
	c := openFake(t, f, driver.ConnConfig{})
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	return c, f
}

// executeBody wraps rows (a JSON array text) in a one-request pipeline
// response for an autocommit statement.
func executeBody(cols, rows string) []byte {
	return []byte(`{"baton":null,"results":[{"type":"ok","response":{"type":"execute","result":{"cols":` + cols +
		`,"rows":` + rows + `,"affected_row_count":0}}},{"type":"ok","response":{"type":"close"}}]}`)
}

// allocDuring reports the peak growth of the live heap (HeapAlloc, sampled
// every 2 ms) while f runs.
func allocDuring(f func()) uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapAlloc, ms.HeapAlloc
	done := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				peak = max(peak, m.HeapAlloc)
			}
		}
	}()
	f()
	close(done)
	<-sampled
	if peak < base {
		return 0
	}
	return peak - base
}

// A hostile server packing arrays into "value" is refused without the
// decoder blowing the bytes up.
func TestHostileValueMemory(t *testing.T) {
	var rows bytes.Buffer
	rows.WriteByte('[')
	cell := `[{"type":"null","value":[` + strings.Repeat(`[],`, 2000) + `[]]}]`
	for rows.Len() < 24<<20 {
		if rows.Len() > 1 {
			rows.WriteByte(',')
		}
		rows.WriteString(cell)
	}
	rows.WriteByte(']')
	body := executeBody(`[{"name":"a","decltype":null}]`, rows.String())
	c, _ := hostileFake(t, body)
	var err error
	alloc := allocDuring(func() { _, err = c.Query(context.Background(), driver.Query{Text: "select a from t"}) })
	if err == nil {
		t.Fatal("a hostile value decoded")
	}
	t.Logf("peak heap growth %d MiB for a %d MiB body", alloc>>20, len(body)>>20)
	if limit := uint64(5 * len(body)); alloc > limit {
		t.Errorf("allocated %d MiB for a %d MiB body (limit %d MiB)", alloc>>20, len(body)>>20, limit>>20)
	}
}

// A well-formed result whose decoded cells exceed the budget is refused
// with driver.ErrLimit, and decoding stops at the budget.
func TestResultBudget(t *testing.T) {
	var rows bytes.Buffer
	rows.WriteByte('[')
	for i := 0; rows.Len() < 40<<20; i++ {
		if i > 0 {
			rows.WriteByte(',')
		}
		rows.WriteString(`[{"type":"null"},{"type":"integer","value":"1"}]`)
	}
	rows.WriteByte(']')
	body := executeBody(`[{"name":"a","decltype":null},{"name":"b","decltype":null}]`, rows.String())
	if len(body) > maxResponseBytes {
		t.Fatalf("test body %d MiB is over the response cap", len(body)>>20)
	}
	c, _ := hostileFake(t, body)
	var err error
	alloc := allocDuring(func() { _, err = c.Query(context.Background(), driver.Query{Text: "select a, b from t"}) })
	if !errors.Is(err, driver.ErrLimit) || driver.KindOf(err) != driver.ErrLimit {
		t.Fatalf("got %v, want ErrLimit", err)
	}
	t.Logf("peak heap growth %d MiB for a %d MiB body", alloc>>20, len(body)>>20)
	// 5x the body: the decoder's peak varies by a few MiB across Go
	// releases; the point is that it stays proportional to the body.
	if limit := uint64(5*len(body)) + resultBudgetBytes; alloc > limit {
		t.Errorf("allocated %d MiB for a %d MiB body (limit %d MiB)", alloc>>20, len(body)>>20, limit>>20)
	}
	// Text and blob cells are charged by their decoded length.
	b := newBudget(1000, 10)
	if err := b.cell(hranaValue{Type: "text", Value: strings.Repeat("x", 900)}); err != nil {
		t.Fatal(err)
	}
	if err := b.cell(hranaValue{Type: "text", Value: strings.Repeat("x", 200)}); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("over the byte budget: %v", err)
	}
	b = newBudget(1<<20, 2)
	for i, want := range []bool{true, true, false} {
		if err := b.cell(hranaValue{Type: "null"}); (err == nil) != want {
			t.Errorf("cell %d: %v", i, err)
		}
	}
}

func TestResponseCapIsLowered(t *testing.T) {
	if maxResponseBytes > 64<<20 {
		t.Errorf("response cap %d MiB, want at most 64 MiB", maxResponseBytes>>20)
	}
}

// Server error text is cut to 1 KiB on a rune boundary, in every branch.
func TestErrorMessageTruncated(t *testing.T) {
	long := strings.Repeat("é", 4000) // 2 bytes each
	check := func(name, msg string) {
		t.Helper()
		if len(msg) > maxErrorBytes+len("…")+len("HTTP 500: ") {
			t.Errorf("%s: %d bytes", name, len(msg))
		}
		if !utf8.ValidString(msg) {
			t.Errorf("%s: invalid UTF-8", name)
		}
	}
	js, _ := json.Marshal(map[string]string{"error": long})
	check("json error", httpError(500, js).Error())
	hj, _ := json.Marshal(map[string]string{"message": long, "code": "X"})
	check("hrana error", httpError(500, hj).Error())
	check("plain", httpError(500, []byte("a"+long)).Error())
	var he hranaError
	if err := json.Unmarshal(hj, &he); err != nil {
		t.Fatal(err)
	}
	check("result error", he.Error())
	if he.Code != "X" {
		t.Errorf("code = %q", he.Code)
	}
}

// A hostile parent chain does not grow the plan text quadratically, and the
// tree depth is capped.
func TestPlanDepthCapped(t *testing.T) {
	const n = 20000
	rows := make([]driver.Row, n)
	for i := range rows {
		rows[i] = driver.Row{driver.Int(int64(i + 1)), driver.Int(int64(i)), driver.Int(0), driver.String("SCAN t")}
	}
	p := buildPlan(rows, "select 1")
	if len(p.Text) > n*(2*maxPlanDepth+len("SCAN t\n")) {
		t.Errorf("plan text is %d bytes", len(p.Text))
	}
	depth, node := 0, p.Root
	for len(node.Children) > 0 {
		node = node.Children[len(node.Children)-1]
		depth++
	}
	if depth > maxPlanDepth {
		t.Errorf("tree depth %d, want at most %d", depth, maxPlanDepth)
	}
}

// A base_url belongs to the stream that received it: new streams and
// one-shot requests keep the configured endpoint.
func TestBaseURLPerStream(t *testing.T) {
	var f *fakeServer
	f = newFake(t, func(r pipelineRequest) (int, any) {
		base := any(nil)
		if r.Baton == nil && len(r.Requests) == 1 && r.Requests[0].Type == "execute" && r.Requests[0].Stmt.SQL == "BEGIN IMMEDIATE" {
			base = f.URL + "/sticky"
		}
		res := okResults(r)
		if r.Baton != nil && len(r.Requests) == 1 && r.Requests[0].Type == "batch" && r.Requests[0].Batch.Steps[0].Stmt.SQL == "BEGIN" {
			batch := res[0]["response"].(map[string]any)["result"].(map[string]any)
			batch["step_errors"].([]any)[0] = map[string]any{"message": "SQLite error: cannot start a transaction within a transaction", "code": "SQLITE_UNKNOWN"}
			batch["step_results"].([]any)[0] = nil
			n := len(r.Requests[0].Batch.Steps)
			batch["step_results"].([]any)[n-1] = nil // ROLLBACK skipped: the probe failed
		}
		return http.StatusOK, map[string]any{"baton": "b", "base_url": base, "results": res}
	})
	c := openFake(t, f, driver.ConnConfig{})
	ctx := context.Background()
	tx1, err := c.Begin(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx1.Query(ctx, driver.Query{Text: "select 1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	paths := append([]string(nil), f.paths...)
	f.mu.Unlock()
	// open ping, BEGIN IMMEDIATE, tx query (sticky), ping (default)
	want := []string{"/v2/pipeline", "/v2/pipeline", "/sticky/v2/pipeline", "/v2/pipeline"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	_ = tx1.Rollback(ctx)
}

// Close cancels requests still in flight and rolls back open transactions
// concurrently, within a bounded total even against a server that hangs.
func TestCloseAbortsInFlightAndIsBounded(t *testing.T) {
	f := newFake(t, okReply)
	c := openFake(t, f, driver.ConnConfig{})
	ctx := context.Background()
	var txs []driver.Tx
	for range 3 {
		tx, err := c.Begin(ctx, driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		txs = append(txs, tx)
	}
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server watches for a disconnect only once the body is read
		<-r.Context().Done()
	})
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := c.Query(ctx, driver.Query{Text: "select 1"})
		errs <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > endTimeout+2*time.Second {
		t.Errorf("Close took %v with 3 transactions on a hung server", d)
	}
	wg.Wait()
	if err := <-errs; err == nil {
		t.Error("the in-flight query succeeded after Close")
	}
	for _, tx := range txs {
		if _, err := tx.Query(ctx, driver.Query{Text: "select 1"}); !errors.Is(err, driver.ErrConnection) {
			t.Errorf("tx after Close: %v", err)
		}
	}
}

func colsJSON(n int, name string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":"` + name + `","decltype":null}`)
	}
	b.WriteByte(']')
	return b.String()
}

// The column count is capped at the protocol's 4096.
func TestColsCapped(t *testing.T) {
	c, _ := hostileFake(t, executeBody(colsJSON(maxColumns, "c"), "[]"))
	if _, err := c.Query(context.Background(), driver.Query{Text: "select * from t"}); err != nil {
		t.Errorf("%d columns: %v", maxColumns, err)
	}
	c2, _ := hostileFake(t, executeBody(colsJSON(maxColumns+1, "c"), "[]"))
	if _, err := c2.Query(context.Background(), driver.Query{Text: "select * from t"}); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("%d columns: got %v, want ErrLimit", maxColumns+1, err)
	}
}

// A hostile cols array is refused at the cap, before it is materialised.
func TestHostileColsMemory(t *testing.T) {
	cols := colsJSON(1_500_000, "x")
	body := executeBody(cols, "[]")
	c, _ := hostileFake(t, body)
	var err error
	alloc := allocDuring(func() { _, err = c.Query(context.Background(), driver.Query{Text: "select * from t"}) })
	t.Logf("peak heap growth %d MiB for a %d MiB body", alloc>>20, len(body)>>20)
	if !errors.Is(err, driver.ErrLimit) {
		t.Fatalf("got %v, want ErrLimit", err)
	}
	if limit := uint64(4 * len(body)); alloc > limit {
		t.Errorf("allocated %d MiB for a %d MiB body (limit %d MiB)", alloc>>20, len(body)>>20, limit>>20)
	}
}

// Each column is charged to the budget as it is decoded, its name included.
func TestColsChargeBudget(t *testing.T) {
	data := []byte(`{"cols":` + colsJSON(3, strings.Repeat("n", 400)) + `,"rows":[]}`)
	if _, err := decodeStmtResult(data, newBudget(1000, 100)); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("got %v, want ErrLimit", err)
	}
	if _, err := decodeStmtResult(data, newBudget(10000, 100)); err != nil {
		t.Errorf("within budget: %v", err)
	}
}

// The budget covers the whole response: batch steps share it, so steps
// that each fit cannot add up past it.
func TestBudgetSharedAcrossBatch(t *testing.T) {
	cellsPerStep := (resultBudgetBytes / cellCost) / 2 // each step uses about half the budget
	var step bytes.Buffer
	step.WriteString(`{"cols":[{"name":"a","decltype":null}],"rows":[`)
	for i := range cellsPerStep {
		if i > 0 {
			step.WriteByte(',')
		}
		step.WriteString(`[{"type":"null"}]`)
	}
	step.WriteString(`]}`)
	s := step.String()
	if _, err := decodeStmtResult([]byte(s), newBudget(resultBudgetBytes, resultBudgetCells)); err != nil {
		t.Fatalf("one step alone must fit the budget: %v", err)
	}
	body := []byte(`{"baton":null,"results":[{"type":"ok","response":{"type":"batch","result":{"step_results":[` +
		s + `,` + s + `,` + s + `],"step_errors":[null,null,null]}}},{"type":"ok","response":{"type":"close"}}]}`)
	if len(body) > maxResponseBytes {
		t.Fatalf("test body %d MiB is over the response cap", len(body)>>20)
	}
	f := newFake(t, okReply)
	c := openFake(t, f, driver.ConnConfig{ReadOnly: true})
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	var err error
	alloc := allocDuring(func() { _, err = c.Query(context.Background(), driver.Query{Text: "select a from t"}) })
	t.Logf("peak heap growth %d MiB for a %d MiB body", alloc>>20, len(body)>>20)
	if !errors.Is(err, driver.ErrLimit) {
		t.Fatalf("three half-budget steps: got %v, want ErrLimit", err)
	}
	// 5x the body: the decoder's peak varies by a few MiB across Go
	// releases; the point is that it stays proportional to the body.
	if limit := uint64(5*len(body)) + resultBudgetBytes; alloc > limit {
		t.Errorf("allocated %d MiB for a %d MiB body (limit %d MiB)", alloc>>20, len(body)>>20, limit>>20)
	}
}
