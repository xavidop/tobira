//go:build integration

package influxdb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// image is the InfluxDB 3 Core image the integration suite runs against.
const image = "influxdb:3.6-core"

// testDB is the seeded database; otherDB holds one more table, to prove
// Query.Namespace and Namespaces.
const (
	testDB  = "tobira"
	otherDB = "tobira_other"
)

// testHostPort is the server's host:port and testToken its admin token
// (the server runs with authentication on).
var testHostPort, testToken string

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

var leakBaseline goleak.Option

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, image,
		testcontainers.WithCmd("influxdb3", "serve", "--node-id", "tobira", "--object-store", "memory"),
		testcontainers.WithExposedPorts("8181/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("startup time").WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start influxdb container:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	testHostPort, err = ctr.PortEndpoint(ctx, "8181/tcp", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	// gRPC tries ::1 first for "localhost", and Docker Desktop's port
	// forward on ::1 can stall the first Flight call for seconds.
	testHostPort = strings.Replace(testHostPort, "localhost:", "127.0.0.1:", 1)
	if testToken, err = adminToken(ctx, ctr); err != nil {
		fmt.Fprintln(os.Stderr, "admin token:", err)
		return 1
	}
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

var tokenRE = regexp.MustCompile(`apiv3_[A-Za-z0-9_-]+`)

// adminToken creates the server's first admin token inside the container.
func adminToken(ctx context.Context, ctr testcontainers.Container) (string, error) {
	_, out, err := ctr.Exec(ctx, []string{"influxdb3", "create", "token", "--admin"})
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(out)
	if err != nil {
		return "", err
	}
	tok := tokenRE.FindString(string(b))
	if tok == "" {
		return "", fmt.Errorf("no token in %q", b)
	}
	return tok, nil
}

// writeLP writes line protocol to db with nanosecond timestamps.
func writeLP(ctx context.Context, db, lines string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+testHostPort+"/api/v3/write_lp?precision=nanosecond&db="+db, strings.NewReader(lines))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("write %s: %s: %s", db, resp.Status, body)
	}
	return nil
}

// lpString quotes a line-protocol string field value.
func lpString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// peopleLP renders People as line protocol. InfluxDB 3 has no decimal, JSON
// or second timestamp type: score is a float field, tags a string field,
// created_at an integer field of Unix nanoseconds (and also the row's
// time), and a NULL age is a missing field.
func peopleLP() string {
	var b bytes.Buffer
	for _, r := range drivertest.People.Rows {
		id, _ := r[0].AsInt()
		name, _ := r[1].AsText()
		email, _ := r[2].AsText()
		score, _ := r[4].AsText()
		active, _ := r[5].AsBool()
		created, _ := r[6].AsTime()
		tags, _ := r[7].AsJSON()
		fields := []string{"id=" + strconv.FormatInt(id, 10) + "i", "name=" + lpString(name), "email=" + lpString(email)}
		if age, ok := r[3].AsInt(); ok {
			fields = append(fields, "age="+strconv.FormatInt(age, 10)+"i")
		}
		fields = append(fields, "score="+score, "active="+strconv.FormatBool(active), "tags="+lpString(string(tags)),
			"created_at="+strconv.FormatInt(created.UnixNano(), 10)+"i")
		fmt.Fprintf(&b, "%s %s %d\n", drivertest.PeopleCollection(), strings.Join(fields, ","), created.UnixNano())
	}
	return b.String()
}

// cpuLP renders the time-series fixture as line protocol.
func cpuLP() string {
	var b bytes.Buffer
	for _, r := range drivertest.TimeSeriesDataset().Rows {
		ts, _ := r[0].AsTime()
		host, _ := r[1].AsText()
		user, _ := r[2].AsFloat()
		sys, _ := r[3].AsFloat()
		fmt.Fprintf(&b, "%s,host=%s usage_user=%s,usage_system=%s %d\n", drivertest.TimeSeriesCollection, host,
			strconv.FormatFloat(user, 'g', -1, 64), strconv.FormatFloat(sys, 'g', -1, 64), ts.UnixNano())
	}
	return b.String()
}

func seed(ctx context.Context) error {
	if err := writeLP(ctx, testDB, peopleLP()); err != nil {
		return err
	}
	if err := writeLP(ctx, testDB, cpuLP()); err != nil {
		return err
	}
	// A table with every field type, and a tag holding a quote.
	types := `types,site=o'hare f=1.5,i=-3i,u=7u,s="x",b=true 1767225600000000000` + "\n"
	if err := writeLP(ctx, testDB, types); err != nil {
		return err
	}
	if err := writeLP(ctx, testDB, sparseLP); err != nil {
		return err
	}
	return writeLP(ctx, otherDB, "things label=\"in-other\" 1767225600000000000\n")
}

// sparseLP is a measurement whose fields are written on different rows, as
// InfluxDB data often is: a appears in hours 0, 2 and 4 only, b in every
// hour. In hour 4 the last row lacks a, so last(a) must still be 5.
const sparseLP = `sparse a=1,b=1 1767225600000000000
sparse b=2 1767229200000000000
sparse a=3,b=3 1767232800000000000
sparse b=4 1767236400000000000
sparse a=5,b=5 1767240000000000000
sparse b=6 1767241800000000000
`

// testDSN is the DSN of the seeded database, token included.
func testDSN(params string) string {
	dsn := "influxdb3://:" + testToken + "@" + testHostPort + "/" + testDB
	if params != "" {
		dsn += "?" + params
	}
	return dsn
}

func open(t testing.TB, cfg driver.ConnConfig) driver.Conn {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = testDSN("")
	}
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func query(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	ctx := ctxT(t)
	rs, err := c.Query(ctx, q)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatalf("Query(%q): reading: %v", q.Text, err)
	}
	return cols, rows, st
}

func colIndex(t *testing.T, cols []driver.Column, name string) int {
	t.Helper()
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	t.Fatalf("no column %q in %v", name, cols)
	return -1
}
