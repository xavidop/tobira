package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/api"
	"github.com/xavidop/tobira/drivers/sqlite"
)

func TestDemoDatabase(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), &out, nil); err != nil {
		t.Fatal(err)
	}
	want := "table main.users\nid\tname\temail\n1\tAda\tada@example.com\n2\tGrace\tgrace@example.com\n3\tLinus\tNULL\n(3 rows)\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestLimitTruncates(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), &out, []string{"-limit", "2", "-query", "select name from users order by id"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "name\nAda\nGrace\n(2 rows, truncated by -limit)\n") {
		t.Errorf("output:\n%s", out.String())
	}
}

// TestOwnDatabaseIsReadOnly: an existing database is opened read-only, so
// a write is refused with api.ErrReadOnly and the data is unchanged.
func TestOwnDatabaseIsReadOnly(t *testing.T) {
	ctx := context.Background()
	reg := api.NewRegistry()
	if err := reg.Add(sqlite.New()); err != nil {
		t.Fatal(err)
	}
	dsn := "sqlite:" + filepath.Join(t.TempDir(), "own.db")
	if err := seed(ctx, reg, dsn); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := explore(ctx, &out, reg, dsn, "delete from users", 0)
	if err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Fatalf("err = %v, want a read-only refusal", err)
	}
	out.Reset()
	if err := run(ctx, &out, []string{"-dsn", dsn, "-query", "select count(*) as n from users"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "n\n3\n") {
		t.Errorf("rows changed after a refused delete:\n%s", out.String())
	}
}

// TestErrorsAreRedacted: a DSN password never reaches an error.
func TestErrorsAreRedacted(t *testing.T) {
	const pw = "hunter2-embedding"
	err := run(context.Background(), &bytes.Buffer{}, []string{"-dsn", "postgres://app:" + pw + "@db.invalid/app"})
	if err == nil {
		t.Fatal("want an error: this program links no postgres driver")
	}
	if strings.Contains(err.Error(), pw) {
		t.Errorf("the password leaked: %v", err)
	}
	if errors.Is(err, api.ErrReadOnly) {
		t.Errorf("unexpected kind: %v", err)
	}
}
