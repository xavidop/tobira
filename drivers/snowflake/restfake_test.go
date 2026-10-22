package snowflake

import (
	"compress/gzip"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// restFake is an httptest server speaking the part of Snowflake's REST API
// that gosnowflake uses: login-request, query-request (JSON rowsets),
// abort-request, session delete and telemetry. It covers what the fakesnow
// container cannot: EXPLAIN, a login that checks credentials, and the
// authenticators. It is not Snowflake.
type restFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	logins   []map[string]any // the "data" object of each login request
	texts    []string         // sqlText of each query request, in order
	params   []map[string]any // "parameters" of each query request
	results  map[string]fakeResult
	password string         // the accepted password; "" accepts any
	pubKey   *rsa.PublicKey // when set, a SNOWFLAKE_JWT login must be signed by it
	block    map[string]bool
	aborts   int
}

// fakeResult is one canned answer: columns and JSON rows, or an error.
type fakeResult struct {
	cols     []fakeCol
	rows     [][]any // each cell a string or nil, as Snowflake's JSON rowset sends it
	errCode  string  // "001003": answer with this error instead
	sqlState string
	message  string
}

type fakeCol struct {
	name  string
	typ   string // fixed, text, real, boolean, variant …
	scale int
}

// The answer to the driver's own probe at Open.
const probeText = "SELECT CURRENT_DATABASE(), CURRENT_SCHEMA()"

func newRestFake(t *testing.T) *restFake {
	t.Helper()
	f := &restFake{results: map[string]fakeResult{}, block: map[string]bool{}}
	f.results[probeText] = fakeResult{
		cols: []fakeCol{{"CURRENT_DATABASE()", "text", 0}, {"CURRENT_SCHEMA()", "text", 0}},
		rows: [][]any{{"DB", "PUBLIC"}},
	}
	// gosnowflake's Ping and transaction statements.
	f.results["SELECT 1"] = fakeResult{cols: []fakeCol{{"1", "fixed", 0}}, rows: [][]any{{"1"}}}
	for _, s := range []string{"BEGIN", "COMMIT", "ROLLBACK"} {
		f.results[s] = fakeResult{cols: []fakeCol{{"status", "text", 0}}, rows: [][]any{{"Statement executed successfully."}}}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/session/v1/login-request", f.login)
	mux.HandleFunc("/queries/v1/query-request", f.query)
	mux.HandleFunc("/queries/v1/abort-request", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.aborts++
		f.mu.Unlock()
		writeJSON(w, map[string]any{"success": true, "data": nil})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSON(w, map[string]any{"success": true, "data": nil, "code": nil, "message": nil})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// dsn returns a DSN that points the driver at the fake.
func (f *restFake) dsn(params string) string {
	u, _ := url.Parse(f.srv.URL)
	s := "snowflake://ada@acct/DB/PUBLIC?protocol=http&host=" + u.Hostname() + "&port=" + u.Port()
	if params != "" {
		s += "&" + params
	}
	return s
}

func (f *restFake) set(text string, r fakeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[text] = r
}

func (f *restFake) setPassword(pw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.password = pw
}

func (f *restFake) blockOn(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block[text] = true
}

// sent returns the query texts received so far.
func (f *restFake) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

func (f *restFake) lastLogin() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logins) == 0 {
		return nil
	}
	return f.logins[len(f.logins)-1]
}

func readBody(r *http.Request) []byte {
	var rd io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		if gz, err := gzip.NewReader(r.Body); err == nil {
			rd = gz
		}
	}
	b, _ := io.ReadAll(rd)
	return b
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *restFake) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(readBody(r), &body)
	f.mu.Lock()
	f.logins = append(f.logins, body.Data)
	password, pub := f.password, f.pubKey
	f.mu.Unlock()
	ok := true
	switch auth, _ := body.Data["AUTHENTICATOR"].(string); {
	case auth == "SNOWFLAKE_JWT" && pub != nil:
		tok, _ := body.Data["TOKEN"].(string)
		ok = verifyJWT(tok, pub)
	case password != "":
		pw, _ := body.Data["PASSWORD"].(string)
		ok = pw == password
	}
	if !ok {
		writeJSON(w, map[string]any{
			"data":    map[string]any{"nextAction": "RETRY_LOGIN", "authnMethod": "USERNAME_PASSWORD"},
			"code":    "390100",
			"message": "Incorrect username or password was specified.",
			"success": false,
		})
		return
	}
	writeJSON(w, map[string]any{
		"data": map[string]any{
			"masterToken": "master-token", "token": "session-token",
			"validityInSeconds": 3600, "masterValidityInSeconds": 14400,
			"displayUserName": "ADA", "serverVersion": "9.40.0", "firstLogin": false,
			"healthCheckInterval": 45, "sessionId": 1,
			"parameters":  []any{map[string]any{"name": "AUTOCOMMIT", "value": true}},
			"sessionInfo": map[string]any{"databaseName": "DB", "schemaName": "PUBLIC", "warehouseName": "WH", "roleName": "R"},
		},
		"code": nil, "message": nil, "success": true,
	})
}

func (f *restFake) query(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SQLText    string         `json:"sqlText"`
		Parameters map[string]any `json:"parameters"`
	}
	_ = json.Unmarshal(readBody(r), &body)
	f.mu.Lock()
	f.texts = append(f.texts, body.SQLText)
	f.params = append(f.params, body.Parameters)
	res, found := f.results[body.SQLText]
	blocked := f.block[body.SQLText]
	f.mu.Unlock()
	if blocked {
		<-r.Context().Done()
		return
	}
	if !found {
		res = fakeResult{errCode: "001003", sqlState: "42000", message: "SQL compilation error: syntax error line 1 at position 0 unexpected 'x'."}
	}
	if res.errCode != "" {
		writeJSON(w, map[string]any{
			"data": map[string]any{
				"internalError": false, "errorCode": res.errCode, "age": 0, "sqlState": res.sqlState,
				"queryId": "01-fake-error", "line": -1, "pos": -1, "type": "COMPILATION",
			},
			"code": res.errCode, "message": res.message, "success": false,
		})
		return
	}
	rowtype := make([]any, len(res.cols))
	for i, c := range res.cols {
		rowtype[i] = map[string]any{
			"name": c.name, "database": "DB", "schema": "PUBLIC", "table": "T", "type": c.typ,
			"scale": c.scale, "precision": 38, "length": nil, "byteLength": nil, "nullable": true, "collation": nil,
		}
	}
	rows := res.rows
	if rows == nil {
		rows = [][]any{}
	}
	writeJSON(w, map[string]any{
		"data": map[string]any{
			"parameters": []any{}, "rowtype": rowtype, "rowset": rows,
			"total": len(rows), "returned": len(rows), "queryId": "01-fake-" + strconv.Itoa(len(f.sent())),
			"finalDatabaseName": "DB", "finalSchemaName": "PUBLIC", "finalWarehouseName": "WH", "finalRoleName": "R",
			"numberOfBinds": 0, "arrayBindSupported": false, "statementTypeId": 4096, "version": 1,
			"queryResultFormat": "json",
		},
		"code": nil, "message": nil, "success": true,
	})
}

// verifyJWT checks an RS256 JWT against pub.
func verifyJWT(tok string, pub *rsa.PublicKey) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) == nil
}
