package dynamodb

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCheckReadOnlyAllows(t *testing.T) {
	for _, s := range []string{
		`SELECT * FROM "tobira_people"`,
		`select id, name from tobira_people where id = ?`,
		`  SELECT * FROM "t"."by_email" WHERE email = 'a@b.c'`,
		`SELECT * FROM t WHERE name = 'it''s; DELETE FROM t'`,
		`SELECT * FROM t WHERE "update" = 1`,
		`SELECT * FROM t -- DELETE FROM t` + "\n" + `WHERE id = 1`,
		`/* a comment */ SELECT * FROM t`,
		`SELECT * FROM t WHERE begins_with(name, 'A') AND size(tags) > 1`,
		`SELECT * FROM t WHERE id IN [1, 2, 3] AND x IS MISSING`,
		`SELECT * FROM t;`,
		`SELECT * FROM t WHERE a <> 1 AND b >= 2 AND c <= 3 AND d != 4`,
		`SELECT * FROM t WHERE m.a[0] = 1.5e3`,
		`SELECT * FROM "a""b"`,
	} {
		if err := checkReadOnly(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestCheckReadOnlyRefuses(t *testing.T) {
	for _, s := range []string{
		``,
		`   `,
		`-- only a comment`,
		`DELETE FROM t WHERE id = 1`,
		`delete from t where id = 1`,
		`UPDATE t SET a = 1 WHERE id = 1`,
		`update t set a = 1 where id = 1`,
		`INSERT INTO t VALUE {'id': 1}`,
		`/* SELECT */ DELETE FROM t WHERE id = 1`,
		`-- SELECT` + "\n" + `DELETE FROM t WHERE id = 1`,
		`SELECT * FROM t; DELETE FROM t WHERE id = 1`,
		`SELECT * FROM t;;`,
		`SELECT * FROM t WHERE EXISTS(SELECT * FROM t)`,
		`EXISTS(SELECT * FROM t WHERE id = 1)`,
		`SELECT * FROM t WHERE x = 1 REMOVE y`,
		`SELECT * FROM t SET a = 1`,
		`REPLACE INTO t VALUE {'id': 1}`,
		`UPSERT INTO t VALUE {'id': 1}`,
		`CREATE TABLE t`,
		`DROP TABLE t`,
		`"SELECT" * FROM t`,
		`'SELECT' * FROM t`,
		`(SELECT * FROM t)`,
		`SELECT * FROM t WHERE a = 'unterminated`,
		`SELECT * FROM "unterminated`,
		`SELECT * FROM t /* unterminated`,
		`SELECT * FROM t /* nested /* */ */`,
		"SELECT * FROM t WHERE a = `ion`",
		`SELECT * FROM t WHERE a = 'x' ` + " " + `DELETE`,
		`SELECTX * FROM t`,
		`SELECT * FROM t WHERE a = 1 RETURNING ALL OLD *`,
		`EXEC something`,
	} {
		err := checkReadOnly(s)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%q: want ErrReadOnly, got %v", s, err)
		}
		var de *driver.Error
		if err != nil && (!errors.As(err, &de) || de.Driver != Name) {
			t.Errorf("%q: want a *driver.Error from %s, got %T", s, Name, err)
		}
	}
}

func TestSelectShape(t *testing.T) {
	for _, tc := range []struct {
		stmt, table, index string
		cols               []string
	}{
		{`SELECT * FROM "tobira_people"`, "tobira_people", "", nil},
		{`select id, "name", email from people where id = 1`, "people", "", []string{"id", "name", "email"}},
		{`SELECT a, size(b) FROM t`, "t", "", nil},
		{`SELECT a.b FROM t`, "t", "", nil},
		{`SELECT * FROM "t"."by_x" WHERE x = 1`, "t", "by_x", nil},
		{`SELECT * FROM t.idx`, "t", "idx", nil},
		{`DELETE FROM t WHERE id = 1`, "", "", nil},
	} {
		toks, err := lex(tc.stmt)
		if err != nil {
			t.Fatalf("%q: %v", tc.stmt, err)
		}
		sh := selectShape(toks)
		if sh.table != tc.table || sh.index != tc.index || !equalStrings(sh.columns, tc.cols) {
			t.Errorf("%q: got %+v", tc.stmt, sh)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
