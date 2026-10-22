package cassandra

import (
	"errors"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCheckReadOnlyAllows(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t",
		"select * from ks.t where id = ?;",
		"SELECT * FROM t;;",
		"SELECT count(*) FROM t WHERE token(id) > token(?)",
		"SELECT writetime(v), ttl(v), maxwritetime(v) FROM t",
		"SELECT toJson(m), blobAsInt(b), intAsBlob(i), blob_as_text(b), text_as_blob(s) FROM t",
		"SELECT system.count(*) FROM t",
		"SELECT JSON * FROM t",
		"SELECT DISTINCT tenant FROM t",
		"SELECT * FROM t WHERE id IN (1, 2, 3)",
		"SELECT * FROM t WHERE (a, b) > (1, 2) ALLOW FILTERING",
		"SELECT CAST(x AS text) FROM t",
		"SELECT * FROM t WHERE s = 'DELETE FROM t'",
		"SELECT * FROM t WHERE s = $$DROP TABLE t$$",
		"SELECT * FROM t -- ; DELETE FROM t\n",
		"SELECT * FROM t WHERE tags CONTAINS 'a' ALLOW FILTERING",
		"SELECT * FROM t WHERE ts > currentTimestamp() AND u < now()",
		"SELECT map_keys(m), collection_count(l), mask_inner(s, 1, 1) FROM t",
		"SELECT similarity_cosine(v, [1.0, 2.0]) FROM t",
		"DESCRIBE TABLE t",
		"DESC KEYSPACES",
		"describe keyspace ks",
		"SELECT floor(ts, 1h), count(*) FROM t WHERE tenant = 'a' GROUP BY floor(ts, 1h)",
		"SELECT * FROM t WHERE m CONTAINS KEY ('a') ALLOW FILTERING",
		"SELECT * FROM t WHERE l CONTAINS (1) ALLOW FILTERING",
	} {
		if err := checkReadOnly(q); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", q, err)
		}
	}
}

func TestCheckReadOnlyRefuses(t *testing.T) {
	for _, q := range []string{
		"",
		"  ;",
		"-- only a comment",
		"DELETE FROM t WHERE id = 1",
		"INSERT INTO t (id) VALUES (1)",
		"INSERT INTO t JSON '{\"id\":1}'",
		"UPDATE t SET a = 1 WHERE id = 1",
		"TRUNCATE t",
		"TRUNCATE TABLE t",
		"BEGIN BATCH INSERT INTO t (id) VALUES (1) APPLY BATCH",
		"BEGIN UNLOGGED BATCH DELETE FROM t WHERE id = 1 APPLY BATCH",
		"SELECT * FROM t; DELETE FROM t WHERE id = 1",
		"SELECT * FROM t;\nDROP TABLE t",
		"/* SELECT */ DELETE FROM t WHERE id = 1",
		"SELECT myfn(id) FROM t",
		"SELECT ks.myfn(id) FROM t",
		`SELECT "count"(id) FROM t`,
		`SELECT ks."myfn"(id) FROM t`,
		"SELECT * FROM t WHERE id = myfn(1)",
		"SELECT myagg(v) FROM t",
		"CREATE TABLE t (id int PRIMARY KEY)",
		"CREATE FUNCTION f (x int) RETURNS NULL ON NULL INPUT RETURNS int LANGUAGE java AS $$return x;$$",
		"DROP TABLE t",
		"ALTER TABLE t ADD x int",
		"GRANT SELECT ON ALL KEYSPACES TO r",
		"REVOKE SELECT ON ALL KEYSPACES FROM r",
		"CREATE ROLE r",
		"LIST ROLES",
		"USE ks",
		"(SELECT * FROM t)",
		"SELECT 'unterminated",
		"SELECT $$x",
		"SELECT * FROM t /* open",
		"SELECT * FROM t WHERE x = 'é' AND y = é",
		// A carriage return ends a line comment, as in Cassandra's lexer.
		"SELECT * FROM t; --x\rDELETE FROM t WHERE id = 1",
		"SELECT * FROM t; //x\rDELETE FROM t WHERE id = 1",
	} {
		err := checkReadOnly(q)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", q, err)
			continue
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("checkReadOnly(%q): want a *driver.Error from %q, got %#v", q, Name, err)
		}
	}
}

func TestCheckRawFilter(t *testing.T) {
	for _, raw := range []string{"age > 30", "name = 'a;b'", "a = 1 -- trailing comment", "token(id) > token(5)", "(a, b) > (1, 2)"} {
		if err := checkRawFilter(raw); err != nil {
			t.Errorf("checkRawFilter(%q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{
		"age > 30; DELETE FROM t", "age > 30 LIMIT 1", "age > 30 ALLOW FILTERING", "a = 1 ORDER BY b",
		"a = 1 GROUP BY a", "a = 1 PER PARTITION LIMIT 1", "a = 'x", "a = (1", "a = 1)",
	} {
		if err := checkRawFilter(raw); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkRawFilter(%q) = %v, want ErrSyntax", raw, err)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{"t": `"t"`, `My"T`: `"My""T"`, "": `""`} {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestBuiltinCalls(t *testing.T) {
	got := builtinCalls("SELECT abs(x), system.count(*), Token(id), floor(ts, 1h) FROM t WHERE y IN (1) AND z = currentDate()")
	if want := []string{"abs", "token", "floor", "currentdate"}; !slices.Equal(got, want) {
		t.Errorf("builtinCalls = %q, want %q (unqualified calls only, lower-cased)", got, want)
	}
	if got := builtinCalls("DESCRIBE TABLE t"); len(got) != 0 {
		t.Errorf("DESCRIBE calls nothing: %q", got)
	}
}

// An unqualified call resolves among the native functions and the user
// functions of the statement's keyspace, so a UDF named like a built-in
// shadows it: such a call is refused, and system.-qualified it passes.
func TestRefuseShadowed(t *testing.T) {
	udfs := map[string]bool{"abs": true}
	if err := refuseShadowed([]string{"token", "abs"}, udfs); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("a call to a name a UDF shares: %v, want ErrReadOnly", err)
	}
	if err := refuseShadowed([]string{"token", "count"}, udfs); err != nil {
		t.Errorf("unshadowed built-ins: %v", err)
	}
	if err := refuseShadowed(nil, nil); err != nil {
		t.Errorf("no calls: %v", err)
	}
}
