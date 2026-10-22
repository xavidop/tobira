package cosmos

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestLex(t *testing.T) {
	toks, err := lex(`SELECT c["a b"], 'it\'s' AS s, -1.5e3 FROM people c -- trailing
WHERE c.n >= @p1 AND c.x != "q\"x"`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range toks {
		got = append(got, tk.text)
	}
	want := []string{"SELECT", "c", "[", `"a b"`, "]", ",", `'it\'s'`, "AS", "s", ",", "-", "1.5e3", "FROM", "people", "c",
		"WHERE", "c", ".", "n", ">=", "@p1", "AND", "c", ".", "x", "!=", `"q\"x"`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lex tokens:\n got %q\nwant %q", got, want)
	}
	for _, bad := range []string{`SELECT 'open`, `SELECT "open`, `SELECT 'a\`, "SELECT \x00", `SELECT #`} {
		if _, err := lex(bad); err == nil {
			t.Errorf("lex(%q): want an error", bad)
		}
	}
}

func TestParseRequestPlainSQL(t *testing.T) {
	for _, tc := range []struct {
		text, container string
	}{
		{"SELECT * FROM people p", "people"},
		{"select * from people", "people"},
		{"SELECT VALUE COUNT(1) FROM people", "people"},
		{"SELECT * FROM people AS p WHERE p.n > 1 ORDER BY p.n", "people"},
		{"SELECT p.id FROM people p JOIN t IN p.tags WHERE EXISTS(SELECT VALUE x FROM x IN p.tags)", "people"},
		{"-- a comment\nSELECT TOP 5 * FROM orders o", "orders"},
		{"SELECT * FROM people p WHERE p.name = 'FROM other'", "people"},
		{"SELECT (SELECT VALUE 1 FROM y IN c.l) AS n FROM tobira_people c", "tobira_people"},
	} {
		req, err := parseRequest(tc.text, nil, true)
		if err != nil {
			t.Errorf("parseRequest(%q): %v", tc.text, err)
			continue
		}
		if req.container != tc.container || req.sql != tc.text || req.partitionKey != nil {
			t.Errorf("parseRequest(%q) = %+v, want container %q, the text unchanged, no partition key", tc.text, req, tc.container)
		}
	}
	for _, text := range []string{
		"SELECT 1",
		"SELECT * FROM root r",
		"SELECT * FROM ROOT",
		"SELECT * FROM (SELECT * FROM c) x",
		"SELECT * FROM 'people'",
	} {
		if _, err := parseRequest(text, nil, false); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseRequest(%q): want ErrSyntax (name the container), got %v", text, err)
		}
	}
}

func TestParseRequestParams(t *testing.T) {
	req, err := parseRequest("SELECT * FROM people p WHERE p.id = @p1 AND p.age > @p2", []driver.Value{driver.String("7"), driver.Int(30)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.params) != 2 || req.params[0].Name != "@p1" || req.params[1].Name != "@p2" {
		t.Fatalf("params = %+v, want @p1 and @p2", req.params)
	}
	if got := string(req.params[1].Value.(json.RawMessage)); got != "30" {
		t.Errorf("@p2 = %s, want 30", got)
	}
	if _, err := parseRequest("SELECT * FROM p", []driver.Value{driver.Unknown("x", nil, "x")}, false); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("an Unknown parameter: want ErrUnsupported, got %v", err)
	}
}

func TestParseRequestEnvelope(t *testing.T) {
	req, err := parseRequest(`{"container":"orders","query":"SELECT * FROM c WHERE c.total > @min","partitionKey":"acme","parameters":{"@min":10}}`, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if req.container != "orders" || req.sql != "SELECT * FROM c WHERE c.total > @min" {
		t.Errorf("envelope request = %+v", req)
	}
	if !reflect.DeepEqual(req.partitionKey, []any{"acme"}) {
		t.Errorf("partitionKey = %#v, want [acme]", req.partitionKey)
	}
	if len(req.params) != 1 || req.params[0].Name != "@min" || string(req.params[0].Value.(json.RawMessage)) != "10" {
		t.Errorf("params = %+v", req.params)
	}
	req, err = parseRequest(`{"container":"t","query":"SELECT * FROM c","partitionKey":["acme",2,true,null]}`, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.partitionKey, []any{"acme", 2.0, true, nil}) {
		t.Errorf("hierarchical partitionKey = %#v", req.partitionKey)
	}
	for _, text := range []string{
		`{"container":"t"}`,
		`{"query":"SELECT * FROM c"}`,
		`{"container":"","query":"SELECT * FROM c"}`,
		`{"container":"t","query":"SELECT * FROM c","partitionKey":{"a":1}}`,
		`{"container":"t","query":"SELECT * FROM c","partitionKey":[]}`,
		`{"container":"t","query":"SELECT * FROM c","parameters":{"min":1}}`,
		`{"container":"t","query":"SELECT * FROM c","container":"u"}`,
		`{"container":"t","query":"SELECT * FROM c"} trailing`,
		`{"container":"t","query":7}`,
		`{"container":"t","query":"SELECT * FROM c","parameters":{"@p1":1}}`,
	} {
		_, err := parseRequest(text, []driver.Value{driver.Int(1)}, false)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseRequest(%s): want ErrSyntax, got %v", text, err)
		}
	}
}

func TestReadOnlyGuard(t *testing.T) {
	refused := []string{
		"",
		"   ",
		"-- only a comment",
		"DELETE FROM c WHERE c.id = '1'",
		"delete from people",
		"UPDATE people SET name = 'x'",
		"INSERT INTO people VALUES (1)",
		"UPSERT people {\"id\":\"1\"}",
		"REPLACE people",
		"EXEC sp_doit('a')",
		"EXECUTE sp_doit",
		"CALL sp_doit()",
		"CREATE TRIGGER t",
		"DROP CONTAINER people",
		"SELECT udf.tax(c.total) FROM people c",
		"SELECT * FROM people c WHERE UDF.isBig(c)",
		"SELECT * FROM people c WHERE c.n > 1 AND udf . f(c)",
		"SELECT [udf.f(c)] FROM people c",
		"SELECT VALUE [udf.f(c)][0] FROM people c",
		"SELECT * FROM people c WHERE ARRAY_CONTAINS([udf.f(c)], 1)",
		"SELECT * FROM people c WHERE c[udf.f(c)] = 1",
		`SELECT udf["f"](c) FROM people c`,
		"SELECT (udf).f(c) FROM people c",
		"SELECT 1 .udf.f(c) FROM people c",
		"SELECT udf FROM people udf",
		"SELECT * FROM people c; DELETE FROM people",
		"SELECT * FROM people c;",
		"SELECT 'unterminated FROM people",
		"(SELECT * FROM people)",
		"foo",
		`{"container":"people","query":"DELETE FROM c"}`,
		`{"container":"people","query":"SELECT udf.f(c) FROM c"}`,
		`{"container":"people","query":"SELECT * FROM c","storedProcedure":"sp"}`,
		`{"sproc":"bulkDelete","container":"people"}`,
		`{"container":"people","query":"SELECT * FROM c","preTrigger":"t"}`,
		`[1]`,
		`{"container":"people"`,
	}
	for _, text := range refused {
		if _, err := parseRequest(text, nil, true); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only parseRequest(%q): want ErrReadOnly, got %v", text, err)
		}
	}
	allowed := []string{
		"SELECT * FROM people c WHERE c.name = 'udf.x(1)'",
		"SELECT c.udf.x FROM people c",
		`SELECT c["a"].udf FROM people c`,
		"SELECT c[\"udf\"] FROM people c",
		"SELECT * FROM people c WHERE c.note = 'DELETE FROM c'",
		"SELECT VALUE c FROM people c",
		"SELECT * FROM people c -- ; DELETE",
	}
	for _, text := range allowed {
		if _, err := parseRequest(text, nil, true); err != nil {
			t.Errorf("read-only parseRequest(%q): want it allowed, got %v", text, err)
		}
	}
}

func TestWritableRefusals(t *testing.T) {
	for _, tc := range []struct {
		text string
		kind error
	}{
		{"DELETE FROM c WHERE c.id = '1'", driver.ErrUnsupported},
		{"update c set x = 1", driver.ErrUnsupported},
		{"EXEC sp_x", driver.ErrUnsupported},
		{"foo bar", driver.ErrSyntax},
		{"SELECT * FROM people c; SELECT 1", driver.ErrSyntax},
		{"", driver.ErrSyntax},
	} {
		if _, err := parseRequest(tc.text, nil, false); !errors.Is(err, tc.kind) {
			t.Errorf("writable parseRequest(%q): want %v, got %v", tc.text, tc.kind, err)
		}
	}
	// A UDF is read-only JavaScript: a writable query may call one.
	if _, err := parseRequest("SELECT udf.tax(c.total) FROM people c", nil, false); err != nil {
		t.Errorf("writable UDF call: %v", err)
	}
}

func TestCheckRawFilter(t *testing.T) {
	for _, raw := range []string{
		"c.age > 30",
		"STARTSWITH(c.name, 'A') OR c.active = true",
		"EXISTS(SELECT VALUE t FROM t IN c.tags WHERE t = 'beta')",
		"c.note = ')'",
	} {
		for _, ro := range []bool{false, true} {
			if err := checkRawFilter(raw, ro); err != nil {
				t.Errorf("checkRawFilter(%q, ro=%v): %v", raw, ro, err)
			}
		}
	}
	for _, raw := range []string{
		"true) OR (true",
		"c.a = 1)",
		"(c.a = 1",
		"c.a = 1; DELETE FROM c",
		"c.a = 'open",
	} {
		if err := checkRawFilter(raw, false); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("writable checkRawFilter(%q): want ErrSyntax, got %v", raw, err)
		}
		if err := checkRawFilter(raw, true); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only checkRawFilter(%q): want ErrReadOnly, got %v", raw, err)
		}
	}
	for _, raw := range []string{"[udf.f(c)] = [1]", "ARRAY_CONTAINS([udf.f(c)], 1)", `udf["f"](c) = 1`, "(udf).f(c) = 1", "1 .udf.f(c) = 1"} {
		if err := checkRawFilter(raw, true); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only raw filter %q: want ErrReadOnly, got %v", raw, err)
		}
	}
	// A comment would swallow the closing parenthesis the filter is wrapped in.
	if err := checkRawFilter("c.a = 1 -- note", false); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("writable raw filter with a comment: want ErrSyntax, got %v", err)
	}
	if err := checkRawFilter("c.a = 1 -- note", true); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only raw filter with a comment: want ErrReadOnly, got %v", err)
	}
	if err := checkRawFilter("c.a = '-- not a comment'", true); err != nil {
		t.Errorf("a raw filter with -- inside a string: %v", err)
	}
	if err := checkRawFilter("udf.f(c) = 1", true); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only raw filter calling a UDF: want ErrReadOnly, got %v", err)
	}
	if err := checkRawFilter("udf.f(c) = 1", false); err != nil {
		t.Errorf("writable raw filter calling a UDF: %v", err)
	}
}
