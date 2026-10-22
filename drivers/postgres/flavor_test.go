package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseFlavor(t *testing.T) {
	for in, want := range map[string]flavor{
		"postgres": flavorPostgres, "PostgreSQL": flavorPostgres,
		"redshift": flavorRedshift, " Redshift ": flavorRedshift,
		"cockroachdb": flavorCockroach, "cockroach": flavorCockroach, "crdb": flavorCockroach,
		"greenplum": flavorGreenplum, "gpdb": flavorGreenplum,
	} {
		got, err := parseFlavor(in)
		if err != nil || got != want {
			t.Errorf("parseFlavor(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "mysql", "yugabyte", "redshift2"} {
		if _, err := parseFlavor(in); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("parseFlavor(%q) err = %v, want ErrUnsupported", in, err)
		}
	}
}

func TestDetectFlavor(t *testing.T) {
	crdb := "v26.2.8"
	tests := []struct {
		version string
		crdb    *string
		want    flavor
	}{
		{"PostgreSQL 17.6 (Debian 17.6-1.pgdg120+1) on aarch64-unknown-linux-gnu", nil, flavorPostgres},
		{"PostgreSQL 8.0.2 on i686-pc-linux-gnu, compiled by GCC gcc (GCC) 3.4.2 20041017 (Red Hat 3.4.2-6.fc3), Redshift 1.0.77467", nil, flavorRedshift},
		{"PostgreSQL 9.4.26 (Greenplum Database 6.25.3 build commit:abc) on x86_64-unknown-linux-gnu", nil, flavorGreenplum},
		{"PostgreSQL 12.12 (Greenplum Database 7.1.0 build commit:abc)", nil, flavorGreenplum},
		{"CockroachDB CCL v26.2.8 (aarch64-unknown-linux-gnu, built 2026/10/15, go1.23.12)", nil, flavorCockroach},
		{"PostgreSQL 13.0", &crdb, flavorCockroach}, // crdb_version answers even if version() is unusual
		{"PostgreSQL 13.0", new(string), flavorPostgres},
	}
	for _, tt := range tests {
		if got := detectFlavor(tt.version, tt.crdb); got != tt.want {
			t.Errorf("detectFlavor(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
}

func TestSplitFlavor(t *testing.T) {
	tests := []struct {
		name, dsn string
		forced    flavor
		wantDSN   string
		want      flavor
	}{
		{"plain url", "postgres://u:p@h/db?sslmode=disable", "", "postgres://u:p@h/db?sslmode=disable", ""},
		{"param only", "postgres://u:p@h/db?flavor=redshift", "", "postgres://u:p@h/db", flavorRedshift},
		{"param among others", "postgresql://u@h/db?sslmode=disable&FLAVOR=crdb&application_name=x", "", "postgresql://u@h/db?sslmode=disable&application_name=x", flavorCockroach},
		{"param first", "postgres://h/db?flavor=greenplum&sslmode=disable", "", "postgres://h/db?sslmode=disable", flavorGreenplum},
		{"password with @", "postgres://u:a@b@h/db?flavor=redshift", "", "postgres://u:a@b@h/db", flavorRedshift},
		{"@ in the query", "postgres://u@h/db?application_name=a@b&flavor=redshift", "", "postgres://u@h/db?application_name=a@b", flavorRedshift},
		{"@ in the query after flavor", "postgres://h/db?flavor=redshift&application_name=a@b", "", "postgres://h/db?application_name=a@b", flavorRedshift},
		{"fragment", "postgres://h/db?flavor=greenplum#frag", "", "postgres://h/db#frag", flavorGreenplum},
		{"fragment with flavor", "postgres://h/db?sslmode=disable#flavor=redshift", "", "postgres://h/db?sslmode=disable#flavor=redshift", ""},
		{"escaped key", "postgres://h/db?%66lavor=crdb", "", "postgres://h/db", flavorCockroach},
		{"password looks like param", "postgres://u:flavor=greenplum@h/db", "", "postgres://u:flavor=greenplum@h/db", ""},
		{"redshift scheme", "redshift://u:p@cluster.example:5439/dev", "", "postgres://u:p@cluster.example:5439/dev", flavorRedshift},
		{"cockroachdb scheme", "cockroachdb://root@h:26257/defaultdb?sslmode=disable", "", "postgres://root@h:26257/defaultdb?sslmode=disable", flavorCockroach},
		{"cockroach scheme", "cockroach://root@h:26257/defaultdb", "", "postgres://root@h:26257/defaultdb", flavorCockroach},
		{"greenplum scheme", "greenplum://gpadmin@h/db", "", "postgres://gpadmin@h/db", flavorGreenplum},
		{"scheme agrees with param", "redshift://h/dev?flavor=redshift", "", "postgres://h/dev", flavorRedshift},
		{"forced by driver", "postgres://h/db", flavorCockroach, "postgres://h/db", flavorCockroach},
		{"forced agrees with scheme", "cockroachdb://h/db", flavorCockroach, "postgres://h/db", flavorCockroach},
		{"explicit postgres skips detection", "postgres://h/db?flavor=postgres", "", "postgres://h/db", flavorPostgres},
		{"keyword dsn", "host=h user=u flavor=redshift dbname=d", "", "host=h user=u dbname=d", flavorRedshift},
		{"keyword dsn spaced and quoted", "host=h flavor = 'greenplum' dbname=d", "", "host=h dbname=d", flavorGreenplum},
		{"keyword dsn first", "flavor=crdb host=h", "", "host=h", flavorCockroach},
		{"keyword dsn no flavor", "host=h password='flavor=redshift'", "", "host=h password='flavor=redshift'", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn, f, err := splitFlavor(tt.dsn, tt.forced)
			if err != nil {
				t.Fatal(err)
			}
			if dsn != tt.wantDSN || f != tt.want {
				t.Errorf("splitFlavor(%q) = %q, %q; want %q, %q", tt.dsn, dsn, f, tt.wantDSN, tt.want)
			}
		})
	}
	for _, tt := range []struct {
		dsn    string
		forced flavor
	}{
		{"postgres://h/db?flavor=oracle", ""},
		{"postgres://h/db?flavor=", ""},
		{"redshift://h/db?flavor=cockroachdb", ""},
		{"postgres://h/db?flavor=redshift&flavor=greenplum", ""},
		{"postgres://h/db?flavor=redshift", flavorCockroach},
		{"greenplum://h/db", flavorCockroach},
		{"host=h flavor=nope", ""},
	} {
		if _, _, err := splitFlavor(tt.dsn, tt.forced); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("splitFlavor(%q, %q) err = %v, want ErrUnsupported", tt.dsn, tt.forced, err)
		}
	}
}

func TestFlavorInfo(t *testing.T) {
	all := New().Info().Capabilities
	tests := []struct {
		f       flavor
		schemes []string
		without []driver.Capability
	}{
		{flavorRedshift, []string{"redshift"}, []driver.Capability{driver.CapMutator, driver.CapTransactor, driver.CapExporter, driver.CapWatcher, driver.CapVectorSearcher}},
		{flavorCockroach, []string{"cockroachdb", "cockroach"}, []driver.Capability{driver.CapWatcher, driver.CapVectorSearcher}},
		{flavorGreenplum, []string{"greenplum"}, nil},
	}
	for _, tt := range tests {
		info := newFlavored(tt.f).Info()
		if info.Name != string(tt.f) || info.Kind != driver.KindSQL || info.Dialect != "postgresql" {
			t.Errorf("%s: info = %+v", tt.f, info)
		}
		if !slices.Equal(info.Schemes, tt.schemes) {
			t.Errorf("%s: schemes = %v, want %v", tt.f, info.Schemes, tt.schemes)
		}
		for _, cp := range all {
			want := !slices.Contains(tt.without, cp)
			if got := slices.Contains(info.Capabilities, cp); got != want {
				t.Errorf("%s: claims %s = %v, want %v", tt.f, cp, got, want)
			}
		}
		if !slices.Contains(info.Capabilities, driver.CapReadOnly) {
			t.Errorf("%s: must keep the read-only guard", tt.f)
		}
		if _, ok := driver.Default.Lookup(string(tt.f)); !ok {
			t.Errorf("%s: not registered in driver.Default", tt.f)
		}
	}
}

func TestFlavorSupports(t *testing.T) {
	for _, tt := range []struct {
		f      flavor
		vector string
		cp     driver.Capability
		want   bool
	}{
		{flavorPostgres, "public", driver.CapWatcher, true},
		{flavorPostgres, "public", driver.CapVectorSearcher, true},
		{flavorPostgres, "", driver.CapVectorSearcher, false},
		{flavorGreenplum, "public", driver.CapVectorSearcher, true},
		{flavorCockroach, "", driver.CapWatcher, false},
		{flavorCockroach, "public", driver.CapVectorSearcher, false},
		{flavorCockroach, "", driver.CapMutator, true},
		{flavorCockroach, "", driver.CapExporter, true},
		{flavorRedshift, "", driver.CapWatcher, false},
		{flavorRedshift, "public", driver.CapVectorSearcher, false},
		{flavorRedshift, "", driver.CapMutator, false},
		{flavorRedshift, "", driver.CapTransactor, false},
		{flavorRedshift, "", driver.CapExporter, false},
		{flavorRedshift, "", driver.CapBrowser, true},
		{flavorRedshift, "", driver.CapExplainer, true},
		{flavorRedshift, "", driver.CapCompleter, true},
	} {
		c := &conn{flavor: tt.f, vectorSchema: tt.vector}
		if got := c.Supports(tt.cp); got != tt.want {
			t.Errorf("%s (vector %q): Supports(%s) = %v, want %v", tt.f, tt.vector, tt.cp, got, tt.want)
		}
	}
}

// TestGatedCallsRefuse checks that a gated capability refuses when called
// directly, not only through driver.As.
func TestGatedCallsRefuse(t *testing.T) {
	ctx := context.Background()
	for _, f := range []flavor{flavorRedshift, flavorCockroach} {
		c := &conn{flavor: f}
		if _, err := c.Watch(ctx, driver.WatchRequest{Channel: "x"}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s: Watch err = %v, want ErrUnsupported", f, err)
		}
	}
	c := &conn{flavor: flavorRedshift}
	if _, err := c.Begin(ctx, driver.TxOptions{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("redshift: Begin err = %v, want ErrUnsupported", err)
	}
	if _, err := c.Insert(ctx, driver.CollectionRef{Name: "t"}, nil); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("redshift: Insert err = %v, want ErrUnsupported", err)
	}
	if err := c.Export(ctx, driver.Query{Text: "select 1"}, "csv", nil); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("redshift: Export err = %v, want ErrUnsupported", err)
	}
}

func TestOpenFlavor(t *testing.T) {
	tests := []struct {
		name     string
		drv      flavor
		cfg      driver.ConnConfig
		want     flavor
		readOnly bool // default_transaction_read_only expected in the startup packet
	}{
		{"default needs detection", "", driver.ConnConfig{DSN: "postgres://u@h/db", ReadOnly: true}, "", true},
		{"param", "", driver.ConnConfig{DSN: "postgres://u@h/db?flavor=greenplum", ReadOnly: true}, flavorGreenplum, true},
		{"cockroach keeps the startup guard", flavorCockroach, driver.ConnConfig{DSN: "cockroachdb://u@h/db", ReadOnly: true}, flavorCockroach, true},
		{"redshift has no startup guard", flavorRedshift, driver.ConnConfig{DSN: "redshift://u@h/db", ReadOnly: true}, flavorRedshift, false},
		{"redshift via param", "", driver.ConnConfig{DSN: "postgres://u@h/db?flavor=redshift", ReadOnly: true}, flavorRedshift, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeConnect{}
			c, err := (&postgresDriver{connect: f.connect, flavor: tt.drv}).Open(context.Background(), tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			got := c.(*conn).flavor
			want := tt.want
			if want == "" {
				want = flavorPostgres // no detect func: the fake has no server to ask
			}
			if got != want {
				t.Errorf("flavor = %q, want %q", got, want)
			}
			rp := f.cfg.ConnConfig.RuntimeParams
			if _, ok := rp["flavor"]; ok {
				t.Error("flavor reached the server as a runtime parameter")
			}
			if has := rp[readOnlyParam] == "on"; has != tt.readOnly {
				t.Errorf("startup read-only param = %v, want %v", has, tt.readOnly)
			}
		})
	}
	f := &fakeConnect{}
	if _, err := openWith(t, f, driver.ConnConfig{DSN: "postgres://u@h/db?flavor=sybase"}); !errors.Is(err, driver.ErrUnsupported) || f.n != 0 {
		t.Errorf("unknown flavor: err = %v, connected %d times", err, f.n)
	}
}

func TestOpenDetectsFlavor(t *testing.T) {
	f := &fakeConnect{}
	d := &postgresDriver{connect: f.connect, detect: func(context.Context, *conn) (flavor, error) { return flavorCockroach, nil }}
	c, err := d.Open(context.Background(), driver.ConnConfig{DSN: "postgres://u@h/db"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if got := c.(*conn).flavor; got != flavorCockroach {
		t.Errorf("flavor = %q, want detected cockroachdb", got)
	}
	// A forced flavor never asks the server.
	d.detect = func(context.Context, *conn) (flavor, error) { t.Fatal("detect called"); return "", nil }
	c2, err := d.Open(context.Background(), driver.ConnConfig{DSN: "postgres://u@h/db?flavor=redshift"})
	if err != nil {
		t.Fatal(err)
	}
	_ = c2.Close()
	// A failed detection fails Open.
	d.detect = func(context.Context, *conn) (flavor, error) { return "", errors.New("boom") }
	if _, err := d.Open(context.Background(), driver.ConnConfig{DSN: "postgres://u@h/db"}); err == nil {
		t.Error("Open succeeded although detection failed")
	}
}

func TestResetStatements(t *testing.T) {
	for f, want := range map[flavor][]string{
		flavorPostgres:  {"DISCARD ALL"},
		flavorGreenplum: {"DISCARD ALL"},
		flavorCockroach: {"DISCARD ALL"},
		flavorRedshift:  {"RESET ALL"},
	} {
		if got := f.resetStatements(); !slices.Equal(got, want) {
			t.Errorf("%s: reset = %v, want %v", f, got, want)
		}
	}
}

func TestDeclareCursor(t *testing.T) {
	if got := flavorPostgres.declareCursor(); got != "DECLARE tobira_cur NO SCROLL CURSOR FOR " {
		t.Errorf("postgres: %q", got)
	}
	if got := flavorRedshift.declareCursor(); got != "DECLARE tobira_cur CURSOR FOR " {
		t.Errorf("redshift: %q", got)
	}
	if flavorRedshift.savepoints() || !flavorCockroach.savepoints() || !flavorPostgres.savepoints() {
		t.Error("only redshift lacks savepoints")
	}
}

func TestFlavorReadOnlyRefusal(t *testing.T) {
	allowed := []string{
		"select * from t",
		"  /* c */ SELECT 1",
		"with x as (select 1) select * from x",
		"values (1)",
		"table t",
		"(select 1)",
		"show tables",
		"explain select 1",
		"EXPLAIN (VERBOSE) SELECT 1",
		"explain analyze select 1",
		"explain analyse verbose select 1",
		"select * from crdb_internal.tables",
		"select 'crdb_internal.force_error(1)'",
		"select unload from t",
		"explain (opt, verbose) select 1",
		"EXPLAIN ANALYZE (VERBOSE) SELECT 1",
		"explain analyze (distsql, types) select 1",
		"explain (vec) select 1",
		"explain (gist, redact) select 1",
		"show tables",
		"show create table t",
		"show jobs",
		"select 'a' into_x from t",
		"select * from [show tables]",
		"with t as (select * from [show jobs]) select * from t",
	}
	refused := []string{
		"delete from t",
		"insert into t values (1)",
		"update t set a = 1",
		"set transaction read write",
		"SET default_transaction_read_only = off",
		"reset all",
		"begin",
		"commit",
		"call p()",
		"unload ('select 1') to 's3://b/k' iam_role 'arn'",
		"/* x */ UNLOAD ('select 1') TO 's3://b/k'",
		"export into csv 'nodelocal://1/x' from select 1",
		"backup into 'nodelocal://1/b'",
		"restore from latest in 'nodelocal://1/b'",
		"import into t csv data ('nodelocal://1/x')",
		"create changefeed for t into 'kafka://k'",
		"cancel query '1'",
		"pause job 1",
		"explain analyze delete from t",
		"explain (verbose) unload ('select 1') to 's3://b'",
		"explain",
		"explain (select 1)",
		"grant select on t to u",
		"vacuum",
		"analyze t",
		"select crdb_internal.force_error('x', 'y')",
		"select crdb_internal . unsafe_upsert_descriptor(1, '')",
		`select "crdb_internal"."force_retry"('1s')`,
		`select CRDB_INTERNAL.reset_sql_stats()`,
		`select U&"crdb\005finternal".force_error('a', 'b')`,
		`select U&"crdb!005finternal" UESCAPE '!' . "unsafe_delete_namespace_entry"(1, 2, 'x', 3)`,
		"select * from t where crdb_internal.request_statement_bundle('x', 0, '0s', '0s')",
		"",
		"   ",
		";",
		// EXPLAIN option blocks: only VERBOSE, TYPES, OPT, VEC, DISTSQL, GIST, REDACT.
		"explain analyze (debug) select 1",
		"EXPLAIN ANALYZE (DEBUG) SELECT 1",
		"explain analyze verbose (debug) select 1",
		"explain (debug) select 1",
		"explain (verbose, debug) select 1",
		"explain (opt, env) select 1",
		"explain (analyze, debug) select 1",
		"explain (verbose) (debug) select 1",
		"explain ((select 1))",
		"explain (verbose",
		"explain analyze (verbose) delete from t",
		// SELECT INTO creates a table.
		"select * into newtable from t",
		"with x as (select 1) select * into newtable from x",
		// SHOW forms that read external storage.
		"show backup 's3://bucket/b'",
		"show backups in 'nodelocal://1/b'",
		"SHOW BACKUP FROM LATEST IN 'gs://b/x'",
		"show backup schemas from latest in 'nodelocal://1/b'",
		"explain show backup 's3://b'",
		"show tables from 's3://x'",
		// Bracketed SHOW sources, at any depth.
		"SELECT * FROM [SHOW BACKUPS IN 'nodelocal://1/b']",
		"select * from [show backup from latest in 'nodelocal://1/b']",
		"with b as (select * from [show backups in 'nodelocal://1/b']) select * from b",
		"select * from (select * from (select * from [ show  /* x */ backups in 'gs://b' ]) s) t",
		"select * from [show tables from 's3://x']",
		"explain select * from [show backups in 'nodelocal://1/b']",
		"select * from [show backups in $1]",
	}
	for _, f := range []flavor{flavorRedshift, flavorCockroach} {
		for _, q := range allowed {
			if err := flavorRefusal(f, "query", q); err != nil {
				t.Errorf("%s: %q refused: %v", f, q, err)
			}
		}
		for _, q := range refused {
			err := flavorRefusal(f, "query", q)
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%s: %q err = %v, want ErrReadOnly", f, q, err)
			}
		}
	}
	for _, f := range []flavor{flavorPostgres, flavorGreenplum} {
		for _, q := range refused {
			if err := flavorRefusal(f, "query", q); err != nil {
				t.Errorf("%s: %q refused by the flavor guard: %v (the postgres guard applies)", f, q, err)
			}
		}
	}
}

func TestExplainPrefix(t *testing.T) {
	tests := []struct {
		f       flavor
		analyze bool
		want    string
	}{
		{flavorCockroach, false, "EXPLAIN "},
		{flavorCockroach, true, "EXPLAIN ANALYZE "},
		{flavorRedshift, false, "EXPLAIN "},
	}
	for _, tt := range tests {
		got, err := tt.f.textExplainPrefix(tt.analyze)
		if err != nil || got != tt.want {
			t.Errorf("%s analyze=%v: %q, %v; want %q", tt.f, tt.analyze, got, err, tt.want)
		}
	}
	if _, err := flavorRedshift.textExplainPrefix(true); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("redshift EXPLAIN ANALYZE err = %v, want ErrUnsupported", err)
	}
	if flavorPostgres.textExplain() || flavorGreenplum.textExplain() || !flavorCockroach.textExplain() || !flavorRedshift.textExplain() {
		t.Error("only cockroachdb and redshift use the text explain")
	}
}

func TestParseTextPlanRedshift(t *testing.T) {
	lines := []string{
		"XN Hash Join DS_DIST_NONE  (cost=112.50..14142.59 rows=170771 width=84)",
		"  Hash Cond: (\"outer\".venueid = \"inner\".venueid)",
		"  ->  XN Seq Scan on event  (cost=0.00..87.98 rows=8798 width=48)",
		"  ->  XN Hash  (cost=2.02..2.02 rows=202 width=36)",
		"        ->  XN Seq Scan on venue  (cost=0.00..2.02 rows=202 width=36)",
	}
	root := parseTextPlan(lines)
	if root == nil || root.Label != "XN Hash Join DS_DIST_NONE" || root.Cost != 14142.59 || root.Rows != 170771 {
		t.Fatalf("root = %+v", root)
	}
	if len(root.Children) != 2 || root.Children[0].Label != "XN Seq Scan on event" || root.Children[0].Rows != 8798 {
		t.Fatalf("children = %+v", root.Children)
	}
	hash := root.Children[1]
	if hash.Label != "XN Hash" || len(hash.Children) != 1 || hash.Children[0].Label != "XN Seq Scan on venue" {
		t.Fatalf("hash = %+v", hash)
	}
	if !strings.Contains(root.Detail, "Hash Cond") {
		t.Errorf("detail = %q, want the condition", root.Detail)
	}
}

func TestParseTextPlanCockroach(t *testing.T) {
	lines := []string{
		"distribution: local",
		"vectorized: true",
		"",
		"• sort",
		"│ estimated row count: 7",
		"│ order: +id",
		"│",
		"└── • hash join",
		"    │ estimated row count: 7",
		"    │ equality: (id) = (person_id)",
		"    │",
		"    ├── • scan",
		"    │     estimated row count: 7 (100% of the table; stats collected 1 minute ago)",
		"    │     table: tobira_people@tobira_people_pkey",
		"    │     spans: FULL SCAN",
		"    │",
		"    └── • scan",
		"          missing stats",
		"          table: tobira_orders@tobira_orders_pkey",
		"          spans: FULL SCAN",
	}
	root := parseTextPlan(lines)
	if root == nil || root.Label != "sort" || root.Rows != 7 {
		t.Fatalf("root = %+v", root)
	}
	if len(root.Children) != 1 || root.Children[0].Label != "hash join" {
		t.Fatalf("children = %+v", root.Children)
	}
	join := root.Children[0]
	if len(join.Children) != 2 || join.Children[0].Label != "scan" || join.Children[1].Label != "scan" {
		t.Fatalf("join children = %+v", join.Children)
	}
	if join.Children[0].Detail != "tobira_people@tobira_people_pkey" || join.Children[0].Rows != 7 {
		t.Errorf("scan = %+v", join.Children[0])
	}
	if join.Children[1].Rows != -1 || join.Children[1].Detail != "tobira_orders@tobira_orders_pkey" {
		t.Errorf("second scan = %+v", join.Children[1])
	}
	if parseTextPlan(nil) != nil || parseTextPlan([]string{"distribution: local"}) != nil {
		t.Error("a plan without nodes has no root")
	}
}
