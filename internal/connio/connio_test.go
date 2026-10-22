package connio

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
)

// fixtureSecrets are the passwords the synthetic fixtures hold, raw and
// unescaped. None may reach a plan, a config or a message.
var fixtureSecrets = []string{"Fx-S3cret-Pw-42", `Fx:S3c\ret`, `Fx\:S3c\\ret`}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// summary renders candidates without their passwords, for comparison.
func summary(cands []Candidate) []string {
	var out []string
	for _, c := range cands {
		s := c.Label + " | "
		if c.Skip != "" {
			s += "skip: " + c.Skip
		} else {
			s += c.Conn.Name + " " + c.Conn.Driver + " " + c.Conn.DSN
			if !c.Password.IsZero() {
				s += " +pw"
			}
		}
		if len(c.Notes) > 0 {
			s += " (" + strings.Join(c.Notes, "; ") + ")"
		}
		out = append(out, s)
	}
	return out
}

func assertNoSecret(t *testing.T, what string, texts ...string) {
	t.Helper()
	for _, s := range texts {
		for _, sec := range fixtureSecrets {
			if strings.Contains(s, sec) {
				t.Errorf("%s leaks a fixture password: %q", what, s)
			}
		}
	}
}

func TestParseDBeaver(t *testing.T) {
	cands, err := ParseDBeaver(fixture(t, "data-sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Orders DB | Orders DB postgres postgres://alice@db.example.test:5432/orders +pw",
		"local-pg | local-pg mysql mysql://bob@localhost:3306/shop",
		"Maria | Maria mariadb mariadb://maria.example.test:3307/app",
		"Local file | Local file sqlite sqlite:/tmp/fixture.db",
		"Legacy Oracle | Legacy Oracle oracle oracle://scott@ora.example.test:1521 +pw",
		"URL only | URL only postgres postgres://carol@url.example.test:6543/reports +pw (password taken from the JDBC URL; other JDBC URL parameters were not imported)",
		"Events | Events clickhouse clickhouse://default@ch.example.test:8123/events",
		"entry 8 | skip: malformed entry",
		"Billing | Billing mssql sqlserver://sa@sql.example.test:1433?database=billing",
		"Ring | Ring cassandra cassandra://cass.example.test:9042/metrics",
		"Analytics | Analytics bigquery bigquery://acme-analytics",
		"Dynamo | Dynamo dynamodb dynamodb://eu-west-1",
		"Dynamo local | skip: no AWS region: a DynamoDB host must be dynamodb.<region>.amazonaws.com; add it with tobira connections add",
		"Search | Search elasticsearch elasticsearch://elastic@es.example.test:9200",
		"Turso | Turso libsql libsql://db-acme.turso.example.test",
		"Warehouse | Warehouse snowflake snowflake://ada@xy12345.eu-west-1/SALES",
		"Duck | Duck duckdb duckdb:///tmp/fixture.duckdb",
		"Columnar | Columnar vertica vertica://dbadmin@vt.example.test:5433/analytics",
		"RS | RS redshift redshift://awsuser@rs.example.test:5439/dev",
		"Roach | Roach cockroachdb cockroachdb://root@crdb.example.test:26257/bank",
		"GP | GP greenplum greenplum://gp.example.test/warehouse",
		"S2 | S2 singlestore singlestore://admin@s2.example.test:3306/app",
		"Duck file | Duck file duckdb duckdb:///tmp/other.duckdb",
		"Logs | Logs opensearch opensearch://admin@os.example.test:9200",
	}
	if got := summary(cands); !reflect.DeepEqual(got, want) {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if cands[0].Password.Reveal() != "Fx-S3cret-Pw-42" {
		t.Error("the plain configuration.password was not picked up")
	}
	for _, c := range cands {
		assertNoSecret(t, "candidate", c.Label, c.Skip, c.Conn.DSN, c.Conn.Name, strings.Join(c.Notes, " "))
	}
}

func TestParsePgpass(t *testing.T) {
	cands, err := ParsePgpass(fixture(t, "pgpass"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"line 3 | db.example.test-orders postgres postgres://alice@db.example.test:5432/orders +pw",
		"line 4 | db.example.test-orders postgres postgres://audit@db.example.test:5432/orders +pw",
		"line 5 | ::1-we\\ird postgres postgres://bob@[::1]:5433/we%5Cird +pw",
		"line 6 | skip: wildcard (*) in hostname and database: it matches many servers, not one connection",
		"line 7 | skip: malformed line: want hostname:port:database:username:password",
		"line 8 | replica.example.test-analytics postgres postgres://dave@replica.example.test/analytics",
		"line 9 | skip: malformed line: want hostname:port:database:username:password",
		"line 10 | localhost-local-pg postgres postgres://eve@localhost:5432/local-pg +pw",
	}
	if got := summary(cands); !reflect.DeepEqual(got, want) {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if cands[2].Password.Reveal() != `Fx:S3c\ret` {
		t.Errorf("escapes not decoded")
	}
	for _, c := range cands {
		assertNoSecret(t, "candidate", c.Label, c.Skip, c.Conn.DSN, c.Conn.Name)
	}
}

func TestPgpassEscapes(t *testing.T) {
	cases := map[string][]string{
		`h:1:d:u:p`:               {"h", "1", "d", "u", "p"},
		`h\:x:1:d\\b:u:p\:q\\r`:   {"h:x", "1", `d\b`, "u", `p:q\r`},
		`h:1:d:u:pw:extra:fields`: {"h", "1", "d", "u", "pw", "extra", "fields"},
		`h:1:d:u:`:                {"h", "1", "d", "u", ""},
		`h:1:d:u:trailing\`:       {"h", "1", "d", "u", `trailing\`},
	}
	for in, want := range cases {
		got, _ := splitPgpass(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	// Only an unescaped lone * is a wildcard.
	if _, wild := splitPgpass(`\*:1:d:u:p`); wild[0] {
		t.Error(`\* counted as a wildcard`)
	}
	if _, wild := splitPgpass(`*:1:d:u:p`); !wild[0] {
		t.Error(`* not counted as a wildcard`)
	}
}

func TestCheckPgpassMode(t *testing.T) {
	for mode, ok := range map[fs.FileMode]bool{0o600: true, 0o400: true, 0o640: false, 0o604: false, 0o644: false} {
		err := CheckPgpassMode(mode)
		if (err == nil) != ok {
			t.Errorf("%#o: err=%v", mode, err)
		}
	}
	if err := CheckPgpassMode(fs.ModeDir | 0o700); err == nil {
		t.Error("a directory passed")
	}
}

func TestParseTablePlus(t *testing.T) {
	cands, err := ParseTablePlus(fixture(t, "Connections.plist"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Prod PG | Prod PG postgres postgres://app@pg.example.test:5432/app",
		"Cache | Cache redis redis://127.0.0.1:6379/2",
		"Notes | Notes sqlite sqlite:/tmp/notes.db",
		"Warehouse | Warehouse snowflake snowflake://acct.example.test",
		"Docs | Docs mongodb mongodb://reader@mongo.example.test/docs",
		"Ledger | Ledger mssql sqlserver://sa@sql.example.test:1433?database=ledger",
		"Ora | Ora oracle oracle://scott@ora.example.test:1521/FREEPDB1",
		"Ring | Ring cassandra cassandra://cass.example.test/metrics",
		"BQ | BQ bigquery bigquery://acme-analytics",
		"Duck | Duck duckdb duckdb:///tmp/notes.duckdb",
		"Turso | Turso libsql libsql://db-acme.turso.example.test",
		"Edge | skip: a Cloudflare D1 connection needs the account and database ids, which the import does not carry; add it with tobira connections add",
		"Columnar | Columnar vertica vertica://dbadmin@vt.example.test/analytics",
		"RS | RS redshift redshift://rs.example.test:5439/dev",
		"Roach | Roach cockroachdb cockroachdb://crdb.example.test:26257/bank",
		"GP | GP greenplum greenplum://gp.example.test",
		"Maria | Maria mariadb mariadb://maria.example.test/app",
		"S2 | S2 singlestore singlestore://s2.example.test",
		"Search | Search elasticsearch elasticsearch://elastic@es.example.test:9200",
	}
	if got := summary(cands); !reflect.DeepEqual(got, want) {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range cands {
		if !c.Password.IsZero() {
			t.Error("a TablePlus password was read")
		}
	}
}

func TestParseFile(t *testing.T) {
	cands, err := ParseFile(fixture(t, "export.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"local-pg | local-pg postgres postgres://app@localhost:5432/app",
		"vault-db | vault-db mysql mysql://svc@mysql.example.test:3306/svc",
		"has-literal | skip: password must be a secret reference (env:NAME, file:///path, dotenv://path#KEY, keyring:NAME)",
		"has spaces | has spaces sqlite sqlite:./spaces.db",
	}
	if got := summary(cands); !reflect.DeepEqual(got, want) {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if cands[1].Conn.Password != "op://Fixtures/svc/password" || !cands[1].Conn.ReadOnly || cands[1].Conn.Options["tls"] != "preferred" {
		t.Errorf("fields lost: %+v", cands[1].Conn)
	}
}

func TestMalformedInputsGiveClearErrors(t *testing.T) {
	cases := []struct {
		name  string
		parse func([]byte) ([]Candidate, error)
		in    string
		want  string
	}{
		{"dbeaver syntax", ParseDBeaver, `{"connections": {"a": {"password": "Fx-S3cret-Pw-42"`, "not valid JSON"},
		{"dbeaver no connections", ParseDBeaver, `{"folders": {}}`, "no connections object"},
		{"dbeaver wrong type", ParseDBeaver, `{"connections": ["Fx-S3cret-Pw-42"]}`, "connections is not an object"},
		{"dbeaver not object", ParseDBeaver, `["x"]`, "not a DBeaver data-sources.json"},
		{"pgpass binary", ParsePgpass, "h:1:d:u:Fx-S3cret-Pw-42\x00\xff", "not a text file"},
		{"tableplus garbage", ParseTablePlus, "Fx-S3cret-Pw-42 <plist", "not a property list"},
		{"tableplus not array", ParseTablePlus, `<?xml version="1.0"?><plist version="1.0"><dict><key>a</key><string>Fx-S3cret-Pw-42</string></dict></plist>`, "not a TablePlus connections list"},
		{"file syntax", ParseFile, "connections:\n  - name: a\n    password: [Fx-S3cret-Pw-42\n", "not valid YAML or JSON"},
		{"file shape", ParseFile, "connections: Fx-S3cret-Pw-42\n", "connections must be a list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.parse([]byte(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			assertNoSecret(t, "error", err.Error())
		})
	}
}

func TestPlanSanitisesAndRenames(t *testing.T) {
	cands := []Candidate{
		{Label: "a", Conn: config.Connection{Name: "Orders DB", Driver: "postgres", DSN: "postgres://h/db"}},
		{Label: "b", Conn: config.Connection{Name: "local-pg", Driver: "postgres", DSN: "postgres://h/db"}},
		{Label: "c", Conn: config.Connection{Name: "local-pg", Driver: "postgres", DSN: "postgres://h/db"}},
		{Label: "d", Conn: config.Connection{Name: "local-pg", Driver: "postgres", DSN: "postgres://h/db"}},
		{Label: "e", Skip: "no driver"},
		{Label: "f", Conn: config.Connection{Name: "%%%", Driver: "sqlite", DSN: "sqlite:x"}},
		{Label: "g", Conn: config.Connection{Name: "bad", Driver: "postgres", DSN: "postgres://u:Fx-S3cret-Pw-42@h/db"}},
		{Label: "Fx-S3cret-Pw-42 name", Conn: config.Connection{Name: "Fx-S3cret-Pw-42 name", Driver: "sqlite", DSN: "sqlite:y"}, Password: driver.NewSecret("Fx-S3cret-Pw-42")},
	}
	cands = append(cands,
		Candidate{Label: "h", Conn: config.Connection{Name: "events", Driver: "clickhouse", DSN: "clickhouse://h/db"}},
		Candidate{Label: "i", Conn: config.Connection{Name: "byscheme", DSN: "clickhouse://h/db"}},
		Candidate{Label: "j", Conn: config.Connection{Name: "events", Driver: "postgres", DSN: "postgres://h/db"}})
	available := func(c config.Connection) bool {
		return c.Driver != "clickhouse" && !strings.HasPrefix(c.DSN, "clickhouse:")
	}
	plan := Plan(cands, []string{"local-pg", "local-pg-imported"}, "-imported", available, false)
	var got []string
	for _, e := range plan {
		got = append(got, string(e.Action)+" "+e.Label+" ["+e.Original+"] "+e.From+" -> "+e.Conn.Name+" "+e.Reason)
	}
	want := []string{
		"add a [Orders DB] Orders-DB -> Orders-DB ",
		"rename b [local-pg] local-pg -> local-pg-imported-2 ",
		"rename c [local-pg] local-pg -> local-pg-imported-3 ",
		"rename d [local-pg] local-pg -> local-pg-imported-4 ",
		"skip e []  ->  no driver",
		"add f [%%%] sqlite -> sqlite ",
		"skip g []  ->  DSN contains a password; put it in a password ref (env:NAME, file:///path, dotenv://path#KEY, keyring:NAME) instead",
		"add xxxxx name [xxxxx name] xxxxx-name -> xxxxx-name ",
		"skip h []  ->  driver not available in this build",
		"skip i []  ->  driver not available in this build",
		"add j [events] events -> events ",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPlanKeepsUnavailableWhenAsked(t *testing.T) {
	cands := []Candidate{
		{Label: "a", Conn: config.Connection{Name: "a", Driver: "memdemo", DSN: "memdemo://x"}},
		{Label: "b", Conn: config.Connection{Name: "b", DSN: "clickhouse://h/db"}},
	}
	plan := Plan(cands, nil, "-imported", func(config.Connection) bool { return false }, true)
	var got []string
	for _, e := range plan {
		got = append(got, string(e.Action)+" "+e.Conn.Name+" "+strings.Join(e.Notes, "; "))
	}
	want := []string{
		"add a driver memdemo not available in this build; install the plugin",
		"add b driver clickhouse not available in this build; install the plugin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestDBeaverURLPasswordWithHost(t *testing.T) {
	in := `{"connections": {"a": {"provider": "postgresql", "name": "A", "configuration": {
		"host": "h", "port": "5432", "database": "db", "user": "u",
		"url": "jdbc:postgresql://h:5432/db?password=Fx-S3cret-Pw-42"}}}}`
	cands, err := ParseDBeaver([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got := summary(cands)
	want := []string{"A | A postgres postgres://u@h:5432/db +pw (password taken from the JDBC URL)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	if cands[0].Password.Reveal() != "Fx-S3cret-Pw-42" {
		t.Error("URL password not offered")
	}
}

func TestDBeaverDeepNestingIsAClearError(t *testing.T) {
	in := `{"connections": {"a": ` + strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + `}}`
	_, err := ParseDBeaver([]byte(in))
	if err == nil || !strings.Contains(err.Error(), "too deeply nested") {
		t.Errorf("err = %v", err)
	}
}

func TestStripDSNPassword(t *testing.T) {
	cases := []struct{ in, want string }{
		{"postgres://alice:Fx-S3cret-Pw-42@h:5432/db?sslmode=require", "postgres://alice@h:5432/db?sslmode=require"},
		{"postgres://:Fx-S3cret-Pw-42@h/db", "postgres://h/db"},
		{"postgres://alice@h/db?password=Fx-S3cret-Pw-42&sslmode=disable", "postgres://alice@h/db?sslmode=disable"},
		{"postgres://alice@h/db?sslmode=disable&password=Fx-S3cret-Pw-42", "postgres://alice@h/db?sslmode=disable"},
		{"host=h user=alice password=Fx-S3cret-Pw-42 dbname=db", "host=h user=alice dbname=db"},
		{"password = 'Fx S3cret' host=h", "host=h"},
		{"alice:Fx-S3cret-Pw-42@tcp(h:3306)/db?parseTime=true", "alice@tcp(h:3306)/db?parseTime=true"},
		{"mysql://alice:Fx-S3cret-Pw-42@h:3306/db", "mysql://alice@h:3306/db"},
		{"sqlite:./x.db", "sqlite:./x.db"},
		{"Server=h;User Id=u;Password=Fx-S3cret-Pw-42;Database=d", "Server=h;User Id=u;Database=d"},
		{"server=h;pwd=Fx-S3cret-Pw-42;", "server=h;"},
		{`Server=h;PWD="a;b""c";Database=d`, "Server=h;Database=d"},
		{"file:./x.db?_auth_user=u&_auth_pass=Fx-S3cret-Pw-42&mode=ro", "file:./x.db?_auth_user=u&mode=ro"},
		{"sqlite:x.db?_auth_pass=Fx-S3cret-Pw-42", "sqlite:x.db"},
		{"sqlite:///x.db?_auth_pass=Fx-S3cret-Pw-42", "sqlite:///x.db"},
		{"Server=tcp:h,1433;Database=d;Pwd={Fx;S3cret-Pw-42};", "Server=tcp:h,1433;Database=d;"},
		{"host=h options='-c a=b;c' password=Fx-S3cret-Pw-42", "host=h options='-c a=b;c'"},
		{"postgres://u@h/db?options=a;b&password=Fx-S3cret-Pw-42", "postgres://u@h/db?options=a;b"},
	}
	for _, tc := range cases {
		got, had := StripDSNPassword(tc.in)
		if got != tc.want || had != (tc.in != tc.want) {
			t.Errorf("%q: got %q had=%v, want %q", tc.in, got, had, tc.want)
		}
	}
}

func TestExportDropsSecrets(t *testing.T) {
	in := []config.Connection{
		{Name: "a", Driver: "postgres", DSN: "postgres://u:Fx-S3cret-Pw-42@h/db", Password: "env:PW"},
		{Name: "b", Driver: "postgres", DSN: "host=h password=Fx-S3cret-Pw-42", Password: "Fx-S3cret-Pw-42"},
		{Name: "c", Driver: "mysql", DSN: "u:Fx-S3cret-Pw-42@tcp(h)/db", Options: map[string]string{"api_token": "Fx-S3cret-Pw-42", "tls": "true"}},
		{Name: "d", Driver: "sqlite", DSN: "sqlite:x", Password: "file:///run/secrets/d"},
		{Name: "e", Driver: "sqlite", DSN: "file:e.db?_auth_pass=Fx-S3cret-Pw-42"},
		{Name: "f", Driver: "mssql", DSN: "Server=h;Password=Fx-S3cret-Pw-42"},
	}
	out, notes := Export(in)
	want := []config.Connection{
		{Name: "a", Driver: "postgres", DSN: "postgres://u@h/db", Password: "env:PW"},
		{Name: "b", Driver: "postgres", DSN: "host=h"},
		{Name: "c", Driver: "mysql", DSN: "u@tcp(h)/db", Options: map[string]string{"tls": "true"}},
		{Name: "d", Driver: "sqlite", DSN: "sqlite:x", Password: "file:///run/secrets/d"},
		{Name: "e", Driver: "sqlite", DSN: "file:e.db"},
		{Name: "f", Driver: "mssql", DSN: "Server=h"},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("got %+v\nwant %+v", out, want)
	}
	wantNotes := []string{
		"a: removed the password from the DSN",
		"b: dropped a password that is not a secret reference",
		"b: removed the password from the DSN",
		"c: removed the password from the DSN",
		"c: dropped option \"api_token\", which looks like a secret",
		"e: removed the password from the DSN",
		"f: removed the password from the DSN",
	}
	if !reflect.DeepEqual(notes, wantNotes) {
		t.Errorf("notes = %q", notes)
	}
	j, err := MarshalJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "json", string(j), strings.Join(notes, "\n"))
	back, err := ParseFile(j)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range back {
		if !reflect.DeepEqual(c.Conn, want[i]) {
			t.Errorf("json round trip %d: %+v", i, c.Conn)
		}
	}
}
