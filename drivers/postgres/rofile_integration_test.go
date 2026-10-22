//go:build integration

package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// On the read-only path COPY and the server-file functions are refused before
// they run, whether the connection is read-only, the call is, or the Tx is.
// The test user is a superuser, so each probe would write a file if it ran;
// the control proves pg_stat_file sees such a write.
func TestReadOnlyRefusesServerFileAccess(t *testing.T) {
	w := openPG(t, false)
	ro := openPG(t, true)

	var lo int64
	if err := w.pool.QueryRow(ctxT(t), "select lo_from_bytea(0, 'tobira')::int8").Scan(&lo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustExec(t, w, fmt.Sprintf("select lo_unlink(%d)", lo)) })

	const ctl = "/tmp/tobira_rofile_control"
	mustExec(t, w, fmt.Sprintf("select lo_export(%d, '%s')", lo, ctl))
	if !fileExists(t, w, ctl) {
		t.Fatal("control: lo_export left no file, so the probe proves nothing")
	}

	n := 0
	probes := func() map[string]string {
		n++
		f := func(i int) string { return fmt.Sprintf("/tmp/tobira_rofile_%d_%d", n, i) }
		return map[string]string{
			"COPY (select 1) TO PROGRAM 'touch " + f(1) + "'":                      f(1),
			"COPY tobira_people TO '" + f(2) + "'":                                 f(2),
			fmt.Sprintf("select lo_export(%d, '%s')", lo, f(3)):                    f(3),
			fmt.Sprintf(`select "lo_export"(%d, '%s')`, lo, f(4)):                  f(4),
			fmt.Sprintf("/* x */ select pg_catalog.LO_EXPORT(%d, '%s')", lo, f(5)): f(5),
		}
	}
	check := func(name string, q driver.Querier, readOnly bool) {
		t.Helper()
		for text, path := range probes() {
			err := queryErr(t, q, driver.Query{Text: text, ReadOnly: readOnly})
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%s: %q: err = %v, want ErrReadOnly", name, text, err)
			}
			if fileExists(t, w, path) {
				t.Errorf("%s: %q ran: %s exists", name, text, path)
			}
		}
	}
	check("read-only conn", ro, false)
	check("per-call ReadOnly", w, true)

	tx, err := w.Begin(ctxT(t), driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	check("read-only Tx", tx.(driver.Querier), false)
	if err := tx.Rollback(ctxT(t)); err != nil {
		t.Fatal(err)
	}

	path := "/tmp/tobira_rofile_explain"
	_, err = ro.Explain(ctxT(t), driver.Query{Text: fmt.Sprintf("select lo_export(%d, '%s')", lo, path)}, driver.ExplainOptions{Analyze: true})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("explain analyze: err = %v, want ErrReadOnly", err)
	}
	if fileExists(t, w, path) {
		t.Errorf("explain analyze ran lo_export: %s exists", path)
	}

	// A writable call is not affected.
	path = "/tmp/tobira_rofile_writable"
	if err := queryErr(t, w, driver.Query{Text: "COPY (select 1) TO PROGRAM 'touch " + path + "'"}); err != nil {
		t.Errorf("writable COPY: %v", err)
	}
	if !fileExists(t, w, path) {
		t.Error("writable COPY TO PROGRAM left no file")
	}
}

// A dollar-quote tag may hold bytes >= 0x80 ($é$): the guard must read the
// text as the server does, or a '…' it imagines hides the lo_export call.
// The writable control proves the same text really runs lo_export.
func TestReadOnlyRefusesLoExportBehindNonASCIIDollarTag(t *testing.T) {
	w := openPG(t, false)
	ro := openPG(t, true)
	var lo int64
	if err := w.pool.QueryRow(ctxT(t), "select lo_from_bytea(0, 'tobira')::int8").Scan(&lo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustExec(t, w, fmt.Sprintf("select lo_unlink(%d)", lo)) })
	probe := func(path string) string {
		return fmt.Sprintf("select $é$'$é$, lo_export(%d, '%s') --'", lo, path)
	}

	const ctl = "/tmp/tobira_rofile_tag_control"
	if err := queryErr(t, w, driver.Query{Text: probe(ctl)}); err != nil {
		t.Fatalf("writable control: %v", err)
	}
	if !fileExists(t, w, ctl) {
		t.Fatal("control: the probe text left no file, so the probe proves nothing")
	}

	for i, c := range []struct {
		name     string
		q        driver.Querier
		readOnly bool
	}{{"read-only conn", ro, false}, {"per-call ReadOnly", w, true}} {
		path := fmt.Sprintf("/tmp/tobira_rofile_tag_%d", i)
		err := queryErr(t, c.q, driver.Query{Text: probe(path), ReadOnly: c.readOnly})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s: err = %v, want ErrReadOnly", c.name, err)
		}
		if fileExists(t, w, path) {
			t.Errorf("%s: lo_export ran: %s exists", c.name, path)
		}
	}
}

// An anonymous code block can build SQL at run time that the lexical guard
// cannot see, so DO is refused on the read-only path, and only there.
func TestReadOnlyRefusesDO(t *testing.T) {
	w := openPG(t, false)
	ro := openPG(t, true)
	const do = "DO $$ begin perform 1; end $$"
	for _, text := range []string{do, "/* x */ do $$ begin perform 1; end $$", ";DO LANGUAGE plpgsql $$ begin perform 1; end $$"} {
		if err := queryErr(t, ro, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only conn: %q: err = %v, want ErrReadOnly", text, err)
		}
		if err := queryErr(t, w, driver.Query{Text: text, ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("per-call ReadOnly: %q: err = %v, want ErrReadOnly", text, err)
		}
	}
	tx, err := w.Begin(ctxT(t), driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := queryErr(t, tx.(driver.Querier), driver.Query{Text: do}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only Tx: err = %v, want ErrReadOnly", err)
	}
	_ = tx.Rollback(ctxT(t))
	if err := queryErr(t, w, driver.Query{Text: do}); err != nil {
		t.Errorf("writable conn: %v", err)
	}
}
