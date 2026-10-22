//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/xavidop/tobira/driver"
)

func exportString(t *testing.T, c *conn, q driver.Query, format string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Export(ctxT(t), q, format, &buf); err != nil {
		t.Fatalf("Export(%q, %s): %v", q.Text, format, err)
	}
	return buf.String()
}

func TestExportFormats(t *testing.T) {
	c := openPG(t, false)
	if got := strings.Join(c.ExportFormats(), ","); got != "csv,binary" {
		t.Errorf("ExportFormats = %s", got)
	}
}

func TestExportCSV(t *testing.T) {
	for _, ro := range []bool{false, true} {
		c := openPG(t, ro)
		got := exportString(t, c, driver.Query{Text: "select id, name from tobira_people where id <= 2 order by id;"}, "csv")
		lines := strings.Split(strings.TrimSpace(got), "\n")
		if len(lines) != 3 || lines[0] != "id,name" || !strings.HasPrefix(lines[1], "1,") {
			t.Errorf("readOnly=%v: csv = %q", ro, got)
		}
	}
}

func TestExportCSVQuoting(t *testing.T) {
	c := openPG(t, false)
	got := exportString(t, c, driver.Query{Text: `values ('a,b', 'say "hi"', 'two
lines', null)`}, "csv")
	want := "column1,column2,column3,column4\n\"a,b\",\"say \"\"hi\"\"\",\"two\nlines\",\n"
	if got != want {
		t.Errorf("csv = %q, want %q", got, want)
	}
}

func TestExportBinary(t *testing.T) {
	c := openPG(t, false)
	got := exportString(t, c, driver.Query{Text: "select id from tobira_people order by id"}, "binary")
	if !strings.HasPrefix(got, "PGCOPY\n\xff\r\n\x00") {
		t.Errorf("binary output lacks the PGCOPY signature: %q", got[:min(len(got), 16)])
	}
}

func TestExportStatementKinds(t *testing.T) {
	c := openPG(t, false)
	for _, text := range []string{
		"with p as (select id from tobira_people) select * from p",
		"values (1), (2)",
		"table tobira_people",
		"(select 1) union all (select 2)",
		"select 1 -- trailing line comment",
		"select ')' as paren, ';' as semi, $$)$$ as dollar",
	} {
		if got := exportString(t, c, driver.Query{Text: text}, "csv"); got == "" {
			t.Errorf("%q exported nothing", text)
		}
	}
}

func TestExportNamespace(t *testing.T) {
	c := openPG(t, false)
	got := exportString(t, c, driver.Query{Text: "select label from things", Namespace: "tobira_ns"}, "csv")
	if got != "label\nin-ns\n" {
		t.Errorf("csv = %q", got)
	}
	// The search_path change did not leak to the pooled connection.
	if rows, _, _ := collect(t, c, driver.Query{Text: "select current_schema()"}); len(rows) != 1 {
		t.Fatal("no row")
	} else if s, _ := rows[0][0].AsText(); s != "public" {
		t.Errorf("current_schema() = %q after export", s)
	}
}

func TestExportRefusals(t *testing.T) {
	c := openPG(t, false)
	before := count(t, c, "tobira_people")
	for name, tt := range map[string]struct {
		q      driver.Query
		format string
	}{
		"delete":       {driver.Query{Text: "delete from tobira_people"}, "csv"},
		"insert":       {driver.Query{Text: "insert into tobira_people select * from tobira_people"}, "csv"},
		"copy":         {driver.Query{Text: "copy tobira_people to stdout"}, "csv"},
		"two":          {driver.Query{Text: "select 1; delete from tobira_people"}, "csv"},
		"params":       {driver.Query{Text: "select * from tobira_people where id = $1", Params: []driver.Value{driver.Int(1)}}, "csv"},
		"limit":        {driver.Query{Text: "select 1", Limit: 1}, "csv"},
		"format":       {driver.Query{Text: "select 1"}, "parquet"},
		"empty":        {driver.Query{}, "csv"},
		"close paren":  {driver.Query{Text: "select 1) TO PROGRAM 'touch /tmp/tobira-pwned' --"}, "csv"},
		"close+opts":   {driver.Query{Text: "select 1) TO STDOUT WITH (FORMAT binary) --"}, "csv"},
		"close+stmt":   {driver.Query{Text: "select 1) TO PROGRAM 'x'; delete from tobira_people; (select 1"}, "csv"},
		"comment hide": {driver.Query{Text: "select 1 /* ) TO PROGRAM 'x' */ ) TO PROGRAM 'y' --"}, "csv"},
		"open comment": {driver.Query{Text: "select 1 /* "}, "csv"},
		"open string":  {driver.Query{Text: "select '"}, "csv"},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			err := c.Export(ctxT(t), tt.q, tt.format, &buf)
			if !errors.Is(err, driver.ErrUnsupported) {
				t.Errorf("err = %v, want ErrUnsupported", err)
			}
			if buf.Len() != 0 {
				t.Errorf("wrote %d bytes before refusing", buf.Len())
			}
		})
	}
	if n := count(t, c, "tobira_people"); n != before {
		t.Errorf("tobira_people has %d rows, had %d: a refused export wrote", n, before)
	}
}

// Even a statement that passes the lexical check cannot write: Export always
// runs in a read-only transaction.
func TestExportCannotWrite(t *testing.T) {
	c := openPG(t, false)
	tbl := scratch(t, c)
	before := count(t, c, tbl)
	err := c.Export(ctxT(t), driver.Query{Text: "with d as (delete from " + tbl + " returning *) select * from d"}, "csv", &bytes.Buffer{})
	if err == nil {
		t.Fatal("a data-modifying CTE was exported")
	}
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
	if n := count(t, c, tbl); n != before {
		t.Errorf("rows = %d, want %d: the export deleted", n, before)
	}
}

func TestExportSyntaxError(t *testing.T) {
	c := openPG(t, false)
	err := c.Export(ctxT(t), driver.Query{Text: "select * from nope"}, "csv", &bytes.Buffer{})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	err = c.Export(ctxT(t), driver.Query{Text: "select from from"}, "csv", &bytes.Buffer{})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("err = %v, want ErrSyntax", err)
	}
	if rows, _, _ := collect(t, c, driver.Query{Text: "select 1"}); len(rows) != 1 {
		t.Error("pool unusable after a failed export")
	}
}

// countWriter counts bytes and keeps nothing: an export of a large table must
// not be buffered by the driver.
type countWriter struct {
	n        int64
	onWrite  func(total int64)
	maxChunk int
}

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	w.maxChunk = max(w.maxChunk, len(p))
	if w.onWrite != nil {
		w.onWrite(w.n)
	}
	return len(p), nil
}

func TestExportStreams(t *testing.T) {
	c := openPG(t, false)
	w := &countWriter{}
	if err := c.Export(ctxT(t), driver.Query{Text: "select n from tobira_big"}, "csv", w); err != nil {
		t.Fatal(err)
	}
	if w.n < 10<<20 { // 2M rows
		t.Errorf("wrote %d bytes, want > 10 MiB", w.n)
	}
	if w.maxChunk > 1<<20 {
		t.Errorf("largest single write = %d bytes: the driver buffered", w.maxChunk)
	}
}

func TestExportCancelMidStream(t *testing.T) {
	for _, ro := range []bool{false, true} {
		c := openPG(t, ro)
		ctx, cancel := context.WithCancel(context.Background())
		w := &countWriter{onWrite: func(total int64) {
			if total > 1<<20 {
				cancel()
			}
		}}
		start := time.Now()
		err := c.Export(ctx, driver.Query{Text: "select n from tobira_big"}, "csv", w)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("readOnly=%v: err = %v, want context.Canceled", ro, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("cancel took %v", d)
		}
		if w.n > 64<<20 {
			t.Errorf("kept writing: %d bytes", w.n)
		}
		if rows, _, _ := collect(t, c, driver.Query{Text: "select 42"}); len(rows) != 1 {
			t.Error("pool unusable after a cancelled export")
		}
	}
}

func TestExportCancelBeforeAnyRow(t *testing.T) {
	c := openPG(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	err := c.Export(ctx, driver.Query{Text: "select pg_sleep(10)"}, "csv", &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestExportWriterError(t *testing.T) {
	c := openPG(t, false)
	err := c.Export(ctxT(t), driver.Query{Text: "select n from tobira_big"}, "csv", failWriter{})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v, want the writer's error", err)
	}
	if rows, _, _ := collect(t, c, driver.Query{Text: "select 1"}); len(rows) != 1 {
		t.Error("pool unusable after a writer error")
	}
}

func TestExportOnClosedConn(t *testing.T) {
	c := openPG(t, false)
	_ = c.Close()
	err := c.Export(ctxT(t), driver.Query{Text: "select 1"}, "csv", &bytes.Buffer{})
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("err = %v, want ErrConnection", err)
	}
}

// fileExists asks the server (a superuser in the test container) whether path
// exists on its filesystem.
func fileExists(t *testing.T, c *conn, path string) bool {
	t.Helper()
	var ok bool
	if err := c.pool.QueryRow(context.Background(), "select (pg_stat_file($1, true)).size is not null", path).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestExportInjectionLive runs injection attempts against the real server.
// The test user is a superuser, so COPY ... TO PROGRAM / TO file would work if
// the text could reach it; the control proves the probe sees such a write.
func TestExportInjectionLive(t *testing.T) {
	c := openPG(t, false)
	ro := openPG(t, true)

	const ctl = "/tmp/tobira_inject_control"
	mustExec(t, c, "copy (select 1) to program 'touch "+ctl+"'")
	if !fileExists(t, c, ctl) {
		t.Fatal("control: COPY TO PROGRAM left no file, so the probe proves nothing")
	}

	cases := []struct {
		name, text string
		mayRun     bool // legitimately a single SELECT: running it is fine, it just must not write
	}{
		{"program", `select 1) to program 'touch /tmp/tobira_inject_1' --`, false},
		{"file", `select 1) to '/tmp/tobira_inject_2'`, false},
		{"escaped quote", `select E'\'') to program 'touch /tmp/tobira_inject_3' --`, false},
		{"backslash plain string", `select '\') to program 'touch /tmp/tobira_inject_4' --`, false},
		{"unicode escape", `select U&'\0041' UESCAPE '!') to program 'touch /tmp/tobira_inject_5' --`, false},
		{"digit dollar tag", `select $1$) to program 'touch /tmp/tobira_inject_6' -- $1$`, false},
		{"dollar after ident", `select a$b$) to program 'touch /tmp/tobira_inject_7' --`, false},
		{"nested comment ends early", `select 1 /* /* */ */) to program 'touch /tmp/tobira_inject_8' --`, false},
		{"second statement", `select 1; copy (select 1) to program 'touch /tmp/tobira_inject_9'`, false},
		{"balanced close", `select 1) to program 'touch /tmp/tobira_inject_10' (select 1`, false},
		{"program inside parens", `select 1 from (select 1) x where (1) = 1) to program 'touch /tmp/tobira_inject_11' --(`, false},
		{"trailing line comment", "select 1 -- ) to program 'touch /tmp/tobira_inject_12'", true},
		{"trailing block comment", "select 1 /* ) to program 'touch /tmp/tobira_inject_13' */", true},
	}
	for _, tt := range cases {
		for _, conn := range []*conn{c, ro} {
			var buf bytes.Buffer
			err := conn.Export(ctxT(t), driver.Query{Text: tt.text}, "csv", &buf)
			if !tt.mayRun && err == nil {
				t.Errorf("%s (readOnly=%v): accepted", tt.name, conn.readOnly)
			}
			if !tt.mayRun && buf.Len() != 0 {
				t.Errorf("%s: wrote %d bytes", tt.name, buf.Len())
			}
		}
	}
	for i := 1; i <= 13; i++ {
		if p := fmt.Sprintf("/tmp/tobira_inject_%d", i); fileExists(t, c, p) {
			t.Errorf("%s exists: an injected COPY target ran", p)
		}
	}
	if fileExists(t, c, "/tmp/tobira_inject_2") {
		t.Error("file target written")
	}
}

// The inner text is parsed alone before it is embedded, so a text the scanner
// misjudged still cannot reach the COPY statement.
func TestExportRefusesTextWithParameters(t *testing.T) {
	c := openPG(t, false)
	err := c.Export(ctxT(t), driver.Query{Text: "select $1::int"}, "csv", &bytes.Buffer{})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestExportParseErrorIsSyntax(t *testing.T) {
	c := openPG(t, false)
	err := c.Export(ctxT(t), driver.Query{Text: "select 1 where"}, "csv", &bytes.Buffer{})
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("err = %v, want ErrSyntax", err)
	}
}

// A failed copy must not leave its connection mid-COPY in the pool. On a pool
// of one: after a writer failure (the socket state is unknown) the connection
// is discarded, so the next query runs on a new backend; after a cancel the
// server ends the COPY with an error and the connection is clean, so it may be
// reused, but only once rolled back and reset (never idle in transaction).
func TestExportFailureDoesNotPoisonPool(t *testing.T) {
	open := func(t *testing.T) (*conn, func() int64) {
		o, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN + "&pool_max_conns=1"})
		if err != nil {
			t.Fatal(err)
		}
		c := o.(*conn)
		t.Cleanup(func() { _ = c.Close() })
		return c, func() int64 {
			rows, _, _ := collect(t, c, driver.Query{Text: "select pg_backend_pid()"})
			n, _ := rows[0][0].AsInt()
			return n
		}
	}
	t.Run("writer error discards", func(t *testing.T) {
		c, pid := open(t)
		before := pid()
		if err := c.Export(ctxT(t), driver.Query{Text: "select n from tobira_big"}, "csv", failWriter{}); err == nil {
			t.Fatal("no error")
		}
		after := pid()
		t.Logf("backend pid before %d, after %d", before, after)
		if after == before {
			t.Errorf("connection with an unknown COPY state was reused (pid %d)", after)
		}
	})
	t.Run("cancel resets", func(t *testing.T) {
		c, pid := open(t)
		before := pid()
		ctx, cancel := context.WithCancel(context.Background())
		w := &countWriter{onWrite: func(total int64) {
			if total > 1<<20 {
				cancel()
			}
		}}
		if err := c.Export(ctx, driver.Query{Text: "select n from tobira_big"}, "csv", w); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		after := pid()
		t.Logf("backend pid before %d, after %d", before, after)
		// Whatever the pid, the session is idle and read-write again.
		rows, _, _ := collect(t, c, driver.Query{Text: "select current_setting('transaction_read_only'), (select count(*) from pg_stat_activity where pid = pg_backend_pid() and state = 'active')"})
		if ro, _ := rows[0][0].AsText(); ro != "off" {
			t.Errorf("transaction_read_only = %q after a cancelled export", ro)
		}
		var st string
		admin := openPG(t, false)
		if err := admin.pool.QueryRow(context.Background(), "select coalesce((select state from pg_stat_activity where pid = $1), 'gone')", after).Scan(&st); err != nil {
			t.Fatal(err)
		}
		if st == "idle in transaction" {
			t.Errorf("session state %q after a cancelled export", st)
		}
	})
}

// Export runs in BEGIN READ ONLY, which does not stop lo_export writing a
// server file, so Export refuses server-file functions on every connection.
// The test user is a superuser. The control runs the very COPY Export would
// build, inside BEGIN READ ONLY, and proves it writes the file.
func TestExportRefusesServerFileAccessLive(t *testing.T) {
	w := openPG(t, false)
	ro := openPG(t, true)
	var lo int64
	if err := w.pool.QueryRow(ctxT(t), "select lo_from_bytea(0, 'tobira')::int8").Scan(&lo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustExec(t, w, fmt.Sprintf("select lo_unlink(%d)", lo)) })
	probe := func(path string) string { return fmt.Sprintf("select lo_export(%d,'%s')", lo, path) }

	const ctl = "/tmp/tobira_export_lo_control"
	tx, err := w.pool.BeginTx(ctxT(t), pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Conn().PgConn().CopyTo(ctxT(t), &bytes.Buffer{}, "COPY ("+probe(ctl)+"\n) TO STDOUT WITH (FORMAT csv, HEADER true)")
	_ = tx.Rollback(ctxT(t))
	if err != nil {
		t.Fatalf("control COPY: %v", err)
	}
	if !fileExists(t, w, ctl) {
		t.Fatal("control: the COPY left no file, so the probe proves nothing")
	}

	for i, c := range []struct {
		name string
		c    *conn
	}{{"writable conn", w}, {"read-only conn", ro}} {
		path := fmt.Sprintf("/tmp/tobira_export_lo_%d", i)
		err := c.c.Export(ctxT(t), driver.Query{Text: probe(path)}, "csv", &bytes.Buffer{})
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s: err = %v, want ErrReadOnly", c.name, err)
		}
		if fileExists(t, w, path) {
			t.Errorf("%s: lo_export ran: %s exists", c.name, path)
		}
	}
}
