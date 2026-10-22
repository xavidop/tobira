//go:build integration

package postgres

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/export"
	"github.com/xavidop/tobira/internal/query"
)

// exportFile runs the host export the way `tobira query --output x.csv`
// does: the call is read-only exactly when the connection is.
func exportFile(t *testing.T, c *conn, text, path string) (export.Result, error) {
	t.Helper()
	return export.ToFile(ctxT(t), export.QuerySource{
		Conn:   c,
		Query:  driver.Query{Text: text, ReadOnly: c.readOnly},
		Driver: "postgres",
	}, export.Options{Format: query.FormatCSV, Path: path})
}

// The native Exporter runs in BEGIN READ ONLY, so a query that writes is
// refused there with ErrReadOnly. On a writable connection the export falls
// back to streaming and succeeds; on a read-only one it stays refused.
func TestExportReadOnlyRefusalFallsBackWhenWritable(t *testing.T) {
	w := openPG(t, false)
	ro := openPG(t, true)
	tbl := scratch(t, w)
	seq := tbl + "_seq"
	mustExec(t, w, "CREATE SEQUENCE "+seq)
	t.Cleanup(func() { mustExec(t, w, "DROP SEQUENCE IF EXISTS "+seq) })
	total := count(t, w, tbl)

	nextval := fmt.Sprintf("select nextval('%s') as n", seq)
	cte := "with d as (delete from " + tbl + " where id = 1 returning id) select * from d"

	t.Run("writable", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "seq.csv")
		res, err := exportFile(t, w, nextval, p)
		if err != nil {
			t.Fatalf("nextval: %v", err)
		}
		if res.Native || res.Rows != 1 {
			t.Errorf("nextval: result = %+v, want one streamed row", res)
		}
		if got := readFile(t, p); got != "n\n1\n" {
			t.Errorf("nextval csv = %q", got)
		}
		p = filepath.Join(dir, "cte.csv")
		res, err = exportFile(t, w, cte, p)
		if err != nil {
			t.Fatalf("cte: %v", err)
		}
		if res.Native || res.Rows != 1 {
			t.Errorf("cte: result = %+v, want one streamed row", res)
		}
		if got := readFile(t, p); got != "id\n1\n" {
			t.Errorf("cte csv = %q", got)
		}
		if n := count(t, w, tbl); n != total-1 {
			t.Errorf("rows = %d, want %d", n, total-1)
		}
	})

	t.Run("read-only", func(t *testing.T) {
		before := count(t, w, tbl)
		for _, text := range []string{nextval, cte} {
			dir := t.TempDir()
			p := filepath.Join(dir, "out.csv")
			_, err := exportFile(t, ro, text, p)
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%q: err = %v, want ErrReadOnly", text, err)
			}
			if _, serr := os.Stat(p); !os.IsNotExist(serr) {
				t.Errorf("%q: target created", text)
			}
		}
		if n := count(t, w, tbl); n != before {
			t.Errorf("rows = %d, want %d: a read-only export wrote", n, before)
		}
		rows, _, _ := collect(t, w, driver.Query{Text: "select last_value, is_called from " + seq})
		if v, _ := rows[0][0].AsInt(); v != 1 {
			t.Errorf("sequence last_value = %d, want 1: a read-only export advanced it", v)
		}
	})
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}
