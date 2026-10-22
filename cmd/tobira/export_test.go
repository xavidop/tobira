package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestQueryOutputEndToEnd exports from a real sqlite database through the
// binary and checks the file, the stderr line, stdout and history.
func TestQueryOutputEndToEnd(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	dsn := "sqlite:" + filepath.Join(t.TempDir(), "e.db")
	if _, code := run(t, bin, env, "query", dsn, "create table t (id integer primary key, name text, ok boolean)"); code != 0 {
		t.Fatalf("create: %d", code)
	}
	if _, code := run(t, bin, env, "query", dsn, "insert into t values (1,'a,b',1),(2,NULL,0),(3,'',1)"); code != 0 {
		t.Fatalf("insert: %d", code)
	}
	p := filepath.Join(t.TempDir(), "t.csv")
	out, errOut, code := runBoth(t, bin, env, "query", dsn, "select * from t order by id", "--output", p)
	if code != 0 || out != "" || errOut != "wrote 3 rows to "+p+"\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	stdout, _ := run(t, bin, env, "query", dsn, "select * from t order by id", "--format", "csv")
	if string(b) != stdout {
		t.Errorf("file %q differs from --format csv stdout %q", b, stdout)
	}
	if got := listHistory(t, bin, env); len(got) < 2 || got[1].Rows != 3 || !got[1].OK {
		t.Errorf("history = %+v, want the export recorded with 3 rows", got)
	}
}

// TestQueryOutputNoResultSetEndToEnd: an UPDATE through --output writes no
// file and history records its affected rows, as without --output.
func TestQueryOutputNoResultSetEndToEnd(t *testing.T) {
	bin := buildBinary(t)
	env := isolatedEnv(t)
	dsn := "sqlite:" + filepath.Join(t.TempDir(), "e.db")
	run(t, bin, env, "query", dsn, "create table t (id integer primary key)")
	run(t, bin, env, "query", dsn, "insert into t values (1),(2),(3)")
	dir := t.TempDir()
	p := filepath.Join(dir, "u.csv")
	_, errOut, code := runBoth(t, bin, env, "query", dsn, "update t set id = id + 10 where id < 3", "--output", p)
	if code != 0 || errOut != "no result set: 2 rows affected; nothing written to "+p+"\n" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files written: %v", ents)
	}
	if got := listHistory(t, bin, env); len(got) < 1 || got[0].Rows != 2 || !got[0].OK {
		t.Errorf("history = %+v, want 2 affected rows", got)
	}
}
