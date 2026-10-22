//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// cockroachImage is the CockroachDB server the cockroachdb flavor runs
// against: a single node in secure mode, so password authentication is real
// (the image creates certificates, the user and the database at start).
const cockroachImage = "cockroachdb/cockroach:v26.2.8"

// startCockroach starts a CockroachDB node, seeds People into database
// tobira and returns a postgres:// DSN for user tobira. The container ends
// with the test.
func startCockroach(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, cockroachImage,
		testcontainers.WithCmd("start-single-node"),
		testcontainers.WithEnv(map[string]string{
			"COCKROACH_DATABASE": "tobira", "COCKROACH_USER": "tobira", "COCKROACH_PASSWORD": "tobira",
		}),
		testcontainers.WithExposedPorts("26257/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("end running init files").WithStartupTimeout(3*time.Minute)),
	)
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	if err != nil {
		t.Fatal("start cockroach:", err)
	}
	hostPort, err := ctr.PortEndpoint(ctx, "26257/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	dsn := "postgres://tobira:tobira@" + hostPort + "/tobira?sslmode=require"
	seed, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seed.Close(ctx) }()
	for _, s := range drivertest.SQLSeed("postgres") {
		if _, err := seed.Exec(ctx, s); err != nil {
			t.Fatalf("seed %.60s: %v", s, err)
		}
	}
	return dsn
}

// withScheme swaps the scheme of a URL DSN.
func withScheme(dsn, scheme string) string {
	_, rest, _ := strings.Cut(dsn, "://")
	return scheme + "://" + rest
}

// flavorConformance runs the Strict kit against dsn with the flavored driver.
func flavorConformance(t *testing.T, f flavor, dsn string) {
	t.Helper()
	d := newFlavored(f)
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := d.Open(context.Background(), driver.ConnConfig{DSN: dsn, ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			if got := c.(*conn).flavor; got != f {
				t.Fatalf("flavor = %q, want %q", got, f)
			}
			return c
		}
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       d,
		Open:         open(false),
		OpenReadOnly: open(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := d.Open(ctx, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict:  true,
		Fixture: peopleFixture(),
	})
}

func TestCockroachDB(t *testing.T) {
	dsn := startCockroach(t)

	t.Run("Conformance", func(t *testing.T) {
		flavorConformance(t, flavorCockroach, withScheme(dsn, "cockroachdb"))
	})

	t.Run("DetectedThroughPostgres", func(t *testing.T) {
		c, err := New().Open(ctxT(t), driver.ConnConfig{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if got := c.(*conn).flavor; got != flavorCockroach {
			t.Fatalf("detected %q, want cockroachdb", got)
		}
		if _, ok := driver.As[driver.Watcher](c); ok {
			t.Error("CockroachDB has no LISTEN, yet Watcher is offered")
		}
		if _, ok := driver.As[driver.VectorSearcher](c); ok {
			t.Error("VectorSearcher offered on CockroachDB")
		}
		if _, ok := driver.As[driver.Mutator](c); !ok {
			t.Error("Mutator missing on CockroachDB")
		}
	})

	t.Run("ReadOnlyGuard", func(t *testing.T) {
		c, err := newFlavored(flavorCockroach).Open(ctxT(t), driver.ConnConfig{DSN: dsn, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		for _, q := range []string{
			"export into csv 'nodelocal://1/tobira' from select * from tobira_people",
			"backup into 'nodelocal://1/tobira'",
			"create changefeed for tobira_people into 'null://'",
			"select crdb_internal.force_error('XX000', 'boom')",
			"select crdb_internal.reset_sql_stats()",
			`select "crdb_internal"."force_retry"('1s')`,
			"set transaction read write",
			"set cluster setting sql.defaults.default_int_size = 4",
			"cancel query '00000000000000000000000000000001'",
			"with d as (delete from tobira_people where id = 1 returning id) select * from d",
			"explain analyze delete from tobira_people where id = 1",
			"explain analyze (debug) select * from tobira_people",
			"EXPLAIN ANALYZE VERBOSE (DEBUG) SELECT 1",
			"explain (verbose, debug) select 1",
			"show backups in 'nodelocal://1/tobira'",
			"show backup from latest in 'nodelocal://1/tobira'",
			"SELECT * FROM [SHOW BACKUPS IN 'nodelocal://1/tobira']",
			"with b as (select * from [show backups in 'nodelocal://1/tobira']) select * from b",
			"select * from (select * from [show backup from latest in 'nodelocal://1/tobira']) s",
			"select * into tobira_copy from tobira_people",
		} {
			rs, err := c.Query(ctxT(t), driver.Query{Text: q})
			if err == nil {
				_, _, err = driver.Collect(ctxT(t), rs, 0)
			}
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%q: err = %v, want ErrReadOnly", q, err)
			}
		}
		// A bracketed SHOW without a location stays readable.
		if rows, _, _ := collect(t, c, driver.Query{Text: "select * from [show tables]"}); len(rows) == 0 {
			t.Error("select * from [show tables] returned no rows")
		}
		// Explain with Analyze cannot smuggle an option block in through the
		// query text either.
		x, _ := driver.As[driver.Explainer](c)
		if _, err := x.Explain(ctxT(t), driver.Query{Text: "(DEBUG) SELECT 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Explain(Analyze, (DEBUG) SELECT 1): err = %v, want ErrReadOnly", err)
		}
		if _, err := x.Explain(ctxT(t), driver.Query{Text: "select * from tobira_people"}, driver.ExplainOptions{Analyze: true}); err != nil {
			t.Errorf("Explain(Analyze) of a read on a read-only connection: %v", err)
		}
		// The rows survived every attempt, and the startup read-only default
		// survived DISCARD ALL on the reused pooled connections.
		rows, _, _ := collect(t, c, driver.Query{Text: "show default_transaction_read_only"})
		if len(rows) != 1 || rows[0][0].String() != "on" {
			t.Errorf("default_transaction_read_only = %v, want on from the startup packet", rows)
		}
		rows, _, _ = collect(t, c, driver.Query{Text: "select count(*) from tobira_people"})
		if n, _ := rows[0][0].AsInt(); n != int64(len(drivertest.People.Rows)) {
			t.Errorf("tobira_people has %d rows", n)
		}
	})

	t.Run("ExplainTree", func(t *testing.T) {
		c, err := newFlavored(flavorCockroach).Open(ctxT(t), driver.ConnConfig{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		x, _ := driver.As[driver.Explainer](c)
		plan, err := x.Explain(ctxT(t), driver.Query{Text: "select * from tobira_people where id = 3"}, driver.ExplainOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Root == nil || plan.Root.Label == "" || !strings.Contains(plan.Text, "•") {
			t.Fatalf("plan = %+v", plan)
		}
		plan, err = x.Explain(ctxT(t), driver.Query{Text: "update tobira_people set name = 'gone' where id = 3"}, driver.ExplainOptions{Analyze: true})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Root == nil {
			t.Errorf("analyze plan has no tree: %q", plan.Text)
		}
		rows, _, _ := collect(t, c, driver.Query{Text: "select name from tobira_people where id = 3"})
		if rows[0][0].String() == "gone" {
			t.Error("EXPLAIN ANALYZE persisted its update")
		}
	})

	t.Run("Describe", func(t *testing.T) {
		c, err := newFlavored(flavorCockroach).Open(ctxT(t), driver.ConnConfig{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		b := c
		nss, err := b.Namespaces(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, ns := range nss {
			if ns.Name == "crdb_internal" || ns.Name == "pg_extension" {
				t.Errorf("system schema %s listed", ns.Name)
			}
		}
		s, err := b.Describe(ctxT(t), driver.CollectionRef{Namespace: "public", Name: "tobira_people"})
		if err != nil {
			t.Fatal(err)
		}
		if s.SizeBytes != -1 || !slices.Equal(s.PrimaryKey, []string{"id"}) {
			t.Errorf("size %d, key %v", s.SizeBytes, s.PrimaryKey)
		}
	})
}

// redshiftEmulation stands in for the Redshift system views on PostgreSQL:
// SVV_TABLE_INFO (rows and size in 1 MB blocks, only tables) and PG_TABLE_DEF
// (only tables on search_path), both in pg_catalog like on Redshift, with the
// distribution and sort keys read from a helper table.
var redshiftEmulation = []string{
	"SET allow_system_table_mods = on",
	`CREATE TABLE pg_catalog.tobira_rs_keys (relid oid, attname name, distkey boolean, sortkey int)`,
	`CREATE VIEW pg_catalog.svv_table_info AS
	   SELECT n.nspname::text AS "schema", c.relname::text AS "table", c.oid AS table_id,
	          (pg_total_relation_size(c.oid) / 1048576)::bigint AS size,
	          greatest(c.reltuples, 0)::numeric(38,0) AS tbl_rows, 'AUTO(EVEN)'::text AS diststyle
	   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	   WHERE c.relkind = 'r' AND n.nspname NOT IN ('pg_catalog', 'information_schema')`,
	`CREATE VIEW pg_catalog.pg_table_def AS
	   SELECT n.nspname AS schemaname, c.relname AS tablename, a.attname AS "column",
	          format_type(a.atttypid, a.atttypmod) AS type, 'none'::text AS encoding,
	          coalesce(k.distkey, false) AS distkey, coalesce(k.sortkey, 0) AS sortkey, a.attnotnull AS notnull
	   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	   JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
	   LEFT JOIN pg_catalog.tobira_rs_keys k ON k.relid = c.oid AND k.attname = a.attname
	   WHERE c.relkind = 'r' AND pg_table_is_visible(c.oid)`,
	"RESET allow_system_table_mods",
	// A table outside search_path with keys, a composite primary key and a
	// two-column foreign key.
	"CREATE SCHEMA tobira_rs",
	"CREATE TABLE tobira_rs.accounts (tenant text, id integer, PRIMARY KEY (tenant, id))",
	`CREATE TABLE tobira_rs.events (tenant text NOT NULL, account integer, seq integer, note text,
	   PRIMARY KEY (seq), CONSTRAINT events_account FOREIGN KEY (tenant, account) REFERENCES tobira_rs.accounts (tenant, id))`,
	"COMMENT ON TABLE tobira_rs.events IS 'an event log'",
	"INSERT INTO tobira_rs.accounts VALUES ('acme', 1)",
	"INSERT INTO tobira_rs.events VALUES ('acme', 1, 1, 'a'), ('acme', 1, 2, 'b')",
	"ANALYZE tobira_rs.events",
	`INSERT INTO pg_catalog.tobira_rs_keys VALUES
	   ('tobira_rs.events'::regclass, 'tenant', true, 1), ('tobira_rs.events'::regclass, 'seq', false, 2)`,
}

func execAll(t *testing.T, dsn string, stmts []string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	for _, s := range stmts {
		if _, err := c.Exec(ctx, s); err != nil {
			t.Fatalf("%.70s: %v", s, err)
		}
	}
}

// TestRedshiftFlavor runs the redshift flavor's SQL against PostgreSQL 17
// with the Redshift system views emulated. No Redshift cluster is involved.
func TestRedshiftFlavor(t *testing.T) {
	dsn := seededDB(t, "tobira_redshift")
	execAll(t, dsn, redshiftEmulation)
	rsDSN := withScheme(dsn, "redshift")

	t.Run("Conformance", func(t *testing.T) { flavorConformance(t, flavorRedshift, rsDSN) })

	open := func(t *testing.T, readOnly bool) driver.Conn {
		t.Helper()
		c, err := New().Open(ctxT(t), driver.ConnConfig{DSN: dsn + "&flavor=redshift", ReadOnly: readOnly})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	t.Run("Describe", func(t *testing.T) {
		c := open(t, false)
		b := c
		s, err := b.Describe(ctxT(t), driver.CollectionRef{Namespace: "tobira_rs", Name: "events"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(s.PrimaryKey, []string{"seq"}) || s.Comment != "an event log" || s.RowEstimate != 2 || s.SizeBytes < 0 {
			t.Errorf("schema = key %v comment %q rows %d size %d", s.PrimaryKey, s.Comment, s.RowEstimate, s.SizeBytes)
		}
		want := []driver.Index{
			{Name: "distkey", Fields: []string{"tenant"}, Method: "distkey"},
			{Name: "sortkey", Fields: []string{"tenant", "seq"}, Method: "compound sortkey"},
		}
		if fmt.Sprint(s.Indexes) != fmt.Sprint(want) {
			t.Errorf("indexes = %+v, want %+v (read from PG_TABLE_DEF outside search_path)", s.Indexes, want)
		}
		wantFK := []driver.ForeignKey{{Name: "events_account", Fields: []string{"tenant", "account"},
			References: driver.CollectionRef{Namespace: "tobira_rs", Name: "accounts"}, RefFields: []string{"tenant", "id"}}}
		if fmt.Sprint(s.ForeignKeys) != fmt.Sprint(wantFK) {
			t.Errorf("foreign keys = %+v, want %+v", s.ForeignKeys, wantFK)
		}
		acc, err := b.Describe(ctxT(t), driver.CollectionRef{Namespace: "tobira_rs", Name: "accounts"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(acc.PrimaryKey, []string{"tenant", "id"}) || acc.Indexes != nil {
			t.Errorf("accounts: key %v indexes %v", acc.PrimaryKey, acc.Indexes)
		}
		cols, err := b.Collections(ctxT(t), driver.NamespaceRef{Name: "tobira_rs"})
		if err != nil {
			t.Fatal(err)
		}
		if len(cols) != 2 || cols[1].Ref.Name != "events" || cols[1].RowEstimate != 2 || cols[1].Comment != "an event log" {
			t.Errorf("collections = %+v", cols)
		}
		nss, err := b.Namespaces(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, ns := range nss {
			names = append(names, ns.Name)
		}
		if !slices.Contains(names, "tobira_rs") || !slices.Contains(names, "public") {
			t.Errorf("namespaces = %v", names)
		}
	})

	t.Run("Gated", func(t *testing.T) {
		c := open(t, false)
		for name, ok := range map[string]bool{
			"Mutator":    is[driver.Mutator](c),
			"Transactor": is[driver.Transactor](c),
			"Exporter":   is[driver.Exporter](c),
			"Watcher":    is[driver.Watcher](c),
			"Vector":     is[driver.VectorSearcher](c),
		} {
			if ok {
				t.Errorf("%s offered on redshift", name)
			}
		}
		x, _ := driver.As[driver.Explainer](c)
		if _, err := x.Explain(ctxT(t), driver.Query{Text: "select 1"}, driver.ExplainOptions{Analyze: true}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("EXPLAIN ANALYZE err = %v, want ErrUnsupported", err)
		}
		plan, err := x.Explain(ctxT(t), driver.Query{Text: "select * from tobira_people where id > 3 order by name"}, driver.ExplainOptions{})
		if err != nil || plan.Root == nil || plan.Root.Label == "" {
			t.Errorf("plan = %+v, %v", plan, err)
		}
	})

	t.Run("ReadOnlyGuard", func(t *testing.T) {
		c := open(t, true)
		for _, q := range []string{
			"unload ('select * from tobira_people') to 's3://bucket/key' iam_role 'arn:aws:iam::0:role/x'",
			"copy tobira_people from 's3://bucket/key'",
			"call refresh_things()",
			"set transaction read write",
			"with d as (delete from tobira_people where id = 1 returning id) select * from d",
			"select * into tobira_copy from tobira_people",
			"explain (verbose, debug) select 1",
		} {
			rs, err := c.Query(ctxT(t), driver.Query{Text: q})
			if err == nil {
				_, _, err = driver.Collect(ctxT(t), rs, 0)
			}
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%q: err = %v, want ErrReadOnly", q, err)
			}
		}
		// No startup GUC: the read-only transaction and the allow-list hold.
		rows, _, _ := collect(t, c, driver.Query{Text: "show default_transaction_read_only"})
		if rows[0][0].String() != "off" {
			t.Errorf("redshift flavor sent default_transaction_read_only: %v", rows)
		}
	})
}

func is[T any](c driver.Conn) bool { _, ok := driver.As[T](c); return ok }

// greenplumEmulation stands in for Greenplum's gp_distribution_policy
// (Greenplum 6/7 layout) on PostgreSQL, with a hashed, a random and a
// replicated table.
var greenplumEmulation = []string{
	"SET allow_system_table_mods = on",
	`CREATE TABLE pg_catalog.gp_distribution_policy (localoid oid, policytype "char", numsegments integer, distkey int2vector, distclass oidvector)`,
	"RESET allow_system_table_mods",
	"CREATE TABLE gp_events (tenant text, id integer, note text, PRIMARY KEY (tenant, id))",
	"CREATE TABLE gp_random (n integer)",
	"CREATE TABLE gp_replicated (n integer)",
	`INSERT INTO pg_catalog.gp_distribution_policy VALUES
	   ('tobira_people'::regclass, 'p', 3, '1', ''),
	   ('gp_events'::regclass, 'p', 3, '2 1', ''),
	   ('gp_random'::regclass, 'p', 3, '', ''),
	   ('gp_replicated'::regclass, 'r', 3, '', '')`,
}

// TestGreenplumFlavor runs the greenplum flavor against PostgreSQL 17 with
// gp_distribution_policy emulated. No Greenplum server is involved.
func TestGreenplumFlavor(t *testing.T) {
	dsn := seededDB(t, "tobira_greenplum")
	execAll(t, dsn, greenplumEmulation)

	t.Run("Conformance", func(t *testing.T) { flavorConformance(t, flavorGreenplum, withScheme(dsn, "greenplum")) })

	t.Run("Distribution", func(t *testing.T) {
		u, _ := url.Parse(dsn)
		q := u.Query()
		q.Set("flavor", "gpdb")
		u.RawQuery = q.Encode()
		c, err := New().Open(ctxT(t), driver.ConnConfig{DSN: u.String()})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		b := c
		for name, want := range map[string][]string{
			"gp_events": {"id", "tenant"}, "tobira_people": {"id"}, "gp_random": nil, "gp_replicated": nil,
		} {
			s, err := b.Describe(ctxT(t), driver.CollectionRef{Namespace: "public", Name: name})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, ix := range s.Indexes {
				if ix.Method == "distribution" {
					got = ix.Fields
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s: distributed by %v, want %v", name, got, want)
			}
		}
		s, err := b.Describe(ctxT(t), driver.CollectionRef{Namespace: "public", Name: "gp_events"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(s.PrimaryKey, []string{"tenant", "id"}) {
			t.Errorf("primary key = %v", s.PrimaryKey)
		}
	})
}
