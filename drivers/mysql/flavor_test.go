package mysql

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/xavidop/tobira/driver"
)

func TestParseFlavor(t *testing.T) {
	for in, want := range map[string]flavor{
		"mysql": flavorMySQL, "MySQL": flavorMySQL, " mariadb ": flavorMariaDB, "MARIADB": flavorMariaDB,
		"singlestore": flavorSingleStore, "SingleStore": flavorSingleStore,
	} {
		if got, err := parseFlavor(in); err != nil || got != want {
			t.Errorf("parseFlavor(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "percona", "memsql", "mysql;", "maria db"} {
		if _, err := parseFlavor(in); err == nil {
			t.Errorf("parseFlavor(%q) accepted", in)
		}
	}
	for f, name := range map[flavor]string{flavorMySQL: "mysql", flavorMariaDB: "mariadb", flavorSingleStore: "singlestore"} {
		if f.String() != name {
			t.Errorf("%d.String() = %q, want %q", f, f.String(), name)
		}
	}
}

func TestFlavorOf(t *testing.T) {
	tests := []struct {
		version, comment string
		mariaVar, memsql bool
		want             flavor
	}{
		{"8.4.3", "MySQL Community Server - GPL", false, false, flavorMySQL},
		{"8.0.36-28", "Percona Server (GPL), Release 28", false, false, flavorMySQL},
		{"11.8.9-MariaDB-ubu2404", "mariadb.org binary distribution", true, false, flavorMariaDB},
		{"5.5.5-10.11.6-MariaDB", "", false, false, flavorMariaDB},
		{"10.6.18-mariadb-log", "", false, false, flavorMariaDB},
		// A MariaDB whose version was overridden to look like MySQL.
		{"8.0.35", "mariadb.org binary distribution", false, false, flavorMariaDB},
		{"8.0.35", "Source distribution", true, false, flavorMariaDB},
		{"5.7.32", "", false, true, flavorSingleStore},
		{"8.0.32", "MemSQL source distribution", false, true, flavorSingleStore},
		{"11.4.2-MariaDB", "", false, true, flavorMariaDB}, // the MariaDB signals decide first
	}
	for _, tt := range tests {
		if got := flavorOf(tt.version, tt.comment, tt.mariaVar, tt.memsql); got != tt.want {
			t.Errorf("flavorOf(%q, %q, %v, %v) = %v, want %v", tt.version, tt.comment, tt.mariaVar, tt.memsql, got, tt.want)
		}
	}
}

// fakeProbe answers the detection probes: exists maps a variable to whether
// the server has it; fail maps it to an error other than "unknown variable".
func fakeProbe(exists map[string]bool, fail map[string]error) func(string) (bool, error) {
	return func(v string) (bool, error) {
		if err, ok := fail[v]; ok {
			return false, err
		}
		return exists[v], nil
	}
}

func TestClassifyFlavor(t *testing.T) {
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	proxyErr := errors.New("proxy: unsupported variable")
	tests := []struct {
		name     string
		version  string
		exists   map[string]bool
		fail     map[string]error
		readOnly bool
		want     flavor
		wantErr  bool
	}{
		{"mysql", "8.4.3", nil, nil, true, flavorMySQL, false},
		{"mariadb by variable", "8.0.35", map[string]bool{mariaDBProbeVar: true}, nil, true, flavorMariaDB, false},
		{"singlestore", "5.7.32", map[string]bool{memsqlProbeVar: true}, nil, true, flavorSingleStore, false},
		{"memsql probe fails, writable", "8.4.3", nil, map[string]error{memsqlProbeVar: proxyErr}, false, flavorMySQL, false},
		{"memsql probe fails, read-only", "8.4.3", nil, map[string]error{memsqlProbeVar: proxyErr}, true, flavorMySQL, true},
		{"mariadb probe fails, writable", "8.4.3", nil, map[string]error{mariaDBProbeVar: proxyErr}, false, flavorMySQL, false},
		{"mariadb probe fails, read-only", "8.4.3", nil, map[string]error{mariaDBProbeVar: proxyErr}, true, flavorMySQL, true},
		// memsql_version is decisive: it is probed first, so a SingleStore
		// that also had in_transaction would still be SingleStore.
		{"both variables", "5.7.32", map[string]bool{mariaDBProbeVar: true, memsqlProbeVar: true}, nil, true, flavorSingleStore, false},
		{"singlestore, mariadb probe would fail", "5.7.32", map[string]bool{memsqlProbeVar: true}, map[string]error{mariaDBProbeVar: proxyErr}, true, flavorSingleStore, false},
		{"mariadb version skips the probes", "11.8.9-MariaDB", nil, map[string]error{mariaDBProbeVar: proxyErr, memsqlProbeVar: proxyErr}, true, flavorMariaDB, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()
			got, err := classifyFlavor(context.Background(), tt.version, "", fakeProbe(tt.exists, tt.fail), tt.readOnly, log)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("classifyFlavor = %v, %v; want %v (error %v)", got, err, tt.want, tt.wantErr)
			}
			if err != nil && !errors.Is(err, proxyErr) {
				t.Errorf("error %v does not wrap the probe's error", err)
			}
			if tt.fail != nil && !tt.wantErr && tt.want == flavorMySQL && !strings.Contains(logs.String(), "level=DEBUG") {
				t.Errorf("a writable fallback must leave a debug log, got %q", logs.String())
			}
		})
	}
}

func TestReadOnlyVarRefusal(t *testing.T) {
	err := readOnlyVarRefusal(&gomysql.MySQLError{Number: 1193, Message: "Unknown system variable 'transaction_read_only'"})
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), "MariaDB 11.1") {
		t.Errorf("unknown transaction_read_only = %v, want ErrUnsupported naming the versions", err)
	}
	for _, other := range []error{
		&gomysql.MySQLError{Number: 1193, Message: "Unknown system variable 'foo'"},
		&gomysql.MySQLError{Number: 1045, Message: "Access denied"},
		errors.New("dial tcp: refused"),
		nil,
	} {
		if err := readOnlyVarRefusal(other); err != nil {
			t.Errorf("readOnlyVarRefusal(%v) = %v, want nil", other, err)
		}
	}
}

func TestTxStartStatements(t *testing.T) {
	tests := []struct {
		server   flavor
		readOnly bool
		lvl      string
		want     string
		wantErr  bool
	}{
		{flavorMySQL, false, "", "START TRANSACTION READ WRITE", false},
		{flavorMySQL, true, "", "SET SESSION TRANSACTION READ ONLY|START TRANSACTION READ ONLY", false},
		{flavorMySQL, false, "SERIALIZABLE", "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE|START TRANSACTION READ WRITE", false},
		{flavorMariaDB, true, "READ COMMITTED", "SET TRANSACTION ISOLATION LEVEL READ COMMITTED|SET SESSION TRANSACTION READ ONLY|START TRANSACTION READ ONLY", false},
		{flavorSingleStore, false, "", "START TRANSACTION", false},
		{flavorSingleStore, true, "", "", true},
		{flavorSingleStore, false, "SERIALIZABLE", "", true},
	}
	for _, tt := range tests {
		got, err := txStartStatements(tt.server, tt.readOnly, tt.lvl)
		if (err != nil) != tt.wantErr || strings.Join(got, "|") != tt.want {
			t.Errorf("txStartStatements(%v, %v, %q) = %q, %v; want %q (error %v)", tt.server, tt.readOnly, tt.lvl, got, err, tt.want, tt.wantErr)
		}
		if err != nil && !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("refusal %v is not ErrUnsupported", err)
		}
	}
}

// The server's flavor, not a forced one, picks syntax the server must parse.
func TestServerFlavor(t *testing.T) {
	for _, tc := range []struct {
		c    *conn
		want flavor
	}{
		{&conn{detected: flavorSingleStore, flavor: flavorMySQL}, flavorSingleStore},
		{&conn{detected: flavorMySQL, flavor: flavorSingleStore}, flavorSingleStore},
		{&conn{detected: flavorMariaDB, flavor: flavorMySQL}, flavorMariaDB},
		{&conn{detected: flavorMySQL, flavor: flavorMariaDB}, flavorMariaDB},
		{&conn{}, flavorMySQL},
	} {
		if got := tc.c.syntaxFlavor(); got != tc.want {
			t.Errorf("detected %v, forced %v: syntaxFlavor = %v, want %v", tc.c.detected, tc.c.flavor, got, tc.want)
		}
	}
}

func TestFlavoredDrivers(t *testing.T) {
	full := []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor, driver.CapCompleter, driver.CapReadOnly}
	for _, tc := range []struct {
		name string
		caps []driver.Capability
	}{
		{"mariadb", full},
		{"singlestore", []driver.Capability{driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapCompleter}},
	} {
		d, ok := driver.Default.Lookup(tc.name)
		if !ok {
			t.Fatalf("%q is not registered", tc.name)
		}
		info := d.Info()
		if info.Name != tc.name || info.Kind != driver.KindSQL || info.Dialect != "mysql" || !slices.Equal(info.Schemes, []string{tc.name}) {
			t.Errorf("%s info = %+v", tc.name, info)
		}
		if !slices.Equal(info.Capabilities, tc.caps) {
			t.Errorf("%s capabilities = %v, want %v", tc.name, info.Capabilities, tc.caps)
		}
		if bySch, ok := driver.Default.ForScheme(tc.name); !ok || bySch.Info().Name != tc.name {
			t.Errorf("scheme %s:// does not resolve to the %s driver", tc.name, tc.name)
		}
	}
	if d, ok := driver.Default.ForScheme("mysql"); !ok || d.Info().Name != "mysql" {
		t.Error("mysql:// does not resolve to the mysql driver")
	}
}

func TestFlavoredDriverForcesFlavor(t *testing.T) {
	tests := []struct {
		d    mysqlDriver
		dsn  string
		want flavor
		ok   bool
	}{
		{newFlavored(flavorMariaDB), "u@tcp(h)/db", flavorMariaDB, true},
		{newFlavored(flavorMariaDB), "mysql://u@h/db", flavorMariaDB, true},
		{newFlavored(flavorMariaDB), "mariadb://u@h/db?flavor=mariadb", flavorMariaDB, true},
		{newFlavored(flavorSingleStore), "singlestore://u@h/db", flavorSingleStore, true},
		{newFlavored(flavorSingleStore), "u@tcp(h)/db", flavorSingleStore, true},
		{newFlavored(flavorMariaDB), "u@tcp(h)/db?flavor=singlestore", 0, false},
		{newFlavored(flavorSingleStore), "mariadb://u@h/db", 0, false},
		{mysqlDriver{}, "u@tcp(h)/db", flavorMySQL, true},
	}
	for _, tt := range tests {
		_, f, forced, err := tt.d.config(driver.ConnConfig{DSN: tt.dsn})
		if tt.ok != (err == nil) {
			t.Errorf("%s driver, %q: err = %v, want ok=%v", tt.d.Info().Name, tt.dsn, err, tt.ok)
			continue
		}
		if !tt.ok {
			if !errors.Is(err, driver.ErrUnsupported) {
				t.Errorf("%s driver, %q: want ErrUnsupported, got %v", tt.d.Info().Name, tt.dsn, err)
			}
			continue
		}
		wantForced := tt.d.flavored
		if f != tt.want || forced != wantForced {
			t.Errorf("%s driver, %q: flavor %v (forced %v), want %v (forced %v)", tt.d.Info().Name, tt.dsn, f, forced, tt.want, wantForced)
		}
	}
}

func TestMariaDBVerified(t *testing.T) {
	tests := []struct {
		version string
		ok      bool
	}{
		{"11.8.9-MariaDB-ubu2404", true},
		{"11.4.2-MariaDB", true},
		{"11.1.0-MariaDB", true},
		{"5.5.5-11.8.9-MariaDB", true},
		{"11.0.6-MariaDB", false}, // no transaction_read_only before 11.1
		{"10.11.6-MariaDB-1:10.11.6+maria~ubu2204", false},
		{"12.0.2-MariaDB", false},
		{"11.8.9", false}, // not a MariaDB version string
		{"8.4.3", false},
		{"", false},
		{"garbage-MariaDB", false},
	}
	for _, tt := range tests {
		if got := mariaDBVerified(tt.version); got != tt.ok {
			t.Errorf("mariaDBVerified(%q) = %v, want %v", tt.version, got, tt.ok)
		}
	}
}

func TestReadOnlyRefusal(t *testing.T) {
	tests := []struct {
		name              string
		version           string
		detected, flavour flavor
		ok                bool
		mention           string
	}{
		{"mysql 8.4", "8.4.3", flavorMySQL, flavorMySQL, true, ""},
		{"mysql 9", "9.1.0", flavorMySQL, flavorMySQL, false, "8.0 to 8.4"},
		{"mariadb 11.8", "11.8.9-MariaDB", flavorMariaDB, flavorMariaDB, true, ""},
		{"mariadb 10.11", "10.11.6-MariaDB", flavorMariaDB, flavorMariaDB, false, "MariaDB 11"},
		{"singlestore", "5.7.32", flavorSingleStore, flavorSingleStore, false, "SingleStore"},
		{"singlestore reporting 8.0", "8.0.32", flavorSingleStore, flavorSingleStore, false, "SingleStore"},
		{"mysql forced on singlestore", "8.0.32", flavorSingleStore, flavorMySQL, false, "flavor=mysql"},
		{"mariadb forced on mysql", "8.4.3", flavorMySQL, flavorMariaDB, false, "flavor=mariadb"},
		{"mysql forced on mariadb", "11.8.9-MariaDB", flavorMariaDB, flavorMySQL, false, "flavor=mysql"},
		{"singlestore forced on mysql", "8.4.3", flavorMySQL, flavorSingleStore, false, "flavor=singlestore"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := readOnlyRefusal(tt.version, tt.detected, tt.flavour)
			if (err == nil) != tt.ok {
				t.Fatalf("readOnlyRefusal = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("refusal %q does not mention %q", err, tt.mention)
			}
			cerr := checkServer(tt.version, tt.detected, tt.flavour, true)
			if tt.ok != (cerr == nil) || cerr != nil && !errors.Is(cerr, driver.ErrUnsupported) {
				t.Errorf("checkServer(read-only) = %v", cerr)
			}
			if err := checkServer(tt.version, tt.detected, tt.flavour, false); err != nil {
				t.Errorf("checkServer(writable) = %v, want nil", err)
			}
		})
	}
}

// A conn whose flavor gates read-only refuses every read-only entry point
// before it touches the (absent) pool.
func TestReadOnlyRefusedByFlavor(t *testing.T) {
	ctx := context.Background()
	for _, c := range []*conn{
		{version: "5.7.32", detected: flavorSingleStore, flavor: flavorSingleStore},
		{version: "8.4.3", detected: flavorMySQL, flavor: flavorMariaDB},
		{version: "10.11.6-MariaDB", detected: flavorMariaDB, flavor: flavorMariaDB},
	} {
		if _, err := c.Query(ctx, driver.Query{Text: "SELECT 1", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%v on %q: read-only Query = %v, want ErrUnsupported", c.flavor, c.version, err)
		}
		if _, err := c.Begin(ctx, driver.TxOptions{ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%v on %q: read-only Begin = %v, want ErrUnsupported", c.flavor, c.version, err)
		}
	}
}

func TestSingleStoreRefusesTransactions(t *testing.T) {
	c := &conn{version: "5.7.32", detected: flavorSingleStore, flavor: flavorSingleStore}
	_, err := c.Begin(context.Background(), driver.TxOptions{})
	if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), "SingleStore") {
		t.Errorf("Begin on SingleStore = %v, want ErrUnsupported naming SingleStore", err)
	}
	// Errors carry the name of the driver that opened the conn.
	for name, want := range map[string]string{"singlestore": "singlestore", "mysql": "mysql", "": "mysql"} {
		c := &conn{version: "5.7.32", detected: flavorSingleStore, flavor: flavorSingleStore, name: name}
		_, err := c.Begin(context.Background(), driver.TxOptions{})
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != want {
			t.Errorf("opened by %q: error driver = %v, want %q", name, err, want)
		}
	}
}

func TestBuildConfigFlavor(t *testing.T) {
	tests := []struct {
		dsn    string
		want   flavor
		forced bool
	}{
		{"mysql://u@h/db", flavorMySQL, false},
		{"mysql://u@h/db?flavor=mariadb", flavorMariaDB, true},
		{"mysql://u@h/db?FLAVOR=SingleStore", flavorSingleStore, true},
		{"mysql://u@h/db?flavor=mysql", flavorMySQL, true},
		{"u@tcp(h)/db?flavor=mariadb", flavorMariaDB, true},
		{"mariadb://u@h/db", flavorMariaDB, true},
		{"MariaDB://u:p@h:3307/db", flavorMariaDB, true},
		{"singlestore://u@h/db", flavorSingleStore, true},
		{"mariadb://u@h/db?flavor=MariaDB", flavorMariaDB, true},
	}
	for _, tt := range tests {
		t.Run(tt.dsn, func(t *testing.T) {
			mc, f, forced, err := parseConfig(driver.ConnConfig{DSN: tt.dsn})
			if err != nil {
				t.Fatal(err)
			}
			if f != tt.want || forced != tt.forced {
				t.Errorf("flavor = %v (forced %v), want %v (forced %v)", f, forced, tt.want, tt.forced)
			}
			if got := paramValues(mc, "flavor"); len(got) != 0 {
				t.Errorf("flavor reached the session variables: %q", got)
			}
		})
	}
	mc := mustConfig(t, driver.ConnConfig{DSN: "mariadb://u:pw@h:3307/app"})
	if mc.User != "u" || mc.Passwd != "pw" || mc.Addr != "h:3307" || mc.DBName != "app" {
		t.Errorf("mariadb:// parsed as %+v", mc)
	}
	if mc = mustConfig(t, driver.ConnConfig{DSN: "singlestore://u@h/db"}); mc.Addr != "h:3306" {
		t.Errorf("singlestore:// addr = %q", mc.Addr)
	}
}

func TestBuildConfigFlavorRefused(t *testing.T) {
	for _, dsn := range []string{
		"mysql://u@h/db?flavor=percona",
		"mysql://u@h/db?flavor=",
		"mysql://u@h/db?flavor=mariadb&FLAVOR=singlestore",
		"mariadb://u@h/db?flavor=singlestore",
		"singlestore://u@h/db?flavor=mysql",
		"u:topsecret@tcp(h)/db?flavor=oracle",
	} {
		_, err := buildConfig(driver.ConnConfig{DSN: dsn})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("buildConfig(%q) = %v, want ErrUnsupported", dsn, err)
		}
		if err != nil && strings.Contains(err.Error(), "topsecret") {
			t.Errorf("error leaks the password: %v", err)
		}
	}
}

func TestMariaDBModeRefusal(t *testing.T) {
	for _, mode := range []string{
		"",
		"STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_AUTO_CREATE_USER,NO_ENGINE_SUBSTITUTION",
		"REAL_AS_FLOAT,PIPES_AS_CONCAT,ANSI_QUOTES,IGNORE_SPACE,ANSI",
		"NO_BACKSLASH_ESCAPES",
		"TRADITIONAL,ONLY_FULL_GROUP_BY,NO_ZERO_DATE,NO_ZERO_IN_DATE,STRICT_ALL_TABLES",
		"no_backslash_escapes, simultaneous_assignment",
	} {
		if err := mariaDBModeRefusal(mode); err != nil {
			t.Errorf("mariaDBModeRefusal(%q) = %v, want nil", mode, err)
		}
	}
	for _, mode := range []string{
		"PIPES_AS_CONCAT,ANSI_QUOTES,IGNORE_SPACE,MSSQL,NO_KEY_OPTIONS,NO_TABLE_OPTIONS,NO_FIELD_OPTIONS",
		"PIPES_AS_CONCAT,ANSI_QUOTES,IGNORE_SPACE,ORACLE,NO_KEY_OPTIONS,NO_TABLE_OPTIONS,NO_FIELD_OPTIONS,NO_AUTO_CREATE_USER,SIMULTANEOUS_ASSIGNMENT",
		"POSTGRESQL",
		"DB2",
		"MAXDB",
		"SOME_FUTURE_MODE",
	} {
		err := mariaDBModeRefusal(mode)
		if err == nil {
			t.Errorf("mariaDBModeRefusal(%q) accepted", mode)
		}
	}
}

func TestNamespacesSQLPerFlavor(t *testing.T) {
	for f, hidden := range map[flavor][]string{
		flavorMySQL:       {"'information_schema'", "'mysql'", "'performance_schema'", "'sys'"},
		flavorMariaDB:     {"'information_schema'", "'mysql'", "'performance_schema'", "'sys'"},
		flavorSingleStore: {"'information_schema'", "'memsql'", "'cluster'"},
	} {
		q := namespacesSQL(f)
		for _, h := range hidden {
			if !strings.Contains(q, h) {
				t.Errorf("%v: namespaces query does not hide %s:\n%s", f, h, q)
			}
		}
	}
	if strings.Contains(namespacesSQL(flavorMySQL), "memsql") {
		t.Error("mysql hides a database named memsql")
	}
}

func TestIndexesSQLPerFlavor(t *testing.T) {
	if !strings.Contains(indexesSQL(flavorMySQL), "EXPRESSION") {
		t.Error("mysql indexes query lost functional key parts")
	}
	if strings.Contains(indexesSQL(flavorMariaDB), "EXPRESSION") {
		t.Error("MariaDB's STATISTICS has no EXPRESSION column")
	}
	s2 := indexesSQL(flavorSingleStore)
	if strings.Contains(s2, "EXPRESSION") || !strings.Contains(s2, "'SHARD'") || !strings.Contains(s2, "GROUP BY") {
		t.Errorf("singlestore indexes query must drop SHARD rows and fold duplicates:\n%s", s2)
	}
}

func TestDefaultExprPerFlavor(t *testing.T) {
	tests := []struct {
		f     flavor
		def   string
		typ   driver.Type
		extra string
		want  string
	}{
		{flavorMySQL, "n/a", driver.TypeString, "", "'n/a'"},
		{flavorMySQL, "0.00", driver.TypeDecimal, "", "0.00"},
		{flavorMySQL, "CURRENT_TIMESTAMP", driver.TypeTime, "DEFAULT_GENERATED", "CURRENT_TIMESTAMP"},
		{flavorMariaDB, "'n/a'", driver.TypeString, "", "'n/a'"},
		{flavorMariaDB, "0.00", driver.TypeDecimal, "", "0.00"},
		{flavorMariaDB, "current_timestamp()", driver.TypeTime, "", "current_timestamp()"},
		{flavorMariaDB, "NULL", driver.TypeInt, "", ""},
		{flavorMariaDB, "'NULL'", driver.TypeString, "", "'NULL'"},
		{flavorSingleStore, "x", driver.TypeString, "", "'x'"},
		{flavorSingleStore, "CURRENT_TIMESTAMP", driver.TypeTime, "", "CURRENT_TIMESTAMP"},
		{flavorSingleStore, "now()", driver.TypeTime, "", "now()"},
		{flavorSingleStore, "2020-01-01 00:00:00", driver.TypeTime, "", "'2020-01-01 00:00:00'"},
	}
	for _, tt := range tests {
		if got := defaultExpr(tt.f, tt.def, tt.typ, tt.extra); got != tt.want {
			t.Errorf("%v: defaultExpr(%q, %v, %q) = %q, want %q", tt.f, tt.def, tt.typ, tt.extra, got, tt.want)
		}
	}
}

func TestFieldTypeMariaDBTypes(t *testing.T) {
	for _, dt := range []string{"uuid", "inet4", "inet6"} {
		if got := fieldType(dt, dt); got != driver.TypeString {
			t.Errorf("fieldType(%q) = %v, want String", dt, got)
		}
	}
}

func TestExplainTextPerFlavor(t *testing.T) {
	for _, tc := range []struct{ format, want string }{
		{fmtJSON, "EXPLAIN FORMAT=JSON SELECT 1"},
		{fmtTree, "EXPLAIN FORMAT=TREE SELECT 1"},
		{fmtTraditional, "EXPLAIN FORMAT=TRADITIONAL SELECT 1"},
		{fmtBareJSON, "EXPLAIN JSON SELECT 1"},
		{fmtBare, "EXPLAIN SELECT 1"},
	} {
		got := explainText(tc.format, "SELECT 1")
		if got != tc.want {
			t.Errorf("explainText(%q) = %q, want %q", tc.format, got, tc.want)
		}
		if err := explainTarget(got, lexMode{}); err != nil {
			t.Errorf("explainTarget(%q) = %v", got, err)
		}
	}
	for _, text := range []string{
		"EXPLAIN JSON",
		"EXPLAIN",
		"EXPLAIN ANALYZE SELECT 1",
		"EXPLAIN JSON ANALYZE SELECT 1",
		"EXPLAIN EXTENDED SELECT 1",
		"EXPLAIN FORMAT=XML SELECT 1",
		"EXPLAIN JSON t",
		"EXPLAIN t",
		"EXPLAIN JSON SELECT 1; DELETE FROM t",
		"EXPLAIN JSON SELECT 1 INTO OUTFILE '/tmp/x'",
		"EXPLAIN /*! ANALYZE */ SELECT 1",
		"PROFILE SELECT 1",
	} {
		if err := explainTarget(text, lexMode{}); err == nil {
			t.Errorf("explainTarget(%q) accepted", text)
		}
	}
}

func TestExplainFormatsPerFlavor(t *testing.T) {
	for f, want := range map[flavor][]string{
		flavorMySQL:       {fmtJSON, fmtTree, fmtTraditional},
		flavorMariaDB:     {fmtJSON, fmtTraditional},
		flavorSingleStore: {fmtBareJSON, fmtBare},
	} {
		var got []string
		_, err := explainPlan(f, func(format string) ([]driver.Column, []driver.Row, error) {
			got = append(got, format)
			switch format {
			case fmtJSON:
				return nil, []driver.Row{{driver.String(`{"query_block": {"select_id": 1}}`)}}, nil
			case fmtBareJSON:
				return nil, []driver.Row{{driver.String(`{"explain": [{"executor": "Gather", "inputs": []}]}`)}}, nil
			case fmtTree:
				return nil, []driver.Row{{driver.String("<not executable by iterator executor>")}}, nil
			}
			return []driver.Column{{Name: "EXPLAIN"}}, []driver.Row{{driver.String("plan")}}, nil
		})
		if err != nil {
			t.Fatalf("%v: %v", f, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%v ran formats %q, want %q", f, got, want)
		}
	}
}

func TestParseMariaDBPlan(t *testing.T) {
	raw := `{"query_block": {"select_id": 1, "cost": 0.0109, "nested_loop": [
	  {"table": {"table_name": "j", "access_type": "ALL", "loops": 1, "rows": 7, "cost": 0.0109, "filtered": 100}}]}}`
	root, err := parsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if root.Label != "query_block" || root.Cost != 0.0109 || len(root.Children) != 1 {
		t.Fatalf("root = %+v", root)
	}
	if tb := root.Children[0]; tb.Label != "table" || tb.Detail != "j ALL" || tb.Rows != 7 || tb.Cost != 0.0109 {
		t.Errorf("table = %+v", tb)
	}
}

func TestParseSingleStorePlan(t *testing.T) {
	raw := `{"plan_warnings": {}, "explain": [{"executor": "Gather", "partitions": "single", "alias": "remote_0",
	  "inputs": [{"executor": "Project", "out": [{"alias": "", "projection": "t.id"}], "subselects": [],
	    "inputs": [{"executor": "ColumnStoreFilter", "condition": ["t.id = 1 index"], "est_rows": "1",
	      "inputs": [{"executor": "ColumnStoreScan", "db": "tobira", "table": "t", "index": "PRIMARY KEY (id)", "inputs": []}]}]}]}],
	  "version": "4", "info": {"memsql_version": "9.1.1"}}`
	root, err := parseSingleStorePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if root.Label != "Gather" || len(root.Children) != 1 {
		t.Fatalf("root = %+v", root)
	}
	project := root.Children[0]
	if project.Label != "Project" || len(project.Children) != 1 {
		t.Fatalf("project = %+v", project)
	}
	filter := project.Children[0]
	if filter.Label != "ColumnStoreFilter" || filter.Detail != "t.id = 1 index" || filter.Rows != 1 || len(filter.Children) != 1 {
		t.Fatalf("filter = %+v", filter)
	}
	if scan := filter.Children[0]; scan.Label != "ColumnStoreScan" || scan.Detail != "tobira.t PRIMARY KEY (id)" || scan.Rows != -1 {
		t.Errorf("scan = %+v", scan)
	}
	two, err := parseSingleStorePlan(`{"explain": [{"executor": "A"}, {"executor": "B"}]}`)
	if err != nil || two.Label != "plan" || len(two.Children) != 2 {
		t.Errorf("two roots = %+v, %v", two, err)
	}
	for _, bad := range []string{"not json", "{}", `{"explain": []}`, `{"explain": "x"}`} {
		if _, err := parseSingleStorePlan(bad); err == nil {
			t.Errorf("parseSingleStorePlan(%q) succeeded", bad)
		}
	}
}

func TestKeysetWhereExpanded(t *testing.T) {
	schema := &driver.CollectionSchema{
		Ref:        driver.CollectionRef{Namespace: "db", Name: "t"},
		Fields:     []driver.Field{{Name: "a", Type: driver.TypeInt}, {Name: "b", Type: driver.TypeString}, {Name: "c", Type: driver.TypeInt}},
		PrimaryKey: []string{"a", "b", "c"},
	}
	cursor, err := encodeKeysetCursor([]driver.Value{driver.Int(1), driver.String("x"), driver.Int(3)})
	if err != nil {
		t.Fatal(err)
	}
	pk := []string{"`a`", "`b`", "`c`"}
	row := &browseQuery{op: "browse", schema: schema}
	if err := row.keysetWhere(cursor, pk); err != nil {
		t.Fatal(err)
	}
	if row.where[0] != "(`a`, `b`, `c`) > (?, ?, ?)" || len(row.params) != 3 {
		t.Errorf("row comparison = %q, %d params", row.where[0], len(row.params))
	}
	exp := &browseQuery{op: "browse", schema: schema, expandKeyset: true}
	if err := exp.keysetWhere(cursor, pk); err != nil {
		t.Fatal(err)
	}
	want := "(`a` > ? OR (`a` = ? AND `b` > ?) OR (`a` = ? AND `b` = ? AND `c` > ?))"
	if exp.where[0] != want {
		t.Errorf("expanded = %q, want %q", exp.where[0], want)
	}
	var got []string
	for _, p := range exp.params {
		got = append(got, p.String())
	}
	if strings.Join(got, ",") != "1,1,x,1,x,3" {
		t.Errorf("expanded params = %v", got)
	}
}

func TestCheckTxStatementRefusesHiddenStatements(t *testing.T) {
	for _, text := range []string{
		"SET STATEMENT max_statement_time=1 FOR CREATE TABLE x (a int)",
		"set statement max_statement_time=1 for select 1",
		"IF 1 THEN COMMIT AND CHAIN; END IF",
		"WHILE 1 DO COMMIT; END WHILE",
		"lbl: BEGIN NOT ATOMIC COMMIT; END",
		"BACKUP STAGE START",
		"BACKUP LOCK t",
	} {
		if err := checkTxStatement(text, lexMode{}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("checkTxStatement(%q) = %v, want ErrUnsupported", text, err)
		}
	}
}
