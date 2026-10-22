package clickhouse

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCheckReadOnlyAccepts(t *testing.T) {
	for _, q := range []string{
		"SELECT 1",
		"select * from t",
		"  \n-- c\nSELECT 1",
		"# c\nSELECT 1",
		"/* a /* nested */ b */ SELECT 1",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"(SELECT 1) UNION ALL (SELECT 2)",
		"((SELECT 1))",
		"SHOW TABLES",
		"show create table t",
		"DESCRIBE TABLE t",
		"desc t",
		"EXPLAIN SELECT 1",
		"explain pipeline select 1",
		"EXISTS TABLE t",
		"SELECT 1;",
		"SELECT 1 ; -- trailing\n",
		"SELECT 'a;b', `c;d`, \"e;f\"",
		"SELECT 'it\\'s; INSERT'",
		"SELECT $$x; INSERT$$",
		"SELECT 1 FORMAT JSON",
	} {
		if err := checkReadOnly(q); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want accepted", q, err)
		}
	}
}

func TestCheckReadOnlyRefuses(t *testing.T) {
	for _, q := range []string{
		"",
		"   ",
		"-- only a comment",
		"INSERT INTO t VALUES (1)",
		"insert into function file('x.csv') select 1",
		"INSERT INTO FUNCTION url('http://x', CSV, 'a String') SELECT 1",
		"INSERT INTO FUNCTION s3('http://x/b/k', CSV) SELECT 1",
		"INSERT INTO FUNCTION remote('h', db.t) SELECT 1",
		"ALTER TABLE t DELETE WHERE 1",
		"ALTER TABLE t UPDATE a = 1 WHERE 1",
		"DELETE FROM t WHERE 1",
		"UPDATE t SET a = 1 WHERE 1",
		"CREATE TABLE x (a Int8) ENGINE = Memory",
		"DROP TABLE t",
		"TRUNCATE TABLE t",
		"RENAME TABLE a TO b",
		"EXCHANGE TABLES a AND b",
		"ATTACH TABLE t",
		"DETACH TABLE t",
		"OPTIMIZE TABLE t FINAL",
		"SYSTEM FLUSH LOGS",
		"SYSTEM SHUTDOWN",
		"KILL QUERY WHERE 1",
		"KILL MUTATION WHERE 1",
		"SET readonly = 0",
		"SET max_threads = 1",
		"USE system",
		"GRANT ALL ON *.* TO u",
		"REVOKE ALL ON *.* FROM u",
		"CHECK TABLE t",
		"BACKUP TABLE t TO Disk('d', 'x')",
		"RESTORE TABLE t FROM Disk('d', 'x')",
		"UNDROP TABLE t",
		"MOVE USER u TO local",
		"BEGIN TRANSACTION",
		"WATCH lv",
		"SELECT 1; INSERT INTO t VALUES (1)",
		"SELECT 1; SELECT 2",
		"/* SELECT */ INSERT INTO t VALUES (1)",
		"/* a /* b */ SELECT */ INSERT INTO t VALUES (1)",
		"-- SELECT\nINSERT INTO t VALUES (1)",
		"SELECT 'unterminated",
		"SELECT `unterminated",
		"SELECT /* unterminated",
		"SELECT $$unterminated",
		"SELECT 1\x00",
		"(INSERT INTO t VALUES (1))",
		"(",
		"SELECT 1 INTO OUTFILE 'x.csv'",
		"select 1 into   outfile 'x'",
	} {
		err := checkReadOnly(q)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", q, err)
		}
	}
}

func TestCheckRawFilter(t *testing.T) {
	for _, raw := range []string{"a = 1", "a IN (1, 2) AND b = ';'", "x = 'it\\'s )'", "f(a) > 0 -- note"} {
		if err := checkRawFilter(raw); err != nil {
			t.Errorf("checkRawFilter(%q) = %v", raw, err)
		}
	}
	for _, raw := range []string{"1) OR (1", "1; DROP TABLE t", "a = 'x", "(a", "a /* x", "1) SETTINGS readonly = 0 --", "a\x00"} {
		if err := checkRawFilter(raw); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkRawFilter(%q) = %v, want ErrSyntax", raw, err)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{
		"t":        "`t`",
		"a`b":      "`a\\`b`",
		`a\b`:      "`a\\\\b`",
		"my db":    "`my db`",
		`x\` + "`": "`x\\\\\\``",
	} {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
	if !strings.HasPrefix(quoteIdent("a"), "`") {
		t.Fatal("identifiers are quoted with backticks")
	}
}

func TestCheckParams(t *testing.T) {
	ok := []struct {
		text   string
		params []driver.Value
	}{
		{"SELECT ? AS a", []driver.Value{driver.String("x")}},
		{"SELECT $$ no placeholder $$, ?", []driver.Value{driver.Int(1)}},
		{"SELECT '?', ?", []driver.Value{driver.String("it's $ fine")}},
		{"SELECT $$ ? $$", nil}, // no params: nothing is bound
	}
	for _, tc := range ok {
		if err := checkParams(tc.text, tc.params); err != nil {
			t.Errorf("checkParams(%q, %v) = %v", tc.text, tc.params, err)
		}
	}
	bad := []struct {
		text   string
		params []driver.Value
	}{
		{"SELECT $$ ? $$", []driver.Value{driver.String("$$ SETTINGS readonly=0 --")}},
		{"SELECT $$ ? $$", []driver.Value{driver.Int(1)}},
		{"SELECT $tag$ ? $tag$", []driver.Value{driver.Int(1)}},
		{"SELECT $$ $1 $$, $1", []driver.Value{driver.Int(1)}},
		{"SELECT ?", []driver.Value{driver.String("a $$ b")}},
		{"SELECT ?", []driver.Value{driver.JSON([]byte(`{"a":"$$"}`))}},
		{"SELECT ?", []driver.Value{driver.List(driver.String("ok"), driver.String("$$"))}},
		{"SELECT ?", []driver.Value{driver.Bytes([]byte("$$"))}},
	}
	for _, tc := range bad {
		if err := checkParams(tc.text, tc.params); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkParams(%q, %v) = %v, want ErrReadOnly", tc.text, tc.params, err)
		}
	}
}

func TestHasFormatClause(t *testing.T) {
	for text, want := range map[string]bool{
		"SELECT 1 FORMAT JSON":                          true,
		"SELECT 1 FORMAT JSON;":                         true,
		"SELECT 1 format `CSV`":                         true,
		"SELECT 1 FORMAT JSON SETTINGS max_threads = 1": true,
		"SELECT 1 SETTINGS max_threads = 1 FORMAT JSON": true,
		"SELECT format FROM t":                          false,
		"SELECT formatDateTime(now(), '%F')":            false,
		"SELECT 1":                                      false,
		"SELECT (SELECT 1 FORMAT JSON)":                 false, // a subquery cannot choose the output format
	} {
		if got := hasFormatClause(text); got != want {
			t.Errorf("hasFormatClause(%q) = %v, want %v", text, got, want)
		}
	}
}

// TestCheckASTCaptured runs checkAST on EXPLAIN AST outputs captured from
// ClickHouse 25.8 (testdata/explain_ast; the file name says the verdict).
// allow_* must pass; refuse_* must be ErrReadOnly naming the function;
// forged_* (quoted names holding newlines, which the dump prints raw) must
// be ErrReadOnly, whatever the reason.
func TestCheckASTCaptured(t *testing.T) {
	want := map[string]string{ // refuse_* file -> the function named
		"refuse_comma_after_on": "icebergs3cluster", "refuse_comma_after_using": "deltalakecluster",
		"refuse_as_array_join": "icebergs3cluster", "refuse_in_fn": "icebergs3cluster",
		"refuse_global_not_in": "remotesecure", "refuse_in_call": "url", "refuse_in_local_fn": "merge",
		"refuse_cte": "url", "refuse_subquery": "remotesecure", "refuse_describe": "url",
		"refuse_desc_file": "file", "refuse_explain": "url", "refuse_union": "file",
		"refuse_scalar_file": "file", "refuse_scalar_alias_url": "url", "refuse_insert_function": "url",
		"refuse_quoted": "url", "refuse_qualified_numbers": "system.numbers", "refuse_view": "view",
		"refuse_join_alias": "url", "refuse_merge": "merge", "refuse_paimon": "paimons3",
		"refuse_scalar_cluster": "somefuturecluster",
	}
	files, err := filepath.Glob(filepath.Join("testdata", "explain_ast", "*.txt"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no testdata: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".txt")
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		err = checkAST(string(raw))
		switch {
		case strings.HasPrefix(name, "allow_"):
			if err != nil {
				t.Errorf("%s: %v, want accepted", name, err)
			}
		case strings.HasPrefix(name, "refuse_"):
			seen[name] = true
			fn, ok := want[name]
			if !ok {
				t.Errorf("%s: no expected function", name)
			}
			if !errors.Is(err, driver.ErrReadOnly) || !strings.Contains(err.Error(), " "+fn+"()") {
				t.Errorf("%s: %v, want ErrReadOnly naming %s()", name, err, fn)
			}
		case strings.HasPrefix(name, "forged_"):
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%s: %v, want ErrReadOnly", name, err)
			}
		default:
			t.Errorf("%s: unknown verdict prefix", name)
		}
		// The dump itself never reaches the message.
		if err != nil {
			for _, leak := range []string{"Literal", "TableExpression", "ExpressionList", "http://x", "\n"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("%s: the error quotes the dump (%q): %v", name, leak, err)
				}
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("%s: testdata missing", name)
		}
	}
}

// TestCheckASTMalformed: output checkAST cannot read as one well-formed tree
// is refused (ErrReadOnly), never accepted.
func TestCheckASTMalformed(t *testing.T) {
	for name, dump := range map[string]string{
		"empty":            "",
		"blank":            "\n\n",
		"leading space":    " SelectWithUnionQuery",
		"two roots":        "ShowTables\nShowTables",
		"indent jump":      "SelectWithUnionQuery (children 1)\n  ExpressionList",
		"too few children": "SelectWithUnionQuery (children 2)\n ExpressionList",
		"too many":         "SelectWithUnionQuery (children 1)\n ExpressionList\n ExpressionList",
		"undeclared":       "SelectWithUnionQuery\n ExpressionList",
		"not a node":       "SelectWithUnionQuery (children 1)\n 123",
		"bad count":        "SelectWithUnionQuery (children x)\n ExpressionList",
		"odd name":         "SelectWithUnionQuery (children 1)\n Function a-b (children 1)\n  ExpressionList",
		"no name":          "SelectWithUnionQuery (children 1)\n Function",
		"odd table child":  "TablesInSelectQueryElement (children 1)\n TableExpression (children 1)\n  TableFunction url",
		"control byte":     "SelectWithUnionQuery (children 1)\n Identifier a\r",
	} {
		if err := checkAST(dump); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s: %v, want ErrReadOnly", name, err)
		}
	}
}

// TestCheckASTText is the lexical pre-filter run before EXPLAIN AST: a
// quoted name holding a control character or a backslash (whose escapes
// can make one) could forge lines in the dump, so it is refused.
func TestCheckASTText(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t",
		"SELECT `a b`, \"c\" FROM `t`",
		"SELECT 'a\nb', $$x\ny$$ FROM t -- c\n",
		"SELECT 'it\\'s'",
	} {
		if err := checkASTText(q); err != nil {
			t.Errorf("checkASTText(%q) = %v, want accepted", q, err)
		}
	}
	for _, q := range []string{
		"SELECT `a\nb` FROM t",
		"SELECT \"a\rb\" FROM t",
		"SELECT `a\\nb` FROM t",
		"SELECT 1 AS \"x\\x0A\"",
		"SELECT `a\tb`",
		"SELECT 'unterminated",
		"SELECT `unterminated",
	} {
		if err := checkASTText(q); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkASTText(%q) = %v, want ErrReadOnly", q, err)
		}
	}
}
