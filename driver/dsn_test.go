package driver

import (
	"strings"
	"testing"
)

func TestScheme(t *testing.T) {
	tests := []struct{ in, want string }{
		{"postgres://u@h/db", "postgres"},
		{"postgresql://u@h/db", "postgresql"},
		{"sqlite:./a.db", "sqlite"},
		{"file:a.db?mode=ro", "file"},
		{"./a.db", ""},
		{"", ""},
		{"host=h user=app", ""},
		{"C:\\data\\a.db", ""},
		{"POSTGRES://h/db", "postgres"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := Scheme(tt.in); got != tt.want {
				t.Errorf("Scheme(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactDSN(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"url", "postgres://app:hunter2@h:5432/db?sslmode=disable", "postgres://app:xxxxx@h:5432/db?sslmode=disable"},
		{"url no password", "postgres://app@h/db", "postgres://app@h/db"},
		{"url no userinfo", "postgres://h/db", "postgres://h/db"},
		{"url empty password", "postgres://app:@h/db", "postgres://app:xxxxx@h/db"},
		{"url at in password", "postgres://app:p@ss@h/db", "postgres://app:xxxxx@h/db"},
		{"url query password", "postgres://h/db?password=hunter2", "postgres://h/db?password=xxxxx"},
		{"url query password mid", "postgres://h/db?a=1&password=hunter2&b=2", "postgres://h/db?a=1&password=xxxxx&b=2"},
		{"url both", "postgres://app:hunter2@h/db?password=hunter2#f", "postgres://app:xxxxx@h/db?password=xxxxx#f"},
		{"keyvalue", "host=h user=app password=hunter2 dbname=db", "host=h user=app password=xxxxx dbname=db"},
		{"keyvalue quoted", "host=h password='hun ter2' dbname=db", "host=h password=xxxxx dbname=db"},
		{"keyvalue quoted escape", `password='it\'s a secret' host=h`, "password=xxxxx host=h"},
		{"keyvalue upper", "PASSWORD=hunter2", "PASSWORD=xxxxx"},
		{"keyvalue last", "host=h password=hunter2", "host=h password=xxxxx"},
		{"keyvalue no password", "host=h user=app dbname=db", "host=h user=app dbname=db"},
		{"keyvalue similar key", "host=h passfile=/x/pw", "host=h passfile=/x/pw"},
		{"kv pwd", "host=h pwd=hunter2 dbname=db", "host=h pwd=xxxxx dbname=db"},
		{"kv passwd upper", "host=h PASSWD=hunter2", "host=h PASSWD=xxxxx"},
		{"kv pass", "host=h pass='hun ter2'", "host=h pass=xxxxx"},
		{"kv passfile untouched", "host=h passfile=/x", "host=h passfile=/x"},
		{"url pwd param", "postgres://h/db?pwd=hunter2&a=1", "postgres://h/db?pwd=xxxxx&a=1"},
		{"sqlite", "sqlite:./a.db", "sqlite:./a.db"},
		{"file", "file:a.db?mode=ro", "file:a.db?mode=ro"},
		{"empty", "", ""},
		{"at in query then sslpassword", "postgres://u:p@h/db?x=a@b&sslpassword=zzz", "postgres://u:xxxxx@b&sslpassword=xxxxx"},
		{"at in query then password", "postgres://h:5432/db?email=a@b&password=zzz", "postgres://h:xxxxx@b&password=xxxxx"},
		{"accepted over-redaction", "postgres://h:5432/db?application_name=svc@prod", "postgres://h:xxxxx@prod"},
		{"spaces around eq", "host=h password = hunter2 dbname=db", "host=h password = xxxxx dbname=db"},
		{"space after eq", "password= hunter2 host=h", "password= xxxxx host=h"},
		{"kv with :// elsewhere", "host=h password=hunter2 sslrootcert=file://x", "host=h password=xxxxx sslrootcert=file://x"},
		{"kv password with ://", "host=h password=a://b dbname=db", "host=h password=xxxxx dbname=db"},
		{"url hash in password", "postgres://u:p#ss@h/db", "postgres://u:xxxxx@h/db"},
		{"url question in password", "postgres://u:p?ss@h/db", "postgres://u:xxxxx@h/db"},
		{"url slash in password", "postgres://u:p/ss@h/db", "postgres://u:xxxxx@h/db"},
		{"url sslpassword", "postgres://h/db?sslpassword=hunter2&a=1", "postgres://h/db?sslpassword=xxxxx&a=1"},
		{"kv sslpassword", "host=h sslpassword=hunter2", "host=h sslpassword=xxxxx"},
		{"kv escaped space", `password=hun\ ter2 host=h`, "password=xxxxx host=h"},
		{"mysql", "user:hunter2@tcp(h:3306)/db?parseTime=true", "user:xxxxx@tcp(h:3306)/db?parseTime=true"},
		{"mysql no password", "user@tcp(h)/db", "user@tcp(h)/db"},
		{"kv with at sign", "host=h user=a@b password=hunter2", "host=h user=a@b password=xxxxx"},
		{"ado", "Server=h;User Id=u;Password=hunter2;Database=d", "Server=h;User Id=u;Password=xxxxx;Database=d"},
		{"ado pwd lower trailing", "server=h;uid=u;pwd=hunter2;", "server=h;uid=u;pwd=xxxxx;"},
		{"ado spaced", "Server=h; PASSWORD = hun ter2 ;Database=d", "Server=h; PASSWORD =xxxxx;Database=d"},
		{"ado quoted with semicolon", `Server=h;Password="a;b""c";Database=d`, "Server=h;Password=xxxxx;Database=d"},
		{"ado tcp server", "Server=tcp:h,1433;Password=hunter2", "Server=tcp:h,1433;Password=xxxxx"},
		// Also a libpq password "hunter2;Server=h": both readings are masked.
		{"ado first ambiguous", "Password=hunter2;Server=h", "Password=xxxxx"},
		{"libpq password with semicolon", "password=a;b host=h", "password=xxxxx host=h"},
		{"ado no password", "Server=h;Database=d", "Server=h;Database=d"},
		{"sqlite auth pass", "file:./x.db?_auth_user=u&_auth_pass=hunter2", "file:./x.db?_auth_user=u&_auth_pass=xxxxx"},
		{"sqlite auth pass url", "sqlite:///x.db?_auth_pass=hunter2&mode=ro", "sqlite:///x.db?_auth_pass=xxxxx&mode=ro"},
		{"sqlite auth pass first", "sqlite:x.db?_auth_pass=hunter2", "sqlite:x.db?_auth_pass=xxxxx"},
		{"odbc brace", "Server=tcp:h,1433;Database=d;Pwd={Abc;Tail99};", "Server=tcp:h,1433;Database=d;Pwd=xxxxx;"},
		{"odbc brace escaped", "Server=h;Password={a}};b}}c};Database=d", "Server=h;Password=xxxxx;Database=d"},
		{"odbc brace unterminated", "Server=h;Password={abc;def", "Server=h;Password=xxxxx"},
		{"odbc prefix brace", "odbc:server=h;uid=sa;pwd={a;Sup3r}}S3cret;b};database=d", "odbc:server=h;uid=sa;pwd=xxxxx;database=d"},
		{"odbc prefix brace upper key", "odbc:server=h;PASSWORD={x;y};database=d", "odbc:server=h;PASSWORD=xxxxx;database=d"},
		{"odbc prefix at in uid", "odbc:server=h;uid=me@corp.com;pwd=Secret1xyz;database=d", "odbc:server=h;uid=me@corp.com;pwd=xxxxx;database=d"},
		{"odbc prefix at in uid braced", "odbc:server=h;uid=me@corp.com;pwd={Sec;ret1xyz};database=d", "odbc:server=h;uid=me@corp.com;pwd=xxxxx;database=d"},
		{"odbc prefix at in password", "odbc:server=h;uid=sa;pwd=Sec@ret1xyz;database=d", "odbc:server=h;uid=sa;pwd=xxxxx;database=d"},
		{"opaque kv before at", "custom:host=h;user=me@corp;password=Sec@ret9;x=1", "custom:xxxxx@corp;password=xxxxx;x=1"}, // lenient userinfo over-redacts
		{"file query keeps later params", "file:test.db?password=S3cretPw&k=v", "file:test.db?password=xxxxx&k=v"},
		{"opaque kv password with question mark", "custom:host=h;password=ab?Sec9;x=1", "custom:host=h;password=xxxxx;x=1"},
		{"mysql password with equals", "root:Se=c9pw@tcp(h:3306)/d", "root:xxxxx@tcp(h:3306)/d"},
		{"mysql password with semicolon", "admin:Se;c9pw@tcp(h)/d?parseTime=true", "admin:xxxxx@tcp(h)/d?parseTime=true"},
		{"mysql password with equals and question", "root:a=b?Sec9pw@tcp(h)/d", "root:xxxxx@tcp(h)/d"},
		{"file path with at and auth pass", "file:/tmp/a@b.db?_auth_pass=Auth9pw", "file:/tmp/a@b.db?_auth_pass=xxxxx"},
		{"url query password with semicolon", "sqlserver://u@h?database=d&password=Se;c9pw", "sqlserver://u@h?database=d&password=xxxxx"},
		{"url query password with semicolon then param", "sqlserver://u@h?password=Se;c9pw&database=d", "sqlserver://u@h?password=xxxxx&database=d"},
		{"file path with equals", "file:/tmp/a=b.db?password=S3cretPw&k=v", "file:/tmp/a=b.db?password=xxxxx&k=v"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactDSN(tt.in); got != tt.want {
				t.Errorf("RedactDSN(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactDSNNoLeak(t *testing.T) {
	for _, in := range []string{
		"postgres://u:p@h/db?x=a@b&sslpassword=zzz",
		"postgres://h:5432/db?email=a@b&password=zzz",
		"postgres://h/db?a=1;password=zzz#f",
	} {
		if got := RedactDSN(in); strings.Contains(got, "zzz") {
			t.Errorf("RedactDSN(%q) leaked: %q", in, got)
		}
	}
}

func TestRedactMessage(t *testing.T) {
	pw := NewSecret("s3cret")
	odd := NewSecret("p@ss w/rd")
	tests := []struct {
		name string
		msg  string
		cfg  ConnConfig
		want string
	}{
		{"embedded url userinfo", "error at postgres://u:s3cret@h/db", ConnConfig{},
			"error at postgres://u:xxxxx@h/db"},
		{"quoted url query param", `cannot parse "postgres://h/db?sslmode=disable&password=s3cret"`, ConnConfig{},
			`cannot parse "postgres://h/db?sslmode=disable&password=xxxxx"`},
		{"keyvalue", "password=s3cret host=h", ConnConfig{}, "password=xxxxx host=h"},
		{"keyvalue mid sentence", "bad dsn: host=h sslpassword='s3 cret' x", ConnConfig{},
			"bad dsn: host=h sslpassword=xxxxx x"},
		{"keyvalue spaced", "password = s3cret", ConnConfig{}, "password = xxxxx"},
		{"literal password", "auth failed for s3cret", ConnConfig{Password: pw},
			"auth failed for xxxxx"},
		{"escaped password", "dial p%40ss+w%2Frd and p%40ss%20w%2Frd", ConnConfig{Password: odd},
			"dial xxxxx and xxxxx"},
		{"literal dsn", "open host=h user=u pass=s3cret!", ConnConfig{DSN: "host=h user=u pass=s3cret!", Password: pw},
			"open host=h user=u pass=xxxxx"},
		{"literal url dsn", "open mysql-ish u:s3cret@tcp(h)/db failed", ConnConfig{DSN: "u:s3cret@tcp(h)/db"},
			"open mysql-ish u:xxxxx@tcp(h)/db failed"},
		{"bare dsn password", "auth failed: s3cret rejected", ConnConfig{DSN: "postgres://u:s3cret@h/db"},
			"auth failed: xxxxx rejected"},
		{"bare escaped dsn password", "auth failed: p@ss w/rd", ConnConfig{DSN: "postgres://u:p%40ss%20w%2Frd@h/db"},
			"auth failed: xxxxx"},
		{"bare keyvalue dsn password", "bad 's3cret'", ConnConfig{DSN: "host=h password='s3cret'"},
			"bad 'xxxxx'"},
		{"bare query dsn password", "rejected s3cret", ConnConfig{DSN: "postgres://h/db?password=s3cret&x=1"},
			"rejected xxxxx"},
		{"two urls", "a postgres://u:one@h b postgres://v:two@h", ConnConfig{},
			"a postgres://u:xxxxx@h b postgres://v:xxxxx@h"},
		{"no secrets", "relation \"x\" does not exist", ConnConfig{DSN: "postgres://h/db"},
			"relation \"x\" does not exist"},
		{"url without password", "connect postgres://u@h/db", ConnConfig{}, "connect postgres://u@h/db"},
		{"pwd key", "bad pwd=s3cret x", ConnConfig{}, "bad pwd=xxxxx x"},
		{"passwd key", "bad PASSWD='s3 cret' x", ConnConfig{}, "bad PASSWD=xxxxx x"},
		{"empty", "", ConnConfig{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactMessage(tt.msg, tt.cfg)
			if got != tt.want {
				t.Errorf("RedactMessage(%q) = %q, want %q", tt.msg, got, tt.want)
			}
			for _, s := range []string{"s3cret", "one@", "two@", "p@ss"} {
				if strings.Contains(got, s) {
					t.Errorf("RedactMessage(%q) = %q leaks %q", tt.msg, got, s)
				}
			}
		})
	}
}

func TestHasPassword(t *testing.T) {
	tests := []struct {
		dsn  string
		want bool
	}{
		{"postgres://u:hunter2@h/db", true},
		{"postgres://u:xxxxx@h/db", true}, // a redacted DSN saved back
		{"postgres://u:@h/db", true},
		{"host=h password=xxxxx", true},
		{"host=h pwd=hunter2", true},
		{"postgres://u@h/db", false},
		{"postgres://h/db?sslmode=disable", false},
		{"host=h user=u dbname=d", false},
		{"sqlite:./a.db", false},
		{"", false},
		{"Server=h;User Id=u;Password=hunter2;", true},
		{"Server=h;Pwd=xxxxx", true},
		{"Server=h;Database=d", false},
		{"file:./x.db?_auth_pass=hunter2", true},
		{"file:./x.db?_auth_user=u", false},
		{"Server=h;Pwd={Abc;Tail99};", true},
		{"root:Se=c9pw@tcp(h:3306)/d", true},
		{"admin:Se;c9pw@tcp(h)/d?parseTime=true", true},
		{"root:a=b?Sec9pw@tcp(h)/d", true},
		{"file:/tmp/a@b.db?_auth_pass=Auth9pw", true},
		{"odbc:server=h;uid=me@corp.com;database=d", false},
	}
	for _, tt := range tests {
		if got := HasPassword(tt.dsn); got != tt.want {
			t.Errorf("HasPassword(%q) = %v, want %v", tt.dsn, got, tt.want)
		}
	}
}

// TestRedactMessageADOAndAuthPass covers the log path: a DSN in the ADO
// form or with _auth_pass, quoted in an error, never shows its password.
func TestRedactMessageADOAndAuthPass(t *testing.T) {
	for dsn, pw := range map[string]string{
		"Server=h;User Id=u;Password=Hunter2-ado;Database=d":     "Hunter2-ado",
		"file:./x.db?_auth_user=u&_auth_pass=Hunter2-ado":        "Hunter2-ado",
		"Server=tcp:h,1433;Database=d;Pwd={Hunter2;ado-Tail99};": "Hunter2;ado-Tail99",
		"Server=h;Pwd={Hunter2-ado}};Tail77};Database=d":         "Hunter2-ado};Tail77",
	} {
		pieces := strings.FieldsFunc(pw, func(r rune) bool { return r == ';' })
		for _, msg := range []string{"open " + dsn + ": refused", "dial failed: " + pw + " rejected"} {
			got := RedactMessage(msg, ConnConfig{DSN: dsn})
			for _, p := range append(pieces, pw) {
				if strings.Contains(got, p) {
					t.Errorf("RedactMessage(%q) = %q leaks %q", msg, got, p)
				}
			}
		}
		got := RedactMessage("bad dsn "+dsn, ConnConfig{})
		for _, p := range append(pieces, pw) {
			if strings.Contains(got, p) {
				t.Errorf("without cfg: %q leaks %q", got, p)
			}
		}
	}
}

func TestRedactMessageKeepsColonAfterValue(t *testing.T) {
	got := RedactMessage("open sqlite:x.db?_auth_pass=Hunter2-ado: no such file", ConnConfig{})
	if want := "open sqlite:x.db?_auth_pass=xxxxx: no such file"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRedactMessageMasksPasswordEndingInColon: in free text, with no cfg to
// name the password, a value ending in ':' is masked whole; only a URL query
// parameter (where a real ':' is normally percent-encoded) gives its trailing
// colon back to the message.
func TestRedactMessageMasksPasswordEndingInColon(t *testing.T) {
	cases := []struct{ in, want string }{
		{"auth failed for password=Pw4-Tail: try again", "auth failed for password=xxxxx try again"},
		{"password=Pw4-Tail:: x", "password=xxxxx x"},
		{"host=h password=Pw4-Tail: dbname=d", "host=h password=xxxxx dbname=d"},
		{"password: Pw4-Tail: retry", "password: xxxxx retry"},
		{`"password": Pw4-Tail: retry`, `"password": xxxxx retry`},
		{"ab: c", "ab: c"},
		// URL query parameters keep the message's colon.
		{"open sqlite:x.db?_auth_pass=Hunter2-ado: no such file", "open sqlite:x.db?_auth_pass=xxxxx: no such file"},
		{"open file:x.db?mode=ro&_auth_pass=Hunter2-ado: no such file", "open file:x.db?mode=ro&_auth_pass=xxxxx: no such file"},
	}
	for _, tc := range cases {
		if got := RedactMessage(tc.in, ConnConfig{}); got != tc.want {
			t.Errorf("RedactMessage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDSNPasswordEndingInColon: a libpq password ending in ':' is the whole
// value, so a message quoting it is masked whole through cfg.DSN too.
func TestDSNPasswordEndingInColon(t *testing.T) {
	cfg := ConnConfig{DSN: "host=h password=Pw4-Tail: dbname=d"}
	got := RedactMessage("server said Pw4-Tail: is wrong", cfg)
	if want := "server said xxxxx is wrong"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if s, _ := StripPassword(cfg.DSN); strings.Contains(s, "Pw4") || strings.Contains(s, "Tail") || strings.Contains(s, ":") {
		t.Errorf("StripPassword left %q", s)
	}
}

func TestStripPassword(t *testing.T) {
	cases := []struct{ in, want string }{
		{"postgres://alice:S3cret@h:5432/db?sslmode=require", "postgres://alice@h:5432/db?sslmode=require"},
		{"postgres://:S3cret@h/db", "postgres://h/db"},
		{"postgres://alice@h/db?password=S3cret&sslmode=disable", "postgres://alice@h/db?sslmode=disable"},
		{"postgres://alice@h/db?sslmode=disable&password=S3cret", "postgres://alice@h/db?sslmode=disable"},
		{"postgres://u@h/db?options=a;b&password=S3cret", "postgres://u@h/db?options=a;b"},
		// Go's URL parser does not split on ';': b=2 may be part of the
		// password, so the DSN is refused rather than half stripped.
		{"postgres://u@h/db?a=1;password=S3cret;b=2#frag", ""},
		{"postgres://myuser@myserver:S3cret@myserver.example.com/db", "postgres://myuser@myserver@myserver.example.com/db"},
		{"root:S3cret@tcp(h:3306)/d?loc=x@y", "root@tcp(h:3306)/d?loc=x@y"},
		{"root:S3cret@cloudsql-mysql(p:r:i)/d", "root@cloudsql-mysql(p:r:i)/d"},
		{"me@corp:S3cret@tcp(h)/d", "me@corp@tcp(h)/d"},
		{"sqlserver://u@h?password=S3cret;tail&database=d", "sqlserver://u@h?database=d"},
		{"host=h user=alice password=S3cret dbname=db", "host=h user=alice dbname=db"},
		{"password = 'S3 cret' host=h", "host=h"},
		{"host=h options='-c a=b;c' password=S3cret", "host=h options='-c a=b;c'"},
		{"alice:S3cret@tcp(h:3306)/db?parseTime=true", "alice@tcp(h:3306)/db?parseTime=true"},
		{"mysql://alice:S3cret@h:3306/db", "mysql://alice@h:3306/db"},
		{"Server=h;User Id=u;Password=S3cret;Database=d", "Server=h;User Id=u;Database=d"},
		{"server=h;pwd=S3cret;", "server=h;"},
		{"Server=h;Password=S3cret", "Server=h"},
		{"Server=h; PASSWORD = S3 cret ;Database=d", "Server=h;Database=d"},
		{`Server=h;PWD="a;b""c";Database=d`, "Server=h;Database=d"},
		{"Server=tcp:h,1433;Database=d;Pwd={Abc;Tail99};", "Server=tcp:h,1433;Database=d;"},
		{"Server=h;Password={a}};b}}c};Database=d", "Server=h;Database=d"},
		{"file:./x.db?_auth_user=u&_auth_pass=S3cret&mode=ro", "file:./x.db?_auth_user=u&mode=ro"},
		{"sqlite:x.db?_auth_pass=S3cret", "sqlite:x.db"},
		{"sqlite:///x.db?_auth_pass=S3cret", "sqlite:///x.db"},
		{"sqlite:./x.db", "sqlite:./x.db"},
		// Ambiguous (also a libpq password "S3cret;Server=h"): nothing safe
		// is left.
		{"Password=S3cret;Server=h", ""},
	}
	for _, tc := range cases {
		got, had := StripPassword(tc.in)
		if got != tc.want || had != (tc.in != tc.want) {
			t.Errorf("%q: got %q had=%v, want %q", tc.in, got, had, tc.want)
		}
		for _, leak := range []string{"S3cret", "S3 cret", "Tail99", "b}}c"} {
			if strings.Contains(got, leak) {
				t.Errorf("%q: result %q leaks %q", tc.in, got, leak)
			}
		}
	}
}

func TestRedactMessageStructuredForms(t *testing.T) {
	tests := []struct{ name, msg, want string }{
		{"yaml", "bad config password: s3cret host: h", "bad config password: xxxxx host: h"},
		{"yaml quoted", `db_password: 's3 cret' x`, "db_password: xxxxx x"},
		{"json", `{"host":"h","password":"s3cret"}`, `{"host":"h","password":xxxxx}`},
		{"json spaced", `{"Password" : "s3 cret", "x": 1}`, `{"Password" : xxxxx, "x": 1}`},
		{"json bare value", `{"pwd":12345,"x":1}`, `{"pwd":xxxxx"x":1}`}, // masked to the next quote or space
		{"yaml comma", "password: pa,ss next", "password: xxxxx next"},
		{"json bracket", `{"password":pa]ss}`, `{"password":xxxxx`},
		{"empty value", "enter password: ", "enter password: "},
		{"empty value eol", "enter password:", "enter password:"},
		{"non password key", `{"user":"s3cret"} user: s3cret`, `{"user":"s3cret"} user: s3cret`},
		{"url scheme untouched", "dial password://h", "dial password://h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactMessage(tt.msg, ConnConfig{}); got != tt.want {
				t.Errorf("RedactMessage(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

func TestRedactMessageShortPasswords(t *testing.T) {
	tests := []struct {
		name, msg string
		cfg       ConnConfig
		want      string
	}{
		{"short cfg password not bare", "a1 table abc", ConnConfig{Password: NewSecret("a")}, "a1 table abc"},
		{"short dsn password not bare", "relation abc missing", ConnConfig{DSN: "postgres://u:a@h/db"},
			"relation abc missing"},
		{"short dsn password in userinfo", "open postgres://u:a@h/db failed", ConnConfig{DSN: "postgres://u:a@h/db"},
			"open postgres://u:xxxxx@h/db failed"},
		{"short password in key", "bad password=a rest", ConnConfig{Password: NewSecret("a")},
			"bad password=xxxxx rest"},
		{"four bytes bare", "auth failed: abcd", ConnConfig{Password: NewSecret("abcd")}, "auth failed: xxxxx"},
		{"four bytes dsn bare", "rejected abcd", ConnConfig{DSN: "postgres://u:abcd@h/db"}, "rejected xxxxx"},
		{"three bytes not bare", "rejected abc", ConnConfig{DSN: "postgres://u:abc@h/db"}, "rejected abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactMessage(tt.msg, tt.cfg); got != tt.want {
				t.Errorf("RedactMessage(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}
