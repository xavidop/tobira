//go:build integration

package bigquery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	bq "cloud.google.com/go/bigquery"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"
	"google.golang.org/api/option"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

const (
	// testImage is the BigQuery emulator, pinned exactly (multi-arch).
	testImage   = "ghcr.io/goccy/bigquery-emulator:0.8.1"
	testProject = "tobira"
	testDataset = "tobira"
	// conformanceDataset holds the conformance kit's copy of People, apart
	// from the driver's own tests.
	conformanceDataset = "conformance"
	nsDataset          = "tobira_ns"
)

// emulatorAddr is host:port of the emulator shared by every test; testDSN
// is a bigquery:// DSN on the tobira dataset.
var emulatorAddr, testDSN string

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, testImage,
		testcontainers.WithCmd("--project="+testProject, "--dataset="+testDataset),
		testcontainers.WithExposedPorts("9050/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/projects/"+testProject+"/datasets").WithPort("9050/tcp").WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start bigquery emulator:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	if emulatorAddr, err = ctr.PortEndpoint(ctx, "9050/tcp", ""); err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	testDSN = "bigquery://" + testProject + "/" + testDataset + "?emulator=" + emulatorAddr
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	baseline := goleak.IgnoreCurrent()
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(baseline, goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop")); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			return 1
		}
	}
	return code
}

// adminClient is a plain client library connection to the emulator, used
// to seed and to verify, bypassing the driver.
func adminClient(ctx context.Context) (*bq.Client, error) {
	return bq.NewClient(ctx, testProject, option.WithEndpoint("http://"+emulatorAddr), option.WithoutAuthentication(),
		option.WithLogger(discard))
}

// peopleSchema is People in BigQuery: tags is a JSON column, id a
// REQUIRED INT64 declared as the (unenforced) primary key.
func peopleSchema() bq.Schema {
	return bq.Schema{
		{Name: "id", Type: bq.IntegerFieldType, Required: true},
		{Name: "name", Type: bq.StringFieldType},
		{Name: "email", Type: bq.StringFieldType},
		{Name: "age", Type: bq.IntegerFieldType},
		{Name: "score", Type: bq.NumericFieldType},
		{Name: "active", Type: bq.BooleanFieldType},
		{Name: "created_at", Type: bq.TimestampFieldType},
		{Name: "tags", Type: bq.JSONFieldType},
	}
}

// peopleInsert renders People as one INSERT into table.
func peopleInsert(table string) string {
	var sb strings.Builder
	sb.WriteString("INSERT INTO " + table + " (id, name, email, age, score, active, created_at, tags) VALUES")
	for i, r := range drivertest.People.Rows {
		id, _ := r[0].AsInt()
		name, _ := r[1].AsText()
		email, _ := r[2].AsText()
		age := "NULL"
		if n, ok := r[3].AsInt(); ok {
			age = fmt.Sprint(n)
		}
		score, _ := r[4].AsText()
		active, _ := r[5].AsBool()
		ts, _ := r[6].AsTime()
		tags, _ := r[7].AsJSON()
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "\n(%d, %s, %s, %s, NUMERIC %s, %t, TIMESTAMP %s, JSON %s)",
			id, sqlString(name), sqlString(email), age, sqlString(score), active,
			sqlString(ts.UTC().Format("2006-01-02 15:04:05+00")), sqlString(string(tags)))
	}
	return sb.String()
}

func sqlString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func createPeople(ctx context.Context, c *bq.Client, dataset string) error {
	t := c.Dataset(dataset).Table("tobira_people")
	if err := t.Create(ctx, &bq.TableMetadata{
		Schema:           peopleSchema(),
		Description:      "the conformance People",
		TableConstraints: &bq.TableConstraints{PrimaryKey: &bq.PrimaryKey{Columns: []string{"id"}}},
	}); err != nil {
		return fmt.Errorf("create %s.tobira_people: %w", dataset, err)
	}
	return runAdmin(ctx, c, peopleInsert("`"+dataset+".tobira_people`"))
}

func runAdmin(ctx context.Context, c *bq.Client, sql string) error {
	job, err := c.Query(sql).Run(ctx)
	if err != nil {
		return err
	}
	status, err := job.Wait(ctx)
	if err != nil {
		return err
	}
	return status.Err()
}

func seed(ctx context.Context) error {
	c, err := adminClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	for _, ds := range []string{conformanceDataset, nsDataset} {
		if err := c.Dataset(ds).Create(ctx, &bq.DatasetMetadata{}); err != nil {
			return fmt.Errorf("create dataset %s: %w", ds, err)
		}
	}
	for _, ds := range []string{testDataset, conformanceDataset} {
		if err := createPeople(ctx, c, ds); err != nil {
			return err
		}
	}
	kinds := c.Dataset(testDataset).Table("tobira_kinds")
	if err := kinds.Create(ctx, &bq.TableMetadata{Schema: bq.Schema{
		{Name: "id", Type: bq.IntegerFieldType, Required: true, Description: "row id"},
		{Name: "d", Type: bq.DateFieldType},
		{Name: "dt", Type: bq.DateTimeFieldType},
		{Name: "tm", Type: bq.TimeFieldType},
		{Name: "b", Type: bq.BytesFieldType},
		{Name: "f", Type: bq.FloatFieldType},
		{Name: "big", Type: bq.BigNumericFieldType},
		{Name: "arr", Type: bq.IntegerFieldType, Repeated: true},
		{Name: "rec", Type: bq.RecordFieldType, Schema: bq.Schema{
			{Name: "label", Type: bq.StringFieldType},
			{Name: "tags", Type: bq.StringFieldType, Repeated: true},
		}},
	}}); err != nil {
		return fmt.Errorf("create tobira_kinds: %w", err)
	}
	stmts := []string{
		"INSERT INTO `" + testDataset + ".tobira_kinds` (id, d, dt, tm, b, f, big, arr, rec) VALUES " +
			"(1, DATE '2026-03-04', DATETIME '2026-03-04 05:06:07', TIME '05:06:07.123456', b'\\x00\\x01', 1.5, " +
			"BIGNUMERIC '123456789012345678901234567890.5', [1, 2, 3], STRUCT('first' AS label, ['x', 'y'] AS tags)), " +
			"(2, NULL, NULL, NULL, NULL, NULL, NULL, [], NULL)",
		"CREATE TABLE `" + nsDataset + ".things` (id INT64, label STRING)",
		"INSERT INTO `" + nsDataset + ".things` (id, label) VALUES (1, 'in-ns')",
	}
	for _, s := range stmts {
		if err := runAdmin(ctx, c, s); err != nil {
			return fmt.Errorf("%.60s…: %w", s, err)
		}
	}
	// The emulator refuses CREATE VIEW on a dataset-qualified name; the
	// API creates it.
	view := c.Dataset(testDataset).Table("tobira_people_view")
	if err := view.Create(ctx, &bq.TableMetadata{ViewQuery: "SELECT id, name FROM `" + testDataset + ".tobira_people`"}); err != nil {
		return fmt.Errorf("create view: %w", err)
	}
	return nil
}

// open opens the driver on dataset (testDataset when empty).
func open(t testing.TB, dataset string, readOnly bool) driver.Conn {
	t.Helper()
	if dataset == "" {
		dataset = testDataset
	}
	dsn := "bigquery://" + testProject + "/" + dataset + "?emulator=" + emulatorAddr
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn, ReadOnly: readOnly})
	if err != nil {
		t.Fatalf("Open(%s): %v", dsn, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func queryAll(t *testing.T, c driver.Conn, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Query(ctxT(t), q)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	defer func() { _ = rs.Close() }()
	rows, st, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatalf("Query(%q): %v", q.Text, err)
	}
	return rs.Columns(), rows, st
}

func peopleCount(t *testing.T, dataset string) int64 {
	t.Helper()
	c, err := adminClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	it, err := c.Query("SELECT COUNT(*) FROM `" + dataset + ".tobira_people`").Read(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var row []bq.Value
	if err := it.Next(&row); err != nil {
		t.Fatal(err)
	}
	return row[0].(int64)
}

func TestOpenUnknownDatasetIsNotFound(t *testing.T) {
	_, err := New().Open(ctxT(t), driver.ConnConfig{DSN: "bigquery://" + testProject + "/nope?emulator=" + emulatorAddr})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Open on a missing dataset: want ErrNotFound, got %v", err)
	}
}
