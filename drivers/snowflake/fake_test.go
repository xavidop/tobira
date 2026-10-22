package snowflake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// These tests run the driver against restFake, an httptest stand-in for
// Snowflake's REST API: they check what the driver sends and how it reads
// the answers, not Snowflake itself.

func openFake(t *testing.T, f *restFake, cfg driver.ConnConfig) driver.Conn {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = f.dsn("")
	}
	if cfg.Password.IsZero() {
		cfg.Password = driver.NewSecret("pw-123456")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := New().Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ctx20(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestFakeLoginIsHardened(t *testing.T) {
	f := newRestFake(t)
	c := openFake(t, f, driver.ConnConfig{})
	login := f.lastLogin()
	if login == nil {
		t.Fatal("no login request")
	}
	if login["LOGIN_NAME"] != "ada" || login["PASSWORD"] != "pw-123456" || login["ACCOUNT_NAME"] != "acct" {
		t.Errorf("login identity: %v %v %v", login["LOGIN_NAME"], login["PASSWORD"], login["ACCOUNT_NAME"])
	}
	sp, _ := login["SESSION_PARAMETERS"].(map[string]any)
	if fmt.Sprint(sp[multiStatementCountParam]) != "1" {
		t.Errorf("SESSION_PARAMETERS = %v, want MULTI_STATEMENT_COUNT 1", sp)
	}
	if _, _, err := driver.Collect(ctx20(t), mustQuery(t, c, driver.Query{Text: probeText}), 0); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.params[len(f.params)-1]
	f.mu.Unlock()
	if fmt.Sprint(last[multiStatementCountParam]) != "1" {
		t.Errorf("query parameters = %v, want MULTI_STATEMENT_COUNT 1 on every request", last)
	}
}

func mustQuery(t *testing.T, c driver.Querier, q driver.Query) driver.ResultSet {
	t.Helper()
	rs, err := c.Query(ctx20(t), q)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	return rs
}

func TestFakeAuthenticators(t *testing.T) {
	f := newRestFake(t)
	for _, c := range []struct{ param, authenticator, token string }{
		{"authenticator=oauth", "OAUTH", "oauth-secret-token"},
		{"authenticator=programmatic_access_token", "PROGRAMMATIC_ACCESS_TOKEN", "pat-secret-token"},
	} {
		openFake(t, f, driver.ConnConfig{DSN: f.dsn(c.param), Password: driver.NewSecret(c.token)})
		login := f.lastLogin()
		if login["AUTHENTICATOR"] != c.authenticator || login["TOKEN"] != c.token {
			t.Errorf("%s: login AUTHENTICATOR=%v TOKEN=%v", c.param, login["AUTHENTICATOR"], login["TOKEN"])
		}
		if pw, _ := login["PASSWORD"].(string); pw != "" {
			t.Errorf("%s: the token was also sent as a password", c.param)
		}
	}

	pemText, key := testKeyPEM(t, "PRIVATE KEY")
	f.mu.Lock()
	f.pubKey = &key.PublicKey
	f.mu.Unlock()
	openFake(t, f, driver.ConnConfig{DSN: f.dsn("authenticator=snowflake_jwt"), Password: driver.NewSecret(pemText)})
	if login := f.lastLogin(); login["AUTHENTICATOR"] != "SNOWFLAKE_JWT" {
		t.Errorf("key pair: AUTHENTICATOR=%v", login["AUTHENTICATOR"])
	}
	// A different key is refused by the fake's signature check.
	other, _ := testKeyPEM(t, "PRIVATE KEY")
	_, err := New().Open(ctx20(t), driver.ConnConfig{DSN: f.dsn("authenticator=snowflake_jwt"), Password: driver.NewSecret(other)})
	if !errors.Is(err, driver.ErrAuth) {
		t.Errorf("key pair with the wrong key: %v, want ErrAuth", err)
	}
	if err != nil && strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("the error quotes the key: %v", err)
	}
}

func TestFakeBadAuth(t *testing.T) {
	f := newRestFake(t)
	f.setPassword("right-password")
	_, err := New().Open(ctx20(t), driver.ConnConfig{DSN: f.dsn(""), Password: driver.NewSecret("wrong-password-123")})
	if !errors.Is(err, driver.ErrAuth) {
		t.Fatalf("Open with a wrong password: %v, want ErrAuth", err)
	}
	if strings.Contains(err.Error(), "wrong-password-123") {
		t.Errorf("the error quotes the password: %v", err)
	}
}

// The read-only guard is lexical and runs before anything is sent: a refused
// text never reaches the server, whatever the server would do with it.
func TestReadOnlyRefusalSendsNothing(t *testing.T) {
	f := newRestFake(t)
	ro := openFake(t, f, driver.ConnConfig{ReadOnly: true})
	rw := openFake(t, f, driver.ConnConfig{})
	writes := []string{"DELETE FROM t", "SELECT 1; DELETE FROM t", "SHOW TABLES ->> DELETE FROM t", "SELECT $1 FROM @stage_marker"}
	for _, w := range writes {
		if _, err := ro.Query(ctx20(t), driver.Query{Text: w}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only conn, %q: %v, want ErrReadOnly", w, err)
		}
		if _, err := rw.Query(ctx20(t), driver.Query{Text: w, ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Query.ReadOnly, %q: %v, want ErrReadOnly", w, err)
		}
		x := rw.(driver.Explainer)
		if _, err := x.Explain(ctx20(t), driver.Query{Text: w, ReadOnly: true}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Explain with Query.ReadOnly, %q: %v, want ErrReadOnly", w, err)
		}
	}
	b := rw.(driver.Browser)
	if _, err := b.Browse(ctx20(t), driver.CollectionRef{Namespace: "DB.PUBLIC", Name: "T"}, driver.BrowseOptions{ReadOnly: true, Filter: driver.Filter{Raw: "1 = 1) ; DELETE FROM t --"}}); err == nil {
		t.Error("Browse with a raw filter that escapes its parentheses succeeded")
	}
	for _, s := range f.sent() {
		if strings.Contains(s, "DELETE") || strings.Contains(s, "stage_marker") {
			t.Errorf("a refused text reached the server: %q", s)
		}
	}
}

func TestFakeQueryResult(t *testing.T) {
	f := newRestFake(t)
	f.set("SELECT * FROM t", fakeResult{
		cols: []fakeCol{{"ID", "fixed", 0}, {"NAME", "text", 0}, {"SCORE", "fixed", 2}, {"OK", "boolean", 0}, {"R", "real", 0}},
		rows: [][]any{{"1", "ada", "12.50", "true", "1.5"}, {"2", nil, nil, "false", nil}, {"3", "c", "0.10", nil, "2"}},
	})
	c := openFake(t, f, driver.ConnConfig{})
	rs := mustQuery(t, c, driver.Query{Text: "SELECT * FROM t", Limit: 2})
	cols := rs.Columns()
	var names []string
	for _, col := range cols {
		names = append(names, col.Name+":"+col.Type.String())
	}
	if got := strings.Join(names, " "); got != "ID:int NAME:string SCORE:decimal OK:bool R:float" {
		t.Errorf("columns = %s", got)
	}
	rows, st, err := driver.Collect(ctx20(t), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !st.Truncated {
		t.Fatalf("rows = %d truncated = %v, want 2 and true", len(rows), st.Truncated)
	}
	want := []driver.Row{
		{driver.Int(1), driver.String("ada"), driver.Decimal("12.50"), driver.Bool(true), driver.Float(1.5)},
		{driver.Int(2), driver.Null(), driver.Null(), driver.Bool(false), driver.Null()},
	}
	for i := range want {
		for j := range want[i] {
			if !drivertest.EqualValue(want[i][j], rows[i][j]) || want[i][j].String() != rows[i][j].String() {
				t.Errorf("row %d col %d = %#v, want %#v", i, j, rows[i][j], want[i][j])
			}
		}
	}
}

func TestFakeExplain(t *testing.T) {
	f := newRestFake(t)
	f.set("EXPLAIN USING TEXT\nSELECT * FROM t", fakeResult{
		cols: []fakeCol{{"content", "text", 0}},
		rows: [][]any{{"GlobalStats:\n    partitionsTotal=1\nResult\n  TableScan T"}},
	})
	c := openFake(t, f, driver.ConnConfig{})
	x := c.(driver.Explainer)
	plan, err := x.Explain(ctx20(t), driver.Query{Text: "SELECT * FROM t"}, driver.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Format != "text" || !strings.Contains(plan.Text, "TableScan T") {
		t.Errorf("plan = %+v", plan)
	}
	n := len(f.sent())
	if _, err := x.Explain(ctx20(t), driver.Query{Text: "SELECT * FROM t"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Explain Analyze: %v, want ErrUnsupported", err)
	}
	if _, err := x.Explain(ctx20(t), driver.Query{Text: "DELETE FROM t", ReadOnly: true}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Explain Analyze of a write with ReadOnly: %v, want ErrReadOnly", err)
	}
	if len(f.sent()) != n {
		t.Errorf("Analyze sent queries: %v", f.sent()[n:])
	}
}

func TestFakeCancelMidQuery(t *testing.T) {
	f := newRestFake(t)
	f.blockOn("SELECT slow")
	c := openFake(t, f, driver.ConnConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.Query(ctx, driver.Query{Text: "SELECT slow"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled query: %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("cancelling took %v", d)
	}
	if err := c.Ping(ctx20(t)); err != nil {
		t.Errorf("Ping after a cancelled query: %v", err)
	}
	// gosnowflake asks the server to abort the query.
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		n := f.aborts
		f.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Error("no abort-request reached the server after the cancel")
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// gosnowflake logs through a process-wide logger that writes to stderr by
// default. The driver replaces it, so nothing reaches the terminal, even
// when a login and a query fail.
func TestFakeWritesNothingToStderr(t *testing.T) {
	f := newRestFake(t)
	f.setPassword("right-password")
	out := drivertest.Stderr(t, func() {
		if c, err := New().Open(context.Background(), driver.ConnConfig{DSN: f.dsn(""), Password: driver.NewSecret("wrong-password")}); err == nil {
			_ = c.Close()
		}
		f.setPassword("")
		c, err := New().Open(context.Background(), driver.ConnConfig{DSN: f.dsn(""), Password: driver.NewSecret("x")})
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = c.Query(context.Background(), driver.Query{Text: "SELECT unknown"})
		_ = c.Close()
	})
	if out != "" {
		t.Errorf("the driver wrote to stderr:\n%s", out)
	}
}

// A Snowflake client config file (sf_client_config.json, found through
// SF_CLIENT_CONFIG_FILE or the home directory) can turn gosnowflake's
// "easy logging" on, writing DEBUG logs to a directory. The driver's
// logger cannot be replaced that way.
func TestFakeIgnoresEasyLoggingConfig(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	conf := filepath.Join(dir, "sf_client_config.json")
	if err := os.WriteFile(conf, []byte(`{"common": {"log_level": "TRACE", "log_path": "`+logDir+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SF_CLIENT_CONFIG_FILE", conf)
	f := newRestFake(t)
	out := drivertest.Stderr(t, func() {
		c := openFake(t, f, driver.ConnConfig{})
		_, _ = c.Query(context.Background(), driver.Query{Text: "SELECT unknown"})
		_ = c.Close()
	})
	if out != "" {
		t.Errorf("the driver wrote to stderr:\n%s", out)
	}
	var files []string
	_ = filepath.Walk(logDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) > 0 {
		t.Errorf("easy logging wrote %v", files)
	}
}

func TestFakeClose(t *testing.T) {
	f := newRestFake(t)
	c := openFake(t, f, driver.ConnConfig{})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := c.Query(context.Background(), driver.Query{Text: "SELECT 1"}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Query after Close: %v, want ErrConnection", err)
	}
}

func TestFakeTxRefusals(t *testing.T) {
	f := newRestFake(t)
	f.set("SELECT 1", fakeResult{cols: []fakeCol{{"1", "fixed", 0}}, rows: [][]any{{"1"}}})
	c := openFake(t, f, driver.ConnConfig{})
	tr := c.(driver.Transactor)
	if _, err := tr.Begin(ctx20(t), driver.TxOptions{Isolation: "serializable"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Begin(serializable): %v, want ErrUnsupported", err)
	}
	tx, err := tr.Begin(ctx20(t), driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, q := range []string{"COMMIT", "CREATE TABLE x (a INT)", "DELETE FROM t"} {
		if _, err := tx.Query(ctx20(t), driver.Query{Text: q}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only Tx, %q: %v, want ErrReadOnly", q, err)
		}
	}
	if _, _, err := driver.Collect(ctx20(t), mustQuery(t, tx, driver.Query{Text: "SELECT 1"}), 0); err != nil {
		t.Errorf("read in a read-only Tx: %v", err)
	}
	if _, err := tx.Query(ctx20(t), driver.Query{Text: "SELECT 1", Namespace: "OTHER.S"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("another namespace inside a Tx: %v, want ErrUnsupported", err)
	}
	for _, s := range f.sent() {
		if strings.Contains(s, "DELETE") || strings.Contains(s, "CREATE") || s == "COMMIT" {
			t.Errorf("a refused statement reached the server: %q", s)
		}
	}

	ro := openFake(t, f, driver.ConnConfig{ReadOnly: true})
	if _, err := ro.(driver.Transactor).Begin(ctx20(t), driver.TxOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-write Begin on a read-only conn: %v, want ErrReadOnly", err)
	}
	m := ro.(driver.Mutator)
	if err := m.Delete(ctx20(t), driver.CollectionRef{Namespace: "DB.PUBLIC", Name: "T"}, driver.Object{{Key: "id", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Delete on a read-only conn: %v, want ErrReadOnly", err)
	}
}

// gosnowflake's platform detection (cloud metadata probes and an AWS STS
// call with local credentials) and its native minicore are switched off
// unless the user set the switches.
func TestSDKProbesDisabled(t *testing.T) {
	for k := range sdkSwitches {
		if v, ok := os.LookupEnv(k); !ok || v == "" {
			t.Errorf("%s is not set after init", k)
		}
	}
	t.Setenv("SF_DISABLE_MINICORE", "false")
	disableSDKProbes()
	if os.Getenv("SF_DISABLE_MINICORE") != "false" {
		t.Error("disableSDKProbes overrode a value the user set")
	}
}
