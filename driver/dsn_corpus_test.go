package driver

import (
	"strings"
	"testing"
)

// leakCorpus is every DSN form the redaction must handle, each with the
// secret substrings that must never survive RedactDSN or RedactMessage. It
// covers URL userinfo and query passwords, go-sql-driver user:pass@net()
// and @/ forms (passwords holding '=', ';', '?', '@'), libpq key/value,
// ADO and ODBC with quotes and braces ("}}", ';' inside), odbc: with '@',
// file: and sqlite: paths with '@', '=' and ';', and Windows paths.
var leakCorpus = []struct {
	dsn     string
	secrets []string
}{
	// URLs
	{"postgres://app:hunter2zz@h:5432/db?sslmode=disable", []string{"hunter2zz"}},
	{"postgres://app:p%40ssWord9@h/db", []string{"p%40ssWord9"}},
	{"postgresql://h/db?password=Qwerty77&a=1", []string{"Qwerty77"}},
	{"postgres://u:p@ssW9x@h/db", []string{"ssW9x"}},
	{"postgres://u:p#ssW9x@h/db", []string{"ssW9x"}},
	{"postgres://u:p/ssW9x@h/db", []string{"ssW9x"}},
	{"postgres://u:p?ssW9x@h/db", []string{"ssW9x"}},
	{"postgres://u:Pw9zz@h/db?x=a@b&sslpassword=Ssl9zz", []string{"Pw9zz", "Ssl9zz"}},
	{"postgres://h:5432/db?email=a@b&password=Zz9zz", []string{"Zz9zz"}},
	{"postgres://h/db?a=1;password=Zz9zz#f", []string{"Zz9zz"}},
	{"mysql://alice:My5ecret@h:3306/db", []string{"My5ecret"}},
	{"mongodb+srv://u:Mongo9pw@cluster.example.net/db?retryWrites=true", []string{"Mongo9pw"}},
	{"mongodb://u:Mongo9pw@h1:27017,h2:27017/db?replicaSet=rs", []string{"Mongo9pw"}},
	{"redis://:Redis9pw@h:6379/0", []string{"Redis9pw"}},
	{"rediss://default:Redis9pw@h:6380", []string{"Redis9pw"}},
	{"sqlserver://sa:Sql9pw!x@h:1433?database=d", []string{"Sql9pw!x"}},
	{"sqlserver://sa@h?database=d&password=Sql9pw!x", []string{"Sql9pw!x"}},
	{"oracle://u:Ora9pw@h:1521/svc", []string{"Ora9pw"}},
	{"clickhouse://u:Ch9pw@h:9000/db?dial_timeout=1s", []string{"Ch9pw"}},
	{"sqlite:///x.db?_auth_pass=Lite9pw&mode=ro", []string{"Lite9pw"}},
	{"sqlserver://u@h?database=d&password=Se;c9pw", []string{"Se;c9pw", "c9pw"}},
	{"sqlserver://u@h?password=Se;c9pw&database=d", []string{"c9pw"}},
	{"postgres://h/db?sslpassword=a;b9zz#frag", []string{"b9zz"}},
	{"file:x.db?_auth_pass=Au;th9pw&mode=ro", []string{"th9pw"}},
	// URL query values holding ';'
	{"sqlserver://u@h?password=Se;c9pw;x&database=d", []string{"c9pw"}},
	{"mysql://u@h:3306/d?tls=true&password=Se;c9pw", []string{"c9pw"}},
	{"sqlite:x.db?_auth_pass=Se;c9pw", []string{"c9pw"}},
	{"sqlserver://u@h?password=Se;a=c9pw&database=d", []string{"c9pw"}},
	// userinfo shapes a strict URL reading misses
	{"root:Sec9pw@tcp(h:3306)/d?loc=x@y", []string{"Sec9pw"}},
	{"root:Sec9pw@cloudsql-mysql(proj:reg:inst)/d", []string{"Sec9pw"}},
	{"root:Sec9pw@tcp-x(h)/d", []string{"Sec9pw"}},
	{"postgres://myuser@myserver:Sec9pw@myserver.postgres.database.azure.com/db", []string{"Sec9pw"}},
	{"postgres://myuser@myserver:Pa/ss9pw@myserver.postgres.database.azure.com/db", []string{"ss9pw"}},
	{"postgres://me@corp.com:Sec9pw@h/d", []string{"Sec9pw"}},
	{"root:Sec9pw@h:3306/d", []string{"Sec9pw"}},
	{"root:Sec9pw@h", []string{"Sec9pw"}},
	{"root:Sec9pw@(h:3306)/d", []string{"Sec9pw"}},
	{"me@corp:Sec9pw@tcp(h)/d", []string{"Sec9pw"}},
	// assorted forms
	{"file:test.db?password=Sx9z&x=1", []string{"Sx9z"}},
	{"postgres://u:Se@c9pw@h/db", []string{"Se@c9pw", "c9pw"}},
	{"postgres://u:Se#c9pw@h/db", []string{"c9pw"}},
	{"postgres://h/db?password=Se@c9pw", []string{"c9pw"}},
	{"root:Se&c9pw@tcp(h)/d", []string{"c9pw"}},
	{"root:Se c9pw@tcp(h)/d", []string{"c9pw"}},
	{"jdbc:sqlserver://h;user=u;password=Jd9pw;database=d", []string{"Jd9pw"}},
	{"snowflake://u:Snow9pw@acct/db/sch?warehouse=w", []string{"Snow9pw"}},
	{"cassandra://u:Cass9pw@h:9042/ks", []string{"Cass9pw"}},
	{"POSTGRES://u:Up9pw@h/db", []string{"Up9pw"}},
	// a non-go-sql-driver user:/…@host whose password starts with '/'
	{"user:/Slash9pw@h:3306/d", []string{"Slash9pw"}},
	{"user:/Slash9pw@h", []string{"Slash9pw"}},
	// ODBC braces
	{"odbc:server=h;uid=sa;pwd={Sup3r;S3cret};database=d", []string{"Sup3r", "S3cret"}},
	// go-sql-driver
	{"user:hunter2zz@tcp(h:3306)/db?parseTime=true", []string{"hunter2zz"}},
	{"root:Se=c9pw@tcp(h:3306)/d", []string{"Se=c9pw", "c9pw"}},
	{"admin:Se;c9pw@tcp(h)/d?parseTime=true", []string{"Se;c9pw", "c9pw"}},
	{"root:a=b?Sec9pw@tcp(h)/d", []string{"Sec9pw"}},
	{"root:Sec9@pw@tcp(h)/d", []string{"Sec9", "@pw@"}},
	{"u:Unix9pw@unix(/tmp/mysql.sock)/d", []string{"Unix9pw"}},
	{"u:Slash9pw@/d", []string{"Slash9pw"}},
	{"r:Pw9=x;y?z@tcp(h)/d?tls=true", []string{"Pw9=x", "y?z"}},
	// libpq
	{"host=h user=app password=hunter2zz dbname=db", []string{"hunter2zz"}},
	{"host=h password='hun ter2zz' dbname=db", []string{"hun ter2zz", "ter2zz"}},
	{`password='it\'s a secret' host=h`, []string{"it\\'s", "secret"}},
	{"host=h password = hunter2zz dbname=db", []string{"hunter2zz"}},
	{"host=h options='-c a=b;c' password=Opt9pw", []string{"Opt9pw"}},
	{"password=a;b9zz host=h", []string{"b9zz"}},
	{"host=h user=a@b password=Kv9pw@x", []string{"Kv9pw", "@x"}},
	{`password=hun\ ter2zz host=h`, []string{"ter2zz"}},
	{"host=h sslpassword=Ssl9zz", []string{"Ssl9zz"}},
	// ADO / ODBC
	{"Server=h;User Id=u;Password=hunter2zz;Database=d", []string{"hunter2zz"}},
	{"Server=h; PASSWORD = hun ter2zz ;Database=d", []string{"hun ter2zz", "ter2zz"}},
	{`Server=h;Password="a;b""c9zz";Database=d`, []string{"c9zz", `b""c`}},
	{"Server=h;Password='a;b''c9zz';Database=d", []string{"c9zz"}},
	{"Server=tcp:h,1433;Database=d;Pwd={Abc;Tail99};", []string{"Abc;Tail99", "Tail99"}},
	{"Server=h;Password={a}};b}}c9zz};Database=d", []string{"b}}c9zz", "c9zz"}},
	{"Server=h;Password={abc;def9zz", []string{"def9zz"}},
	{"Password=hunter2zz;Server=h", []string{"hunter2zz"}},
	{"Server=h;Uid=me@corp.com;Pwd=Ado9@pw;Database=d", []string{"Ado9", "@pw"}},
	{"odbc:server=h;uid=sa;pwd={a;Sup3r}}S3cret;b};database=d", []string{"Sup3r", "S3cret"}},
	{"odbc:server=h;PASSWORD={x;yy9zz};database=d", []string{"yy9zz"}},
	{"odbc:server=h;uid=me@corp.com;pwd=Secret1xyz;database=d", []string{"Secret1xyz"}},
	{"odbc:server=h;uid=me@corp.com;pwd={Sec;ret1xyz};database=d", []string{"ret1xyz"}},
	{"odbc:server=h;uid=sa;pwd=Sec@ret1xyz;database=d", []string{"Sec@", "ret1xyz"}},
	{"custom:host=h;user=me@corp;password=Sec@ret9;x=1", []string{"Sec@", "ret9"}},
	{"custom:host=h;password=ab?Sec9;x=1", []string{"Sec9"}},
	// secret parameter names, whole-name and case-insensitive: URL query, key/value and ADO forms
	{"scheme://h/db?a=1&credentials=Sv00Zq9x&b=2", []string{"Sv00Zq9x"}},
	{"host=h credentials=Sv00Zq9x dbname=db", []string{"Sv00Zq9x"}},
	{"Server=h;credentials=Sv00Zq9x;Database=d", []string{"Sv00Zq9x"}},
	{"scheme://h/db?a=1&CREDENTIALS_JSON=Sv01Zq9x&b=2", []string{"Sv01Zq9x"}},
	{"host=h CREDENTIALS_JSON=Sv01Zq9x dbname=db", []string{"Sv01Zq9x"}},
	{"Server=h;CREDENTIALS_JSON=Sv01Zq9x;Database=d", []string{"Sv01Zq9x"}},
	{"scheme://h/db?a=1&private_key=Sv02Zq9x&b=2", []string{"Sv02Zq9x"}},
	{"host=h private_key=Sv02Zq9x dbname=db", []string{"Sv02Zq9x"}},
	{"Server=h;private_key=Sv02Zq9x;Database=d", []string{"Sv02Zq9x"}},
	{"scheme://h/db?a=1&PRIVATEKEY=Sv03Zq9x&b=2", []string{"Sv03Zq9x"}},
	{"host=h PRIVATEKEY=Sv03Zq9x dbname=db", []string{"Sv03Zq9x"}},
	{"Server=h;PRIVATEKEY=Sv03Zq9x;Database=d", []string{"Sv03Zq9x"}},
	{"scheme://h/db?a=1&token=Sv04Zq9x&b=2", []string{"Sv04Zq9x"}},
	{"host=h token=Sv04Zq9x dbname=db", []string{"Sv04Zq9x"}},
	{"Server=h;token=Sv04Zq9x;Database=d", []string{"Sv04Zq9x"}},
	{"scheme://h/db?a=1&AUTH_TOKEN=Sv05Zq9x&b=2", []string{"Sv05Zq9x"}},
	{"host=h AUTH_TOKEN=Sv05Zq9x dbname=db", []string{"Sv05Zq9x"}},
	{"Server=h;AUTH_TOKEN=Sv05Zq9x;Database=d", []string{"Sv05Zq9x"}},
	{"scheme://h/db?a=1&authtoken=Sv06Zq9x&b=2", []string{"Sv06Zq9x"}},
	{"host=h authtoken=Sv06Zq9x dbname=db", []string{"Sv06Zq9x"}},
	{"Server=h;authtoken=Sv06Zq9x;Database=d", []string{"Sv06Zq9x"}},
	{"scheme://h/db?a=1&ACCESS_TOKEN=Sv07Zq9x&b=2", []string{"Sv07Zq9x"}},
	{"host=h ACCESS_TOKEN=Sv07Zq9x dbname=db", []string{"Sv07Zq9x"}},
	{"Server=h;ACCESS_TOKEN=Sv07Zq9x;Database=d", []string{"Sv07Zq9x"}},
	{"scheme://h/db?a=1&secret=Sv08Zq9x&b=2", []string{"Sv08Zq9x"}},
	{"host=h secret=Sv08Zq9x dbname=db", []string{"Sv08Zq9x"}},
	{"Server=h;secret=Sv08Zq9x;Database=d", []string{"Sv08Zq9x"}},
	{"scheme://h/db?a=1&CLIENT_SECRET=Sv09Zq9x&b=2", []string{"Sv09Zq9x"}},
	{"host=h CLIENT_SECRET=Sv09Zq9x dbname=db", []string{"Sv09Zq9x"}},
	{"Server=h;CLIENT_SECRET=Sv09Zq9x;Database=d", []string{"Sv09Zq9x"}},
	{"scheme://h/db?a=1&api_key=Sv10Zq9x&b=2", []string{"Sv10Zq9x"}},
	{"host=h api_key=Sv10Zq9x dbname=db", []string{"Sv10Zq9x"}},
	{"Server=h;api_key=Sv10Zq9x;Database=d", []string{"Sv10Zq9x"}},
	{"scheme://h/db?a=1&APIKEY=Sv11Zq9x&b=2", []string{"Sv11Zq9x"}},
	{"host=h APIKEY=Sv11Zq9x dbname=db", []string{"Sv11Zq9x"}},
	{"Server=h;APIKEY=Sv11Zq9x;Database=d", []string{"Sv11Zq9x"}},
	{"scheme://h/db?a=1&api-key=Sv12Zq9x&b=2", []string{"Sv12Zq9x"}},
	{"host=h api-key=Sv12Zq9x dbname=db", []string{"Sv12Zq9x"}},
	{"Server=h;api-key=Sv12Zq9x;Database=d", []string{"Sv12Zq9x"}},
	{"scheme://h/db?a=1&KEY=Sv13Zq9x&b=2", []string{"Sv13Zq9x"}},
	{"host=h KEY=Sv13Zq9x dbname=db", []string{"Sv13Zq9x"}},
	{"Server=h;KEY=Sv13Zq9x;Database=d", []string{"Sv13Zq9x"}},
	{"scheme://h/db?a=1&account_key=Sv14Zq9x&b=2", []string{"Sv14Zq9x"}},
	{"host=h account_key=Sv14Zq9x dbname=db", []string{"Sv14Zq9x"}},
	{"Server=h;account_key=Sv14Zq9x;Database=d", []string{"Sv14Zq9x"}},
	{"scheme://h/db?a=1&ACCOUNTKEY=Sv15Zq9x&b=2", []string{"Sv15Zq9x"}},
	{"host=h ACCOUNTKEY=Sv15Zq9x dbname=db", []string{"Sv15Zq9x"}},
	{"Server=h;ACCOUNTKEY=Sv15Zq9x;Database=d", []string{"Sv15Zq9x"}},
	{"scheme://h/db?a=1&sas=Sv16Zq9x&b=2", []string{"Sv16Zq9x"}},
	{"host=h sas=Sv16Zq9x dbname=db", []string{"Sv16Zq9x"}},
	{"Server=h;sas=Sv16Zq9x;Database=d", []string{"Sv16Zq9x"}},
	{"scheme://h/db?a=1&SIG=Sv17Zq9x&b=2", []string{"Sv17Zq9x"}},
	{"host=h SIG=Sv17Zq9x dbname=db", []string{"Sv17Zq9x"}},
	{"Server=h;SIG=Sv17Zq9x;Database=d", []string{"Sv17Zq9x"}},
	{`bigquery://p/ds?credentials={"type":"service_account","private_key":"PkJ9zzKey","client_email":"a@b"}&location=EU`, []string{"PkJ9zzKey"}},
	{`host=h credentials={"type":"service_account","private_key":"PkK9zzKey"} dbname=d`, []string{"PkK9zzKey"}},
	{`host=h credentials='{"type": "service_account", "private_key": "PkL9zzKey"}' dbname=d`, []string{"PkL9zzKey"}},
	{`Server=h;Credentials={"type":"service_account","private_key":"PkM9zzKey"};Database=d`, []string{"PkM9zzKey"}},
	{`Server=h;Credentials="{""type"":""service_account"",""private_key"":""PkN9zzKey""}";Database=d`, []string{"PkN9zzKey"}},
	// file: and sqlite:
	{"file:./x.db?_auth_user=u&_auth_pass=hunter2zz", []string{"hunter2zz"}},
	{"file:test.db?password=S3cretPw&k=v", []string{"S3cretPw"}},
	{"file:/tmp/a=b.db?password=S3cretPw&k=v", []string{"S3cretPw"}},
	{"file:/tmp/a@b.db?_auth_pass=Auth9pw", []string{"Auth9pw"}},
	{"file:/tmp/a;b.db?_auth_pass=Auth9pw&mode=ro", []string{"Auth9pw"}},
	{"file:/tmp/a@b;c=d.db?_auth_pass=Auth9pw", []string{"Auth9pw"}},
	{"sqlite:x.db?_auth_pass=Lite9pw", []string{"Lite9pw"}},
	{`C:\data\x.db?_auth_pass=Win9pw`, []string{"Win9pw"}},
	{`file:C:\data\a@b.db?_auth_pass=Win9pw`, []string{"Win9pw"}},
}

// TestRedactionCorpusNoLeak runs every reading on every DSN: whatever the
// form, no secret substring survives RedactDSN, RedactMessage (the DSN
// quoted in an error), or StripPassword, and HasPassword sees a password.
func TestRedactionCorpusNoLeak(t *testing.T) {
	for _, tc := range leakCorpus {
		red := RedactDSN(tc.dsn)
		msg := RedactMessage(`open "`+tc.dsn+`": connection refused`, ConnConfig{DSN: tc.dsn})
		stripped, had := StripPassword(tc.dsn)
		for _, s := range tc.secrets {
			if strings.Contains(red, s) {
				t.Errorf("RedactDSN(%q) = %q leaks %q", tc.dsn, red, s)
			}
			if strings.Contains(msg, s) {
				t.Errorf("RedactMessage(%q) = %q leaks %q", tc.dsn, msg, s)
			}
			if strings.Contains(stripped, s) {
				t.Errorf("StripPassword(%q) = %q leaks %q", tc.dsn, stripped, s)
			}
		}
		if stripped != "" && keepsPassword(tc.dsn, stripped) {
			t.Errorf("StripPassword(%q) = %q keeps a password value a reading found", tc.dsn, stripped)
		}
		if !HasPassword(tc.dsn) || !had {
			t.Errorf("HasPassword(%q) = %v, StripPassword had = %v; want both true", tc.dsn, HasPassword(tc.dsn), had)
		}
		if HasPassword(stripped) {
			t.Errorf("StripPassword(%q) = %q still has a password", tc.dsn, stripped)
		}
	}
}

// cleanCorpus are password-less DSNs every reading leaves untouched.
var cleanCorpus = []string{
	"postgres://app@h/db?sslmode=disable",
	"user@tcp(h)/db",
	"host=h user=app dbname=db options='-c a=b;c'",
	"Server=h;Database=d",
	"odbc:server=h;uid=sa;database=d",
	"odbc:server=h;uid=me@corp.com;database=d",
	"sqlite:./a.db",
	"file:/tmp/a=b;c.db?mode=ro",
	`C:\data\x.db`,
	"u@tcp(h:3306)/d?loc=x@y",
	"libsql://db-org.turso.io?tls=true",
	"bigquery://proj/ds?location=EU",
	"cassandra://h:9042/ks?keyspace=app&consistency=quorum",
	"host=h keyspace=app tokenizer=x signature=y monkey=1 dbname=d",
	"Server=h;Keyspace=app;SasUrl=none;Sigma=1;Database=d",
	"scheme://h/db?keyspace=app&secretary=x&tokens=2&sass=1&sigs=3&keys=4",
	"dynamodb://us-east-1?endpoint=http://localhost:8000&profile=p",
	"elasticsearch://h:9200?index=x",
	"d1://acct/db",
	"sqlserver://u@h?database=d",
	"snowflake://u@acct/db/sch?warehouse=w",
	"mysql://u@h:3306/d",
	"clickhouse://h:9000/db",
	"/var/lib/x.db",
}

// Passwordless DSNs stay untouched by every reading.
func TestRedactionCorpusKeepsCleanDSNs(t *testing.T) {
	for _, dsn := range cleanCorpus {
		if got := RedactDSN(dsn); got != dsn {
			t.Errorf("RedactDSN(%q) = %q, want it unchanged", dsn, got)
		}
		if HasPassword(dsn) {
			t.Errorf("HasPassword(%q) = true", dsn)
		}
		if got, had := StripPassword(dsn); got != dsn || had {
			t.Errorf("StripPassword(%q) = %q, had %v; want it unchanged", dsn, got, had)
		}
	}
}

// Password-less DSNs with a '@' in a path or after an address: no reading
// takes it for userinfo (path schemes such as sqlite: and file: have none),
// so every function leaves them alone.
func TestPathAtIsNotUserinfo(t *testing.T) {
	for _, dsn := range []string{
		"sqlite:/tmp/a@b.db",
		"file:/tmp/a@b.db?mode=ro",
		"u@tcp(h:3306)/d?loc=x@y",
	} {
		if got := RedactDSN(dsn); got != dsn {
			t.Errorf("RedactDSN(%q) = %q, want it unchanged", dsn, got)
		}
		if got, had := StripPassword(dsn); got != dsn || had {
			t.Errorf("StripPassword(%q) = %q, had %v; want it unchanged", dsn, got, had)
		}
	}
}

// StripPassword's output is always clean under every lenient reading:
// RedactDSN(StripPassword(x)) == StripPassword(x), for every corpus entry,
// clean or not.
func TestStripPasswordOutputIsRedactionFixedPoint(t *testing.T) {
	var all []string
	for _, tc := range leakCorpus {
		all = append(all, tc.dsn)
	}
	all = append(all, cleanCorpus...)
	all = append(all, "sqlite:/tmp/a@b.db", "file:/tmp/a@b.db?mode=ro", "postgres://u@h:5432/d?application_name=me@x",
		"mysql://u@h:3306/d?x=a@b", "postgres://h:5432/db?application_name=svc@prod")
	for _, dsn := range all {
		out, _ := StripPassword(dsn)
		if got := RedactDSN(out); got != out {
			t.Errorf("StripPassword(%q) = %q, but RedactDSN of it is %q", dsn, out, got)
		}
	}
}

// A DSN the strict reading cannot rewrite cleanly is refused, never
// returned partly stripped.
func TestStripPasswordRefusesAmbiguous(t *testing.T) {
	for _, dsn := range []string{
		"postgres://h:5432/db?application_name=svc@prod",
		// A query '@' after a user without a password: the lenient
		// reading cannot tell it from an Azure user@server:pa?ss@host
		// password, so RedactDSN over-reads it and StripPassword refuses.
		"postgres://u@h:5432/d?application_name=me@x",
		"mysql://u@h:3306/d?x=a@b",
		"user:/Slash9pw@h:3306/d",
		"root:Sec9pw@h:3306/d",
		"sqlserver://u@h?password=Se;a=c9pw&database=d",
		"postgres://u@h/db?a=1;password=S3cret;b=2#frag",
	} {
		if got, had := StripPassword(dsn); got != "" || !had {
			t.Errorf("StripPassword(%q) = %q, had %v; want \"\" (refused), had true", dsn, got, had)
		}
	}
}

// A JSON record's "key" field in a message (a change event line) is not a
// secret, while a key=value parameter still is.
func TestRedactMessageKeepsJSONKeyField(t *testing.T) {
	line := `{"op":"insert","key":"user:42","payload":{"key": 7}}`
	if got := RedactMessage(line, ConnConfig{}); got != line {
		t.Errorf("RedactMessage(%q) = %q, want it unchanged", line, got)
	}
	if got := RedactMessage("bad request key=Sec9pwKey", ConnConfig{}); strings.Contains(got, "Sec9pwKey") {
		t.Errorf("RedactMessage leaked a key= value: %q", got)
	}
}
