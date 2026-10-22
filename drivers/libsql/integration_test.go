//go:build integration

package libsql

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// testImage is the sqld (libsql-server) image the integration suite runs
// against, pinned exactly.
const testImage = "ghcr.io/tursodatabase/libsql-server:v0.24.33"

// testURL is the server's http:// URL; rwToken and roToken are JWTs signed
// with the key the server was started with (claims "a": "rw" and "a": "ro").
var testURL, rwToken, roToken string

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

// leakBaseline is taken after the container exists, so testcontainers' and
// the docker client's goroutines are not counted.
var leakBaseline goleak.Option

func runMain(m *testing.M) int {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "key:", err)
		return 1
	}
	rwToken, roToken = signJWT(priv, "rw"), signJWT(priv, "ro")
	ctr, err := testcontainers.Run(ctx, testImage,
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithEnv(map[string]string{"SQLD_AUTH_JWT_KEY": base64.RawURLEncoding.EncodeToString(pub)}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/health").WithPort("8080/tcp").WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start sqld container:", err)
		if ctr != nil {
			_ = testcontainers.TerminateContainer(ctr)
		}
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	endpoint, err := ctr.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	testURL = endpoint
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	leakBaseline = goleak.IgnoreCurrent()
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(leakBaseline); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			return 1
		}
	}
	return code
}

// signJWT returns an EdDSA JWT with the access claim a ("rw" or "ro").
func signJWT(priv ed25519.PrivateKey, a string) string {
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	body := enc.EncodeToString([]byte(`{"a":"` + a + `"}`))
	return head + "." + body + "." + enc.EncodeToString(ed25519.Sign(priv, []byte(head+"."+body)))
}

func seed(ctx context.Context) error {
	c, err := New().Open(ctx, cfgFor(false, rwToken))
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	stmts := append(drivertest.SQLSeed("sqlite"),
		`CREATE VIEW tobira_people_view AS SELECT id, name FROM tobira_people`,
		`CREATE TABLE tobira_orders (id INTEGER PRIMARY KEY AUTOINCREMENT, person_id INTEGER NOT NULL REFERENCES tobira_people(id),
		  total DECIMAL(8,2) DEFAULT 0, doubled REAL GENERATED ALWAYS AS (total * 2) VIRTUAL, note TEXT DEFAULT 'n/a')`,
		`CREATE INDEX tobira_orders_person_total ON tobira_orders (person_id, total)`,
		`CREATE UNIQUE INDEX tobira_orders_note ON tobira_orders (note)`,
		`CREATE TABLE tobira_desc (k INTEGER PRIMARY KEY DESC, v TEXT)`,
		`CREATE TABLE tobira_norowid (k INTEGER PRIMARY KEY, v TEXT) WITHOUT ROWID`,
	)
	for _, s := range stmts {
		rs, err := c.Query(ctx, driver.Query{Text: s})
		if err != nil {
			return fmt.Errorf("%.60s: %w", s, err)
		}
		_ = rs.Close()
	}
	return nil
}

// cfgFor is a ConnConfig for the test server: the token through
// ConnConfig.Password, as a password ref resolves.
func cfgFor(readOnly bool, token string) driver.ConnConfig {
	cfg := driver.ConnConfig{Driver: Name, DSN: testURL, ReadOnly: readOnly}
	if token != "" {
		cfg.Password = driver.NewSecret(token)
	}
	return cfg
}

// openLibsql opens a connection with the read-write token.
func openLibsql(t *testing.T, readOnly bool) *conn {
	t.Helper()
	c, err := New().Open(context.Background(), cfgFor(readOnly, rwToken))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

var scratchSeq atomic.Int64

// scratch copies People into a private table, so tests can write.
func scratch(t *testing.T, c driver.Conn) string {
	t.Helper()
	name := fmt.Sprintf("tobira_scratch_%d", scratchSeq.Add(1))
	mustExec(t, c, "CREATE TABLE "+name+" AS SELECT * FROM tobira_people")
	t.Cleanup(func() {
		cc, err := New().Open(context.Background(), cfgFor(false, rwToken))
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = cc.Close() }()
		mustExec(t, cc, "DROP TABLE "+name)
	})
	return name
}

func mustExec(t *testing.T, c driver.Querier, text string, params ...driver.Value) {
	t.Helper()
	rs, err := c.Query(context.Background(), driver.Query{Text: text, Params: params})
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	_ = rs.Close()
}

func run(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Row, driver.Stats, []driver.Column) {
	t.Helper()
	rs, err := c.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows, st, cols
}

func count(t *testing.T, c driver.Querier, table string) int64 {
	t.Helper()
	rows, _, _ := run(t, c, driver.Query{Text: "select count(*) from " + table})
	n, _ := rows[0][0].AsInt()
	return n
}
