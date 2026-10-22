package bigquery

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"

	"github.com/xavidop/tobira/driver"
)

// fakeAPI is a minimal stand-in for the BigQuery REST API, used where the
// emulator cannot show what real BigQuery does: the emulator's dry run
// reports statementType SELECT for every statement, so the dry-run layer
// of the read-only guard is proven here, against the statement types real
// BigQuery returns.
type fakeAPI struct {
	t *testing.T

	mu sync.Mutex
	// statementType answers a dry run of a query text; "-" leaves the
	// field out, as a server that does not report it would.
	statementType func(query string) string
	dryRuns       []map[string]any // the configuration.query of each dry run
	runs          []map[string]any // the configuration.query of each real run
	cancels       []string         // job ids cancelled
	blockResults  bool             // getQueryResults waits until the request ends
	dml           bool             // real runs are a DELETE of 3 rows
	bigValue      bool             // results hold one 2 MiB string
	resultsCalled chan struct{}
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{t: t, statementType: func(string) string { return "SELECT" }, resultsCalled: make(chan struct{}, 16)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/bigquery/v2")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	w.Header().Set("Content-Type", "application/json")
	switch {
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "jobs" && r.Method == http.MethodPost:
		f.insertJob(w, r, parts[1])
	case len(parts) == 4 && parts[2] == "queries" && r.Method == http.MethodGet:
		f.queryResults(w, r, parts[1], parts[3])
	case len(parts) >= 4 && parts[2] == "jobs" && r.URL.Query().Get("location") != "asia-south1":
		// Real BigQuery finds a job outside US and EU only with its location.
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"error": map[string]any{"code": 404, "message": "Not found: Job", "errors": []any{map[string]any{"reason": "notFound", "message": "Not found: Job"}}}})
	case len(parts) == 5 && parts[2] == "jobs" && parts[4] == "cancel":
		f.mu.Lock()
		f.cancels = append(f.cancels, parts[3])
		f.mu.Unlock()
		writeJSON(w, map[string]any{"job": doneJob(parts[1], parts[3], nil)})
	case len(parts) == 4 && parts[2] == "jobs" && r.Method == http.MethodGet:
		writeJSON(w, doneJob(parts[1], parts[3], f.runStats()))
	case len(parts) == 4 && parts[2] == "datasets" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"datasetReference": map[string]any{"projectId": parts[1], "datasetId": parts[3]}, "location": "US"})
	default:
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"error": map[string]any{"code": 404, "message": "Not found: " + r.URL.Path, "errors": []any{map[string]any{"reason": "notFound", "message": "Not found"}}}})
	}
}

func writeJSON(w io.Writer, v any) { _ = json.NewEncoder(w).Encode(v) }

func doneJob(project, id string, stats map[string]any) map[string]any {
	j := map[string]any{
		"jobReference": map[string]any{"projectId": project, "jobId": id, "location": "asia-south1"},
		"status":       map[string]any{"state": "DONE"},
		"configuration": map[string]any{
			"query": map[string]any{"query": "x"},
		},
	}
	if stats != nil {
		j["statistics"] = stats
	}
	return j
}

func (f *fakeAPI) insertJob(w http.ResponseWriter, r *http.Request, project string) {
	var job struct {
		JobReference  map[string]any `json:"jobReference"`
		Configuration struct {
			DryRun bool           `json:"dryRun"`
			Query  map[string]any `json:"query"`
		} `json:"configuration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		f.t.Errorf("fake: bad job body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	id, _ := job.JobReference["jobId"].(string)
	q, _ := job.Configuration.Query["query"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	if job.Configuration.DryRun {
		f.dryRuns = append(f.dryRuns, job.Configuration.Query)
		qs := map[string]any{
			"totalBytesProcessed": "2048",
			"schema": map[string]any{"fields": []any{
				map[string]any{"name": "id", "type": "INTEGER", "mode": "REQUIRED"},
				map[string]any{"name": "tags", "type": "STRING", "mode": "REPEATED"},
			}},
			"referencedTables": []any{map[string]any{"projectId": project, "datasetId": "ds", "tableId": "tobira_people"}},
		}
		if st := f.statementType(q); st != "-" {
			qs["statementType"] = st
		}
		j := doneJob(project, id, map[string]any{"totalBytesProcessed": "2048", "query": qs})
		j["configuration"] = map[string]any{"dryRun": true, "query": job.Configuration.Query}
		writeJSON(w, j)
		return
	}
	f.runs = append(f.runs, job.Configuration.Query)
	j := doneJob(project, id, f.runStatsLocked())
	if f.blockResults {
		j["status"] = map[string]any{"state": "RUNNING"}
	}
	writeJSON(w, j)
}

func (f *fakeAPI) runStats() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runStatsLocked()
}

func (f *fakeAPI) runStatsLocked() map[string]any {
	if f.dml {
		return map[string]any{"query": map[string]any{"statementType": "DELETE", "numDmlAffectedRows": "3"}}
	}
	return map[string]any{"query": map[string]any{"statementType": "SELECT"}}
}

func (f *fakeAPI) queryResults(w http.ResponseWriter, r *http.Request, project, id string) {
	f.mu.Lock()
	block, dml := f.blockResults, f.dml
	f.mu.Unlock()
	if dml {
		writeJSON(w, map[string]any{
			"jobReference": map[string]any{"projectId": project, "jobId": id, "location": "asia-south1"},
			"jobComplete":  true, "totalRows": "0", "numDmlAffectedRows": "3",
		})
		return
	}
	select {
	case f.resultsCalled <- struct{}{}:
	default:
	}
	if block {
		<-r.Context().Done()
		return
	}
	f.mu.Lock()
	big := f.bigValue
	f.mu.Unlock()
	if big {
		writeJSON(w, map[string]any{
			"jobReference": map[string]any{"projectId": project, "jobId": id, "location": "asia-south1"},
			"jobComplete":  true,
			"schema":       map[string]any{"fields": []any{map[string]any{"name": "s", "type": "STRING"}}},
			"rows":         []any{map[string]any{"f": []any{map[string]any{"v": strings.Repeat("x", 2<<20)}}}},
			"totalRows":    "1",
		})
		return
	}
	writeJSON(w, map[string]any{
		"jobReference": map[string]any{"projectId": project, "jobId": id, "location": "asia-south1"},
		"jobComplete":  true,
		"schema": map[string]any{"fields": []any{
			map[string]any{"name": "id", "type": "INTEGER", "mode": "NULLABLE"},
		}},
		"rows":      []any{map[string]any{"f": []any{map[string]any{"v": "7"}}}},
		"totalRows": "1",
	})
}

func (f *fakeAPI) counts() (dry, run int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dryRuns), len(f.runs)
}

// openFake opens a connection on the fake API as an emulator endpoint.
func openFake(t *testing.T, srv *httptest.Server, params string, readOnly bool) driver.Conn {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "bigquery://p/ds?emulator=" + host + params, ReadOnly: readOnly})
	if err != nil {
		t.Fatalf("Open on the fake: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func drain(ctx context.Context, c driver.Conn, q driver.Query) ([]driver.Row, error) {
	rs, err := c.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	return rows, err
}

// The dry run is the second layer: a statement the lexical allow-list
// passes is still refused, and never run, when real BigQuery says it is
// not a SELECT, or says nothing.
func TestDryRunLayerRefusesNonSelect(t *testing.T) {
	for _, st := range []string{"DELETE", "SCRIPT", "CREATE_TABLE_AS_SELECT", "EXPORT_DATA", "-", ""} {
		t.Run("type "+st, func(t *testing.T) {
			f, srv := newFakeAPI(t)
			f.statementType = func(string) string { return st }
			for _, tc := range []struct {
				name string
				conn driver.Conn
				q    driver.Query
			}{
				{"conn read-only", openFake(t, srv, "", true), driver.Query{Text: "SELECT 1"}},
				{"query read-only", openFake(t, srv, "", false), driver.Query{Text: "SELECT 1", ReadOnly: true}},
			} {
				_, err := drain(context.Background(), tc.conn, tc.q)
				if !errors.Is(err, driver.ErrReadOnly) {
					t.Errorf("%s: want ErrReadOnly, got %v", tc.name, err)
				}
			}
			if dry, run := f.counts(); dry != 2 || run != 0 {
				t.Errorf("fake saw %d dry runs and %d runs, want 2 and 0", dry, run)
			}
		})
	}
}

func TestDryRunLayerAllowsSelect(t *testing.T) {
	f, srv := newFakeAPI(t)
	c := openFake(t, srv, "", true)
	rows, err := drain(context.Background(), c, driver.Query{Text: "SELECT id FROM t WHERE id = ?", Params: []driver.Value{driver.Int(7)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	if n, ok := rows[0][0].AsInt(); !ok || n != 7 {
		t.Errorf("rows = %v", rows)
	}
	if dry, run := f.counts(); dry != 1 || run != 1 {
		t.Errorf("fake saw %d dry runs and %d runs, want 1 and 1", dry, run)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range append(f.dryRuns, f.runs...) {
		if q["maximumBytesBilled"] != "1073741824" {
			t.Errorf("job maximumBytesBilled = %v, want 1073741824", q["maximumBytesBilled"])
		}
		if q["useLegacySql"] != false {
			t.Errorf("job useLegacySql = %v, want false", q["useLegacySql"])
		}
		if q["createSession"] == true || q["destinationTable"] != nil {
			t.Errorf("job carries a session or destination: %v", q)
		}
	}
	if params, _ := f.runs[0]["queryParameters"].([]any); len(params) != 1 {
		t.Errorf("queryParameters = %v", f.runs[0]["queryParameters"])
	}
}

// A writable call skips the dry run but keeps the byte cap; the cap is
// configurable, and 0 sends none.
func TestBytesBilledCap(t *testing.T) {
	f, srv := newFakeAPI(t)
	c := openFake(t, srv, "&max_bytes_billed=5000", false)
	if _, err := drain(context.Background(), c, driver.Query{Text: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	c0 := openFake(t, srv, "&max_bytes_billed=0", false)
	if _, err := drain(context.Background(), c0, driver.Query{Text: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dryRuns) != 0 || len(f.runs) != 2 {
		t.Fatalf("dry runs %d, runs %d; want 0 and 2", len(f.dryRuns), len(f.runs))
	}
	if f.runs[0]["maximumBytesBilled"] != "5000" {
		t.Errorf("maximumBytesBilled = %v, want 5000", f.runs[0]["maximumBytesBilled"])
	}
	if v, ok := f.runs[1]["maximumBytesBilled"]; ok {
		t.Errorf("max_bytes_billed=0 still sent maximumBytesBilled %v", v)
	}
}

// A value over the cap is cut and the result says so.
func TestLargeValueWarning(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.bigValue = true
	c := openFake(t, srv, "", false)
	rs, err := c.Query(context.Background(), driver.Query{Text: "SELECT s FROM t"})
	if err != nil {
		t.Fatal(err)
	}
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %d, err %v", len(rows), err)
	}
	if s, _ := rows[0][0].AsText(); len(s) != 1<<20 {
		t.Errorf("value length %d, want %d", len(s), 1<<20)
	}
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "1 MiB") {
		t.Errorf("warnings = %q", st.Warnings)
	}
}

// DML on a writable connection reports the rows it changed, from the
// job's statistics (the emulator reports no statement type for DML, so
// this is only checked here).
func TestDMLRowsAffected(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.dml = true
	c := openFake(t, srv, "", false)
	rs, err := c.Query(context.Background(), driver.Query{Text: "DELETE FROM t WHERE x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, st, err := driver.Collect(context.Background(), rs, 0); err != nil || st.RowsAffected != 3 || len(rs.Columns()) != 0 {
		t.Errorf("DML: columns %v, stats %+v, err %v; want no columns and RowsAffected 3", rs.Columns(), st, err)
	}
}

func TestExplainIsADryRun(t *testing.T) {
	f, srv := newFakeAPI(t)
	c := openFake(t, srv, "", false)
	x, _ := driver.As[driver.Explainer](c)
	plan, err := x.Explain(context.Background(), driver.Query{Text: "SELECT * FROM tobira_people"}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SELECT", "2048", "2.0 KiB", "id INTEGER REQUIRED", "tags STRING REPEATED", "p.ds.tobira_people", "1073741824"} {
		if !strings.Contains(plan.Text, want) {
			t.Errorf("plan text lacks %q:\n%s", want, plan.Text)
		}
	}
	if _, err := x.Explain(context.Background(), driver.Query{Text: "SELECT 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Analyze: want ErrUnsupported, got %v", err)
	}
	if _, err := x.Explain(context.Background(), driver.Query{Text: "DELETE FROM t WHERE true", ReadOnly: true}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only explain of a write: want ErrReadOnly, got %v", err)
	}
	if _, run := f.counts(); run != 0 {
		t.Errorf("explain ran %d real jobs", run)
	}
}

// Cancelling a running query returns at once and asks the server to
// cancel the job, so it stops billing.
func TestCancelStopsTheJob(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.blockResults = true
	c := openFake(t, srv, "", false)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-f.resultsCalled
		cancel()
	}()
	start := time.Now()
	_, err := drain(ctx, c, driver.Query{Text: "SELECT 1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("cancel took %v", d)
	}
	_ = c.Close() // waits for the background cancel
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cancels) != 1 || !strings.HasPrefix(f.cancels[0], "tobira_") {
		t.Errorf("cancelled jobs = %q, want one tobira_ job", f.cancels)
	}
}

// A well-formed service-account key that the token endpoint rejects is
// ErrAuth, and no part of the key reaches the error.
func TestServiceAccountRejectedIsErrAuth(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "invalid_grant", "error_description": "Invalid JWT Signature."})
	}))
	t.Cleanup(tokenSrv.Close)
	_, apiSrv := newFakeAPI(t)
	key := testServiceAccountKey(t)
	pw := serviceAccountJSON(t, key, tokenSrv.URL)
	cfg := driver.ConnConfig{DSN: "bigquery://p/ds", Password: driver.NewSecret(pw)}
	c, err := openWith(context.Background(), cfg, option.WithEndpoint(apiSrv.URL))
	if err == nil {
		_ = c.Close()
		t.Fatal("Open with a rejected key succeeded")
	}
	if !errors.Is(err, driver.ErrAuth) {
		t.Errorf("want ErrAuth, got %v", err)
	}
	sa, _ := parseServiceAccount(pw)
	for _, frag := range append(secretFragments(sa), sa.PrivateKey) {
		if len(frag) >= 8 && strings.Contains(err.Error(), frag) {
			t.Errorf("error holds key material %q: %v", frag[:8], err)
		}
	}
}

func testServiceAccountKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func serviceAccountJSON(t *testing.T, k *rsa.PrivateKey, tokenURI string) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "p",
		"private_key_id": "0123456789abcdef0123456789abcdef01234567",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "tobira@p.iam.gserviceaccount.com",
		"client_id":      "1234567890",
		"token_uri":      tokenURI,
	})
	return string(b)
}
