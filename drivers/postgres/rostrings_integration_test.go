//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// The settings that change how '…' is lexed cannot be changed on the
// read-only path, in any form.
func TestReadOnlyRefusesStringLexingSettings(t *testing.T) {
	w := openPG(t, false)
	ro := openPG(t, true)
	texts := []string{
		"SET standard_conforming_strings = off",
		"SET LOCAL standard_conforming_strings = off",
		"SET SESSION backslash_quote = on",
		"RESET escape_string_warning",
		"select set_config('standard_conforming_strings', 'off', true)",
	}
	tx, err := w.Begin(ctxT(t), driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctxT(t)) }()
	for _, text := range texts {
		if err := queryErr(t, tx.(driver.Querier), driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only Tx: %q: err = %v, want ErrReadOnly", text, err)
		}
		if err := queryErr(t, ro, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only conn: %q: err = %v, want ErrReadOnly", text, err)
		}
		if err := queryErr(t, w, driver.Query{Text: text, ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("per-call ReadOnly: %q: err = %v, want ErrReadOnly", text, err)
		}
	}
	if got := showSCS(t, tx); got != "on" {
		t.Errorf("standard_conforming_strings in the Tx = %q, want on", got)
	}
	if err := queryErr(t, w, driver.Query{Text: "SET standard_conforming_strings = on"}); err != nil {
		t.Errorf("writable: %v", err)
	}
}

// A role whose default is standard_conforming_strings=off makes '\” one
// quote, which the standard lexing misreads. The guard lexes both ways.
func TestReadOnlyGuardWithNonStandardStrings(t *testing.T) {
	w := openPG(t, false)
	mustExec(t, w, "DROP ROLE IF EXISTS tobira_nscs")
	mustExec(t, w, "CREATE ROLE tobira_nscs LOGIN SUPERUSER PASSWORD 'nscs'")
	mustExec(t, w, "ALTER ROLE tobira_nscs SET standard_conforming_strings = off")
	t.Cleanup(func() { mustExec(t, w, "DROP ROLE IF EXISTS tobira_nscs") })
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("tobira_nscs", "nscs")
	open := func(readOnly bool) *conn {
		c, err := New().Open(context.Background(), driver.ConnConfig{DSN: u.String(), ReadOnly: readOnly})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c.(*conn)
	}
	nw, nro := open(false), open(true)
	if got := showSCS(t, nro); got != "off" {
		t.Fatalf("the role's standard_conforming_strings = %q, want off", got)
	}

	var lo int64
	if err := w.pool.QueryRow(ctxT(t), "select lo_from_bytea(0, 'tobira')::int8").Scan(&lo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustExec(t, w, fmt.Sprintf("select lo_unlink(%d)", lo)) })
	probe := func(path string) string { return fmt.Sprintf(`select '\'', lo_export(%d, '%s')`, lo, path) }

	const ctl = "/tmp/tobira_nscs_control"
	if err := queryErr(t, nw, driver.Query{Text: probe(ctl)}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if !fileExists(t, w, ctl) {
		t.Fatal("control: the probe left no file, so it proves nothing")
	}
	for i, q := range []struct {
		c  *conn
		ro bool
	}{{nro, false}, {nw, true}} {
		path := fmt.Sprintf("/tmp/tobira_nscs_%d", i)
		if err := queryErr(t, q.c, driver.Query{Text: probe(path), ReadOnly: q.ro}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("probe %d: err = %v, want ErrReadOnly", i, err)
		}
		if fileExists(t, w, path) {
			t.Errorf("probe %d ran: %s exists", i, path)
		}
	}
	tx, err := nw.Begin(ctxT(t), driver.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctxT(t)) }()
	path := "/tmp/tobira_nscs_tx"
	if err := queryErr(t, tx.(driver.Querier), driver.Query{Text: probe(path)}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only Tx probe: err = %v, want ErrReadOnly", err)
	}
	if fileExists(t, w, path) {
		t.Errorf("read-only Tx probe ran: %s exists", path)
	}
}

func showSCS(t *testing.T, q driver.Querier) string {
	t.Helper()
	rs, err := q.Query(ctxT(t), driver.Query{Text: "show standard_conforming_strings"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows[0][0].String()
}
