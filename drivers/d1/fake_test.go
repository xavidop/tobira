package d1

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// fakeD1 is a container-free stand-in for the Cloudflare D1 REST API:
// POST /client/v4/accounts/{account}/d1/database/{database}/query and /raw,
// with a bearer token, backed by an in-process modernc SQLite file. It
// answers in D1's envelope ({"result": [...], "errors": [...], "messages":
// [], "success": …}) and serialises values the way D1's JavaScript does:
// numbers as JSON numbers (so a REAL 3.0 comes back as 3), blobs as arrays of
// byte values. It has NOT been checked against Cloudflare; it encodes our
// reading of the API documentation.
type fakeD1 struct {
	t                        testing.TB
	db                       *sql.DB
	srv                      *httptest.Server
	account, database, token string

	mu       sync.Mutex
	requests []fakeRequest
	// hook, when set, may answer a request itself (return true).
	hook func(w http.ResponseWriter, r *http.Request, body []byte) bool
}

// fakeRequest records one request the fake received.
type fakeRequest struct {
	Path   string
	Auth   string
	SQL    string
	Params []any
}

const (
	fakeAccount  = "0123456789abcdef0123456789abcdef"
	fakeDatabase = "6f1c2b3a-0d4e-4f5a-9b8c-7d6e5f4a3b2c"
	fakeToken    = "cf-test-token-0123456789"
)

// newFakeD1 starts a fake seeded with drivertest's People.
func newFakeD1(t testing.TB) *fakeD1 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d1.db")
	// _texttotime=0 keeps DATETIME text as text, as D1 returns it.
	db, err := sql.Open("sqlite", "file:"+path+"?_texttotime=0&_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range drivertest.SQLSeed("sqlite") {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	f := &fakeD1{t: t, db: db, account: fakeAccount, database: fakeDatabase, token: fakeToken}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.srv.Close()
		_ = db.Close()
	})
	return f
}

// endpoint is the API base URL the driver is pointed at.
func (f *fakeD1) endpoint() string { return f.srv.URL + "/client/v4" }

func (f *fakeD1) dsn() string {
	return "d1://" + f.account + "/" + f.database + "?endpoint=" + url.QueryEscape(f.endpoint())
}

func (f *fakeD1) cfg(readOnly bool) driver.ConnConfig {
	return driver.ConnConfig{DSN: f.dsn(), Password: driver.NewSecret(f.token), ReadOnly: readOnly}
}

// open opens a driver connection to the fake.
func (f *fakeD1) open(t testing.TB, readOnly bool) *conn {
	t.Helper()
	c, err := New().Open(context.Background(), f.cfg(readOnly))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

func (f *fakeD1) log() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func (f *fakeD1) setHook(h func(w http.ResponseWriter, r *http.Request, body []byte) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = h
}

// mustExec runs SQL straight on the backing database.
func (f *fakeD1) mustExec(t testing.TB, q string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// count reads one integer straight from the backing database.
func (f *fakeD1) count(t testing.TB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeEnvelope(w http.ResponseWriter, status int, result any, errs ...cfError) {
	if errs == nil {
		errs = []cfError{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"result": result, "errors": errs, "messages": []any{}, "success": status == http.StatusOK,
	})
}

func (f *fakeD1) serve(w http.ResponseWriter, r *http.Request) {
	body, err := readAll(r)
	if err != nil {
		return
	}
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook != nil && hook(w, r, body) {
		return
	}
	prefix := "/client/v4/accounts/" + f.account + "/d1/database/"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	if r.Method != http.MethodPost || !ok {
		writeEnvelope(w, http.StatusNotFound, nil, cfError{7003, "Could not route to " + r.URL.Path + ", perhaps your object identifier is invalid?"})
		return
	}
	db, endpoint, _ := strings.Cut(rest, "/")
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		writeEnvelope(w, http.StatusForbidden, nil, cfError{10000, "Authentication error"})
		return
	}
	if db != f.database {
		writeEnvelope(w, http.StatusNotFound, nil, cfError{7404, "The database " + db + " could not be found"})
		return
	}
	if endpoint != "query" && endpoint != "raw" {
		writeEnvelope(w, http.StatusNotFound, nil, cfError{7003, "No route for that URI"})
		return
	}
	var req struct {
		SQL    string `json:"sql"`
		Params []any  `json:"params"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil || req.SQL == "" {
		writeEnvelope(w, http.StatusBadRequest, nil, cfError{7400, "The request is malformed"})
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), SQL: req.SQL, Params: req.Params})
	f.mu.Unlock()
	args := make([]any, len(req.Params))
	for i, p := range req.Params {
		switch x := p.(type) {
		case json.Number:
			if n, err := x.Int64(); err == nil {
				args[i] = n
			} else if fl, err := x.Float64(); err == nil {
				args[i] = fl
			}
		case bool:
			args[i] = map[bool]int64{true: 1, false: 0}[x]
		case string, nil:
			args[i] = x
		default:
			writeEnvelope(w, http.StatusBadRequest, nil, cfError{7400, "unsupported parameter type"})
			return
		}
	}
	res, err := f.run(r.Context(), req.SQL, args)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeEnvelope(w, http.StatusBadRequest, []any{}, cfError{7500, d1Message(err)})
		return
	}
	if endpoint == "raw" {
		writeEnvelope(w, http.StatusOK, []any{map[string]any{
			"results": map[string]any{"columns": res.cols, "rows": res.rows},
			"success": true, "meta": res.meta,
		}})
		return
	}
	objs := make([]map[string]any, len(res.rows))
	for i, row := range res.rows {
		objs[i] = map[string]any{}
		for j, c := range res.cols {
			objs[i][c] = row[j]
		}
	}
	writeEnvelope(w, http.StatusOK, []any{map[string]any{"results": objs, "success": true, "meta": res.meta}})
}

func readAll(r *http.Request) ([]byte, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(r.Body)
	return b.Bytes(), err
}

type fakeResult struct {
	cols []string
	rows [][]any
	meta map[string]any
}

// run executes one statement on its own connection and reports D1's meta.
func (f *fakeD1) run(ctx context.Context, q string, args []any) (*fakeResult, error) {
	start := time.Now()
	sc, err := f.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sc.Close() }()
	var before int64
	if err := sc.QueryRowContext(ctx, "select total_changes()").Scan(&before); err != nil {
		return nil, err
	}
	rows, err := sc.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	cols, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	res := &fakeResult{cols: cols, rows: [][]any{}}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			_ = rows.Close()
			return nil, err
		}
		for i, v := range vals {
			vals[i] = jsValue(v)
		}
		res.rows = append(res.rows, vals)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	var after, lastID int64
	if err := sc.QueryRowContext(ctx, "select total_changes(), last_insert_rowid()").Scan(&after, &lastID); err != nil {
		return nil, err
	}
	res.meta = map[string]any{
		"changed_db": after != before, "changes": after - before, "last_row_id": lastID,
		"duration": float64(time.Since(start).Microseconds()) / 1000, "rows_read": len(res.rows), "rows_written": after - before,
		"served_by_region": "FAKE", "size_after": 0,
	}
	return res, nil
}

// jsValue is v as D1's JavaScript would serialise it.
func jsValue(v any) any {
	switch x := v.(type) {
	case []byte:
		out := make([]int, len(x))
		for i, b := range x {
			out[i] = int(b)
		}
		return out
	case time.Time: // not expected with _texttotime=0
		return x.UTC().Format("2006-01-02 15:04:05")
	}
	return v
}

// d1Message renders a SQLite error the way D1 reports it: "<message>:
// SQLITE_<CODE>".
func d1Message(err error) string {
	var se *sqlite.Error
	if errors.As(err, &se) {
		msg := se.Error()
		if i := strings.Index(msg, ": "); i >= 0 && strings.HasSuffix(msg, ")") {
			msg = msg[i+2:] // drop modernc's "<code text>: " prefix
		}
		if i := strings.LastIndex(msg, " ("); i >= 0 {
			msg = msg[:i]
		}
		code := "SQLITE_ERROR"
		switch se.Code() & 0xff {
		case 19:
			code = "SQLITE_CONSTRAINT"
		case 8:
			code = "SQLITE_READONLY"
		case 5:
			code = "SQLITE_BUSY"
		case 23:
			code = "SQLITE_AUTH"
		}
		return fmt.Sprintf("%s: %s", msg, code)
	}
	return err.Error() + ": SQLITE_ERROR"
}
