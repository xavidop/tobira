package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xavidop/tobira/driver"
)

// fakeConnect records the config Open built and returns a lazy (unconnected)
// pool, so no server is needed.
type fakeConnect struct {
	cfg *pgxpool.Config
	err error
	n   int
}

func (f *fakeConnect) connect(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	f.n++
	f.cfg = cfg
	if f.err != nil {
		return nil, f.err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

func openWith(t *testing.T, f *fakeConnect, cfg driver.ConnConfig) (driver.Conn, error) {
	t.Helper()
	c, err := (&postgresDriver{connect: f.connect}).Open(context.Background(), cfg)
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "postgres" || info.Kind != driver.KindSQL || info.Dialect != "postgresql" {
		t.Errorf("info = %+v", info)
	}
	if len(info.Schemes) != 2 || info.Schemes[0] != "postgres" || info.Schemes[1] != "postgresql" {
		t.Errorf("schemes = %v", info.Schemes)
	}
	want := map[driver.Capability]bool{driver.CapBrowser: true, driver.CapMutator: true, driver.CapExplainer: true, driver.CapTransactor: true, driver.CapCompleter: true, driver.CapExporter: true, driver.CapWatcher: true, driver.CapVectorSearcher: true, driver.CapReadOnly: true}
	if len(info.Capabilities) != len(want) {
		t.Fatalf("caps = %v", info.Capabilities)
	}
	for _, c := range info.Capabilities {
		if !want[c] {
			t.Errorf("unexpected capability %v", c)
		}
	}
	if _, ok := driver.Default.Lookup(Name); !ok {
		t.Error("not registered in driver.Default")
	}
}

func TestOpenBuildsConfig(t *testing.T) {
	const dsn = "postgres://alice:fromdsn@db.example:5433/app?sslmode=disable"
	tests := []struct {
		name       string
		cfg        driver.ConnConfig
		wantParams map[string]string
		wantPass   string
	}{
		{"defaults", driver.ConnConfig{DSN: dsn},
			map[string]string{"application_name": "tobira"}, "fromdsn"},
		{"password override", driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("override")},
			map[string]string{"application_name": "tobira"}, "override"},
		{"read only", driver.ConnConfig{DSN: dsn, ReadOnly: true},
			map[string]string{"application_name": "tobira", "default_transaction_read_only": "on"}, "fromdsn"},
		{"options", driver.ConnConfig{DSN: dsn, Options: map[string]string{
			"application_name": "myapp", "search_path": "s1,public", "statement_timeout": "5s"}},
			map[string]string{"application_name": "myapp", "search_path": "s1,public", "statement_timeout": "5s"}, "fromdsn"},
		{"dsn application_name wins over default", driver.ConnConfig{DSN: dsn + "&application_name=fromdsn"},
			map[string]string{"application_name": "fromdsn"}, "fromdsn"},
		{"keyword dsn", driver.ConnConfig{DSN: "host=h user=u dbname=d password=pw"},
			map[string]string{"application_name": "tobira"}, "pw"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeConnect{}
			if _, err := openWith(t, f, tt.cfg); err != nil {
				t.Fatal(err)
			}
			if f.n != 1 {
				t.Fatalf("connect called %d times", f.n)
			}
			rp := f.cfg.ConnConfig.RuntimeParams
			for k, v := range tt.wantParams {
				if rp[k] != v {
					t.Errorf("RuntimeParams[%s] = %q, want %q", k, rp[k], v)
				}
			}
			if _, ok := rp["default_transaction_read_only"]; !ok && tt.cfg.ReadOnly {
				t.Error("read-only param missing")
			}
			if !tt.cfg.ReadOnly && rp["default_transaction_read_only"] != "" {
				t.Error("read-only param set on a writable connection")
			}
			if f.cfg.ConnConfig.Password != tt.wantPass {
				t.Errorf("password = %q, want %q", f.cfg.ConnConfig.Password, tt.wantPass)
			}
		})
	}
}

func TestOptionsAllowList(t *testing.T) {
	const dsn = "postgres://u@h/db"
	for _, key := range []string{"application_name", "search_path", "statement_timeout"} {
		f := &fakeConnect{}
		if _, err := openWith(t, f, driver.ConnConfig{DSN: dsn, Options: map[string]string{key: "v"}}); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	for _, key := range []string{"default_transaction_read_only", "role", "sslmode", "bogus"} {
		f := &fakeConnect{}
		_, err := openWith(t, f, driver.ConnConfig{DSN: dsn, Options: map[string]string{key: "off"}})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", key, err)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: error does not name the key: %v", key, err)
		}
		if f.n != 0 {
			t.Errorf("%s: connected despite invalid options", key)
		}
	}
}

func TestOpenReadOnlyCannotBeLiftedByOptions(t *testing.T) {
	f := &fakeConnect{}
	_, err := openWith(t, f, driver.ConnConfig{
		DSN: "postgres://u@h/db?default_transaction_read_only=off", ReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.cfg.ConnConfig.RuntimeParams["default_transaction_read_only"]; got != "on" {
		t.Errorf("read-only param = %q, want on", got)
	}
}

func TestOpenReadOnlyCaseVariants(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u@h/db?DEFAULT_TRANSACTION_READ_ONLY=off",
		"postgres://u@h/db?Default_Transaction_Read_Only=off&default_transaction_read_only=off",
	} {
		for range 20 { // map order is random
			f := &fakeConnect{}
			if _, err := openWith(t, f, driver.ConnConfig{DSN: dsn, ReadOnly: true}); err != nil {
				t.Fatal(err)
			}
			rp := f.cfg.ConnConfig.RuntimeParams
			n := 0
			for k, v := range rp {
				if strings.EqualFold(k, "default_transaction_read_only") {
					n++
					if k != "default_transaction_read_only" || v != "on" {
						t.Fatalf("%s: %s=%s", dsn, k, v)
					}
				}
			}
			if n != 1 {
				t.Fatalf("%s: %d read-only keys", dsn, n)
			}
		}
	}
	for _, opt := range []string{
		"-c default_transaction_read_only=off",
		"-c DEFAULT_TRANSACTION_READ_ONLY=off",
		"--default-transaction-read-only=off",
		"-c transaction_read_only=off",
	} {
		f := &fakeConnect{}
		_, err := openWith(t, f, driver.ConnConfig{DSN: "postgres://u@h/db?options=" + url.QueryEscape(opt), ReadOnly: true})
		if !errors.Is(err, driver.ErrUnsupported) || f.n != 0 {
			t.Errorf("%q: err = %v", opt, err)
		}
	}
}

func TestScrubKeepsChain(t *testing.T) {
	pe := &pgconn.PgError{Code: "28P01", Message: "password hunter2 rejected"}
	cfg := driver.ConnConfig{DSN: "postgres://u:hunter2@h/db"}
	err := scrubError(mapError(context.Background(), "open", pe), cfg, "")
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("leaks: %v", err)
	}
	var got *pgconn.PgError
	if !errors.As(err, &got) || !errors.Is(err, driver.ErrAuth) {
		t.Errorf("chain lost: %v", err)
	}
}

func TestOpenErrors(t *testing.T) {
	pgErr := func(code string) error { return &pgconn.PgError{Code: code, Message: "boom"} }
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"auth", fmt.Errorf("failed to connect: %w", pgErr("28P01")), driver.ErrAuth},
		{"refused", refusedConnectError(t, context.Background()), driver.ErrConnection},
		{"missing db", pgErr("3D000"), driver.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := openWith(t, &fakeConnect{err: tt.err}, driver.ConnConfig{DSN: "postgres://u@h/db"})
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			var de *driver.Error
			if !errors.As(err, &de) || de.Op != "open" || de.Driver != Name {
				t.Errorf("err = %#v", err)
			}
		})
	}
}

func TestOpenInvalidDSNNeverLeaksPassword(t *testing.T) {
	const secret = "s3cr3t-p@ss"
	dsns := []string{
		"postgres://alice:s3cr3t-p@ss@db.example:notaport/app",
		"postgres://alice:s3cr3t-p%ZZss@db.example/app",
		"host=h port=notaport user=alice password=s3cr3t-p@ss dbname=d",
		"postgres://alice:s3cr3t-p@ss@db.example/app?sslmode=bogus",
	}
	for _, dsn := range dsns {
		f := &fakeConnect{}
		_, err := openWith(t, f, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret(secret)})
		if err == nil {
			t.Errorf("%q: want error", dsn)
			continue
		}
		if f.n != 0 {
			t.Errorf("%q: connected", dsn)
		}
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%q: kind = %v, want ErrSyntax", dsn, driver.KindName(err))
		}
		for _, leak := range []string{secret, "s3cr3t", "%ZZss"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%q: error leaks password: %v", dsn, err)
			}
		}
		// The unwrapped chain must not carry it either.
		for e := errors.Unwrap(err); e != nil; e = errors.Unwrap(e) {
			if strings.Contains(e.Error(), "s3cr3t") {
				t.Errorf("%q: wrapped error leaks password: %v", dsn, e)
			}
		}
	}
}

func TestOpenConnectErrorNeverLeaksPassword(t *testing.T) {
	const secret = "hunter2"
	f := &fakeConnect{err: errors.New("failed to connect: password=hunter2 rejected")}
	_, err := openWith(t, f, driver.ConnConfig{DSN: "postgres://u:hunter2@h/db"})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("err = %v", err)
	}
}

func TestConnLifecycle(t *testing.T) {
	c, err := openWith(t, &fakeConnect{}, driver.ConnConfig{DSN: "postgres://u@h/db"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if err := c.Ping(context.Background()); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Ping after Close = %v, want ErrConnection", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c2, err := openWith(t, &fakeConnect{}, driver.ConnConfig{DSN: "postgres://u@h/db"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Ping with canceled ctx = %v", err)
	}
}

func TestCallsAfterCloseOrCancel(t *testing.T) {
	c, err := openWith(t, &fakeConnect{}, driver.ConnConfig{DSN: "postgres://u@h/db"})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	calls := map[string]func(context.Context) error{
		"Query":       func(ctx context.Context) error { _, err := c.Query(ctx, driver.Query{Text: "select 1"}); return err },
		"Namespaces":  func(ctx context.Context) error { _, err := c.Namespaces(ctx); return err },
		"Collections": func(ctx context.Context) error { _, err := c.Collections(ctx, driver.NamespaceRef{}); return err },
		"Describe": func(ctx context.Context) error {
			_, err := c.Describe(ctx, driver.CollectionRef{Name: "t"})
			return err
		},
	}
	for name, call := range calls {
		if err := call(canceled); !errors.Is(err, context.Canceled) {
			t.Errorf("%s with canceled ctx = %v", name, err)
		}
	}
	_ = c.Close()
	for name, call := range calls {
		if err := call(context.Background()); !errors.Is(err, driver.ErrConnection) {
			t.Errorf("%s after Close = %v, want ErrConnection", name, err)
		}
	}
}

func TestLeadingKeyword(t *testing.T) {
	tests := []struct{ text, want string }{
		{"select 1", "SELECT"},
		{"  \n\tWith x as (select 1) select * from x", "WITH"},
		{"-- note\nvalues (1)", "VALUES"},
		{"/* a */ /* nested /* b */ still comment */ table t", "TABLE"},
		{"(select 1) union (select 2)", "("},
		{"delete from t", "DELETE"},
		{"/* unterminated select 1", ""},
		{"-- only a comment", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := leadingKeyword(tt.text); got != tt.want {
			t.Errorf("leadingKeyword(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestQueryExecModeIsExtendedProtocol(t *testing.T) {
	const base = "postgres://u@h/db"
	tests := []struct {
		name    string
		dsn     string
		want    pgx.QueryExecMode
		wantErr bool
	}{
		{"default forced to cache_describe", base, pgx.QueryExecModeCacheDescribe, false},
		{"explicit cache_statement is upgraded", base + "?default_query_exec_mode=cache_statement", pgx.QueryExecModeCacheDescribe, false},
		{"describe_exec kept", base + "?default_query_exec_mode=describe_exec", pgx.QueryExecModeDescribeExec, false},
		{"cache_describe kept", base + "?default_query_exec_mode=cache_describe", pgx.QueryExecModeCacheDescribe, false},
		{"simple protocol refused", base + "?default_query_exec_mode=simple_protocol", 0, true},
		{"exec refused", base + "?default_query_exec_mode=exec", 0, true},
		{"cache disabled falls back to describe_exec", base + "?description_cache_capacity=0", pgx.QueryExecModeDescribeExec, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pcfg, _, err := buildConfig(driver.ConnConfig{DSN: tt.dsn}, "")
			if tt.wantErr {
				if !errors.Is(err, driver.ErrUnsupported) {
					t.Fatalf("err = %v, want ErrUnsupported", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := pcfg.ConnConfig.DefaultQueryExecMode; got != tt.want {
				t.Errorf("mode = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPoolResetsSessionAndCancelsGently(t *testing.T) {
	f := &fakeConnect{}
	if _, err := openWith(t, f, driver.ConnConfig{DSN: "postgres://u@h/db"}); err != nil {
		t.Fatal(err)
	}
	if f.cfg.AfterRelease == nil {
		t.Error("AfterRelease (DISCARD ALL) not set")
	}
	if f.cfg.ConnConfig.BuildContextWatcherHandler == nil {
		t.Error("cancel-request context watcher not set")
	}
}

func TestIsPrepareTransaction(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"PREPARE TRANSACTION 'x'", true},
		{"  /* c */ prepare\n-- c\n transaction 'x'", true},
		{"prepare p as select 1", false},
		{"prepare", false},
		{"prepare/**/transaction 'x'", true},
		{"prepared transaction", false},
		{"select 'prepare transaction'", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isPrepareTransaction(tt.text); got != tt.want {
			t.Errorf("isPrepareTransaction(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestReadOnlyMapsActiveSQLTransaction(t *testing.T) {
	tests := []struct {
		name string
		ro   bool
		msg  string
		want bool
		// routine is the server routine that raised the error.
		routine string
	}{
		{"read-write mode, read-only path", true, "transaction read-write mode must be set before any query", true, "call_bool_check_hook"},
		{"read-write mode, read-write path", false, "transaction read-write mode must be set before any query", false, "call_bool_check_hook"},
		{"isolation level, read-only path", true, "SET TRANSACTION ISOLATION LEVEL must be called before any query", false, "call_enum_check_hook"},
		{"vacuum, read-only path", true, "VACUUM cannot run inside a transaction block", false, "PreventInTransactionBlock"},
		{"subtransaction, read-only path", true, "cannot set transaction read-write mode inside a read-only transaction", true, "call_bool_check_hook"},
		{"translated message, hook routine", true, "la modalidad de lectura-escritura debe fijarse antes de cualquier consulta", true, "call_bool_check_hook"},
		{"deferrable, hook routine", true, "SET TRANSACTION [NOT] DEFERRABLE must be called before any query", false, "call_bool_check_hook"},
		{"translated message, other routine", true, "otro error", false, "call_enum_check_hook"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := &pgconn.PgError{Code: "25001", Message: tt.msg, Routine: tt.routine}
			err := (&resultSet{ro: tt.ro}).mapErr(context.Background(), pe)
			if got := errors.Is(err, driver.ErrReadOnly); got != tt.want {
				t.Errorf("ErrReadOnly = %v, want %v (err %v)", got, tt.want, err)
			}
			if !tt.want && driver.KindOf(err) != nil {
				t.Errorf("kind = %v, want none", driver.KindOf(err))
			}
		})
	}
}
