package tui

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"fmt"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/palette"
	"github.com/xavidop/tobira/internal/tui/sidebar"
)

// editConfig is the config of the inline editing tests: local can write,
// ro is read-only.
func editConfig() *config.Config {
	return &config.Config{Connections: []config.Connection{
		{Name: "local", Driver: "fake", DSN: "fake://local"},
		{Name: "ro", Driver: "fake", DSN: "fake://ro", ReadOnly: true},
	}}
}

// newEditHarness is newHarness on a Fake with opts. forceRO is the
// --read-only flag. drv, when set, wraps the Fake's driver.
func newEditHarness(t *testing.T, forceRO bool, wrap func(driver.Driver) driver.Driver, opts ...drivertest.FakeOption) *harness {
	t.Helper()
	f := drivertest.NewFake(append([]drivertest.FakeOption{drivertest.WithPeople()}, opts...)...)
	other := drivertest.PeopleDataset()
	f.AddCollection("main", "other", other.Columns, other.Rows[:3], other.Key...)
	f.AddCollection("main", "nokey", other.Columns, other.Rows[:3])
	reg := driver.NewRegistry()
	d := f.Driver()
	if wrap != nil {
		d = wrap(d)
	}
	if err := reg.Add(d); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(editConfig(), reg, secrets.Resolver{}, forceRO)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark"})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, m: model.(*Model), fake: f, mgr: mgr, ch: make(chan tea.Msg, 4096)}
	h.run(h.m.Init())
	h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	return h
}

// openColl opens conn's collection name in the grid and waits for its
// schema, which enables editing.
func (h *harness) openColl(conn, name string) {
	h.t.Helper()
	if _, err := h.mgr.Open(context.Background(), conn); err != nil {
		h.t.Fatal(err)
	}
	h.send(sidebar.OpenCollectionMsg{Conn: conn, Ref: driver.CollectionRef{Namespace: "main", Name: name}})
	h.pump(func() bool {
		return h.m.src != nil && h.m.src.Described() && h.m.gr.Status().Loaded > 0 && !h.m.gr.Loading()
	})
}

func (h *harness) palette(cmd string) {
	h.t.Helper()
	h.keys(":")
	h.typ(cmd)
	h.keys("enter")
	h.drain(30 * time.Millisecond) // the palette delivers its command
}

// editCell opens the cell editor on the cursor, replaces its text and
// presses enter.
func (h *harness) editCell(text string) {
	h.t.Helper()
	h.keys("e")
	if h.m.cell == nil {
		h.t.Fatalf("e opened no cell editor:\n%s", h.text())
	}
	h.send(tea.KeyPressMsg{Code: tea.KeyEnd})
	h.send(ctrl('u'))
	h.typ(text)
	h.keys("enter")
}

// col moves the grid cursor to the column named name.
func (h *harness) col(name string) {
	h.t.Helper()
	h.send(tea.KeyPressMsg{Code: tea.KeyHome})
	for _, c := range h.m.gr.Columns() {
		if c.Name == name {
			return
		}
		h.keys("right")
	}
	h.t.Fatalf("no column %q", name)
}

// row moves the grid cursor to grid row i (pinned rows first).
func (h *harness) row(i int) {
	h.t.Helper()
	_, c := h.m.gr.Cursor()
	h.m.gr.SetCursor(i, c)
}

// fakeRow reads the People row with id from the Fake, nil when it is gone.
func fakeRow(t *testing.T, f *drivertest.Fake, id int64) driver.Row {
	t.Helper()
	c, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rs, err := c.Query(context.Background(), driver.Query{Text: "select * from tobira_people where id = " + itoa(int(id))})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

func mutationOps(f *drivertest.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		switch c.Op {
		case "insert", "update", "delete", "begin", "commit", "rollback":
			out = append(out, c.Op)
		}
	}
	return out
}

func (h *harness) flashText() string { return ansi.Strip(h.m.st.View(400)) }

// pendingSetup edits row id=1's name, inserts row 100 and marks id=2 for
// deletion: 3 pending.
func (h *harness) pendingSetup() {
	h.t.Helper()
	h.col("name")
	h.editCell("Grace")
	h.keys("i")
	if i, ok := h.m.gr.PinnedAt(); !ok || i != 0 {
		h.t.Fatalf("i: cursor not on the inserted row")
	}
	if _, c := h.m.gr.Cursor(); c != 0 {
		h.t.Fatalf("i: cursor on column %d, want the first editable one", c)
	}
	h.editCell("100")
	h.col("name")
	h.editCell("New Person")
	h.row(2) // pinned row, id=1, id=2
	h.keys("d")
	if n := h.m.pend.count(); n != 3 {
		h.t.Fatalf("pending = %d, want 3", n)
	}
}

func TestInlineEditApplyInATx(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.pendingSetup()
	if !strings.Contains(h.text(), "3 pending") {
		t.Fatalf("no pending count:\n%s", h.text())
	}
	h.palette("apply")
	h.pump(func() bool { return h.m.apply == nil })
	if !strings.Contains(h.flashText(), "applied 3 changes") {
		t.Fatalf("no success flash: %q", h.flashText())
	}
	if got := strings.Join(mutationOps(h.fake), ","); got != "begin,update,insert,delete,commit" {
		t.Fatalf("mutations = %s", got)
	}
	if r := fakeRow(t, h.fake, 1); r == nil || r[1].String() != "Grace" {
		t.Fatalf("row 1 = %v", r)
	}
	if r := fakeRow(t, h.fake, 100); r == nil || r[1].String() != "New Person" {
		t.Fatalf("row 100 = %v", r)
	}
	if r := fakeRow(t, h.fake, 2); r != nil {
		t.Fatalf("row 2 not deleted: %v", r)
	}
	if !h.m.pend.empty() || strings.Contains(h.text(), "pending") {
		t.Fatalf("pending left after apply:\n%s", h.text())
	}
	// The grid is reloaded and shows the new data.
	h.pump(func() bool { return !h.m.gr.Loading() && strings.Contains(h.text(), "Grace") })
}

func TestInlineApplyTxFailureAppliesNothing(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.pendingSetup()
	h.fake.FailNext("insert", driver.Wrap(driver.ErrConstraint, "fake", "insert",
		&textErr{"no for fake://u:hunter22@db \x1b[2Jcleared"}))
	h.palette("apply")
	h.pump(func() bool { return h.m.apply == nil })
	raw := h.raw()
	if strings.Contains(raw, "hunter22") || strings.Contains(raw, "\x1b[2J") {
		t.Fatalf("error not redacted/escaped:\n%s", ansi.Strip(raw))
	}
	if f := h.flashText(); !strings.Contains(f, "insert row +1: ") || !strings.Contains(f, `\x1b[2Jcleared`) {
		t.Fatalf("flash = %q", f)
	}
	if got := strings.Join(mutationOps(h.fake), ","); got != "begin,update,insert,rollback" {
		t.Fatalf("mutations = %s", got)
	}
	if r := fakeRow(t, h.fake, 1); r[1].String() == "Grace" {
		t.Fatal("the update was applied")
	}
	if n := h.m.pend.count(); n != 3 {
		t.Fatalf("pending = %d, want 3", n)
	}
	checkFrame(t, h, winW, winH)
}

func TestInlineApplyWithoutTransactorStopsAtTheFirstError(t *testing.T) {
	h := newEditHarness(t, false, nil, drivertest.WithoutCapabilities(driver.CapTransactor))
	h.openColl("local", "tobira_people")
	h.keys("i")
	h.editCell("100")
	h.row(2) // id=2
	h.keys("d")
	for i, id := range []int{3, 4, 5} {
		h.row(id)
		h.col("age")
		h.editCell(itoa(90 + i))
	}
	if n := h.m.pend.count(); n != 5 {
		t.Fatalf("pending = %d, want 5", n)
	}
	h.fake.FailNext("update", errors.New("boom"))
	h.palette("apply")
	h.pump(func() bool { return h.m.apply == nil })
	if f := h.flashText(); !strings.Contains(f, "applied 2 of 5 — failed at update id=3: boom") {
		t.Fatalf("flash = %q", f)
	}
	if got := strings.Join(mutationOps(h.fake), ","); got != "insert,delete,update" {
		t.Fatalf("mutations = %s", got)
	}
	if fakeRow(t, h.fake, 100) == nil || fakeRow(t, h.fake, 2) != nil {
		t.Fatal("the first two changes were not applied")
	}
	if n := h.m.pend.count(); n != 3 {
		t.Fatalf("pending = %d, want the 3 updates", n)
	}
	if len(h.m.pend.inserts) != 0 {
		t.Fatal("the applied insert is still pending")
	}
}

func TestInlineRefusals(t *testing.T) {
	t.Run("keyless", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		h.openColl("local", "nokey")
		for _, k := range []string{"e", "i", "d"} {
			h.keys(k)
			if f := h.flashText(); !strings.Contains(f, noEditText) {
				t.Fatalf("%s: flash = %q", k, f)
			}
		}
		h.palette("apply")
		if h.m.cell != nil || len(mutationOps(h.fake)) != 0 {
			t.Fatal("a keyless collection was edited")
		}
	})
	t.Run("no mutator", func(t *testing.T) {
		h := newEditHarness(t, false, nil, drivertest.WithoutCapabilities(driver.CapMutator))
		if _, err := h.mgr.Open(context.Background(), "local"); err != nil {
			t.Fatal(err)
		}
		h.send(sidebar.OpenCollectionMsg{Conn: "local", Ref: driver.CollectionRef{Namespace: "main", Name: "tobira_people"}})
		h.until("Ada Lovelace")
		h.keys("e")
		if f := h.flashText(); !strings.Contains(f, noEditText) {
			t.Fatalf("flash = %q", f)
		}
		for _, c := range h.fake.Calls() {
			if c.Op == "describe" {
				t.Fatal("described a collection that cannot be edited")
			}
		}
	})
	t.Run("read-only config", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		h.openColl("ro", "tobira_people")
		for _, k := range []string{"e", "i", "d"} {
			h.keys(k)
			if f := h.flashText(); !strings.Contains(f, "🔒 ro is read-only; the edit was refused") {
				t.Fatalf("%s: flash = %q", k, f)
			}
		}
	})
	t.Run("read-only flag", func(t *testing.T) {
		h := newEditHarness(t, true, nil)
		h.openColl("local", "tobira_people")
		h.keys("e")
		if f := h.flashText(); !strings.Contains(f, "🔒 local is read-only; the edit was refused") {
			t.Fatalf("flash = %q", f)
		}
	})
	t.Run("read-only after editing", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		h.openColl("local", "tobira_people")
		h.col("name")
		h.editCell("Grace")
		cfg := editConfig()
		cfg.Connections[0].ReadOnly = true
		h.mgr.Reload(cfg)
		h.palette("apply")
		if f := h.flashText(); !strings.Contains(f, "🔒 local is read-only; the edit was refused") {
			t.Fatalf("flash = %q", f)
		}
		if h.m.apply != nil || len(mutationOps(h.fake)) != 0 || h.m.pend.count() != 1 {
			t.Fatal(":apply ran on a read-only connection")
		}
	})
	t.Run("result tab", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		h.openColl("local", "tobira_people")
		h.send(ctrl('e'))
		h.send(tea.PasteMsg{Content: "select * from tobira_people"})
		h.runUntilDone(keyF5)
		h.keys("esc", "e")
		if f := h.flashText(); !strings.Contains(f, "edit rows in the browse tab") {
			t.Fatalf("flash = %q", f)
		}
	})
	t.Run("generated", func(t *testing.T) {
		h := newEditHarness(t, false, func(d driver.Driver) driver.Driver { return genDriver{d} })
		h.openColl("local", "tobira_people")
		h.keys("e")
		if f := h.flashText(); !strings.Contains(f, "id is generated") || h.m.cell != nil {
			t.Fatalf("flash = %q", f)
		}
		h.keys("i")
		if _, c := h.m.gr.Cursor(); c != 1 {
			t.Fatalf("i put the cursor on column %d, want 1 (id is generated)", c)
		}
	})
}

// genDriver is the Fake whose id column is generated.
type genDriver struct{ driver.Driver }

func (d genDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &genConn{Conn: c, Browser: c.(driver.Browser), Mutator: c.(driver.Mutator)}, nil
}

type genConn struct {
	driver.Conn
	driver.Browser
	driver.Mutator
}

func (c *genConn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	s, err := c.Conn.Describe(ctx, ref)
	if err == nil && len(s.Fields) > 0 {
		s.Fields[0].Generated = true
	}
	return s, err
}

func TestInlineTypedParseErrors(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	for _, tt := range []struct{ col, text, err string }{
		{"age", "abc", "not a whole number"},
		{"score", "1,5", "not a number"},
		{"active", "maybe", "not a boolean"},
		{"created_at", "yesterday", "not a time"},
	} {
		h.col(tt.col)
		h.editCell(tt.text)
		if h.m.cell == nil || !strings.Contains(h.text(), tt.err) {
			t.Fatalf("%s=%q: editor closed or no %q:\n%s", tt.col, tt.text, tt.err, h.text())
		}
		checkFrame(t, h, winW, winH)
		h.keys("esc")
	}
	if !h.m.pend.empty() {
		t.Fatal("a failed parse left a pending change")
	}
	// Committing the text unchanged changes nothing.
	h.col("age")
	h.keys("e", "enter")
	if !h.m.pend.empty() || h.m.cell != nil {
		t.Fatal("an unchanged edit is pending")
	}
	// A value back to the original drops the cell.
	h.editCell("70")
	h.editCell("21") // id=1's age as loaded
	if !h.m.pend.empty() {
		t.Fatalf("pending = %d after restoring the value", h.m.pend.count())
	}
}

func TestInlineSetNull(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.col("id")
	h.keys("e")
	h.send(ctrl('n'))
	if f := h.flashText(); !strings.Contains(f, "id can't be NULL") || h.m.cell == nil {
		t.Fatalf("flash = %q, editor open = %v", f, h.m.cell != nil)
	}
	h.keys("esc")
	h.col("age")
	h.keys("e")
	h.send(ctrl('n'))
	if h.m.cell != nil || h.m.pend.count() != 1 {
		t.Fatal("ctrl+n did not set age to NULL")
	}
	if c := h.m.pend.changes()[0]; !c.values[0].Value.IsNull() {
		t.Fatalf("change = %+v", c)
	}
}

func TestInlineJSONEditor(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.col("tags") // id=1: ["admin","beta"]
	h.keys("e")
	if h.m.cell == nil || !h.m.cell.multi() {
		t.Fatal("tags did not open the multi-line editor")
	}
	if !strings.Contains(h.text(), `"beta"`) || !strings.Contains(h.text(), "edit tags") {
		t.Fatalf("pretty JSON not shown:\n%s", h.text())
	}
	h.keys("enter") // a newline, not a commit
	if h.m.cell == nil {
		t.Fatal("enter committed the JSON editor")
	}
	h.typ("x")
	h.send(ctrl('s'))
	if h.m.cell == nil || !strings.Contains(h.text(), "not valid JSON") {
		t.Fatalf("invalid JSON accepted:\n%s", h.text())
	}
	checkFrame(t, h, winW, winH)
	h.send(tea.KeyPressMsg{Code: tea.KeyBackspace})
	h.send(tea.KeyPressMsg{Code: tea.KeyBackspace})
	h.send(tea.KeyPressMsg{Code: tea.KeyBackspace})
	h.typ(`, "new"]`)
	h.send(keyAltEnter)
	if h.m.cell != nil || h.m.pend.count() != 1 {
		t.Fatalf("alt+enter did not commit:\n%s", h.text())
	}
	if v := h.m.pend.changes()[0].values[0].Value.String(); v != `["admin","beta","new"]` {
		t.Fatalf("tags = %s", v)
	}
}

func TestInlineDiscardConfirm(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.palette("discard")
	if f := h.flashText(); !strings.Contains(f, "nothing to discard") {
		t.Fatalf("flash = %q", f)
	}
	h.col("name")
	h.editCell("Grace")
	h.palette("discard")
	if !strings.Contains(h.text(), "discard 1 pending change? y/N") {
		t.Fatalf("no confirmation:\n%s", h.text())
	}
	h.keys("n")
	if h.m.guardSt != nil || h.m.pend.count() != 1 {
		t.Fatal("n discarded the changes")
	}
	h.palette("discard")
	h.keys("y")
	h.drain(20 * time.Millisecond)
	if !h.m.pend.empty() || !strings.Contains(h.flashText(), "discarded 1 change") {
		t.Fatalf("y kept the changes: %q", h.flashText())
	}
}

func TestInlineLeavingAsksFirst(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.col("name")
	h.editCell("Grace")
	other := sidebar.OpenCollectionMsg{Conn: "local", Ref: driver.CollectionRef{Namespace: "main", Name: "other"}}
	h.send(other)
	if !strings.Contains(h.text(), "discard 1 pending change? y/N") {
		t.Fatalf("no confirmation:\n%s", h.text())
	}
	checkFrame(t, h, winW, winH)
	h.keys("x") // anything but y keeps the edits
	if h.m.cur.ref.Name != "tobira_people" || h.m.pend.count() != 1 {
		t.Fatal("the collection changed without a yes")
	}
	h.send(other)
	h.keys("y")
	h.pump(func() bool { return h.m.cur.ref.Name == "other" })
	if !h.m.pend.empty() {
		t.Fatal("pending kept after leaving")
	}
	// Reopening the same collection keeps its edits.
	h.openColl("local", "other")
	h.col("name")
	h.editCell("Kept")
	h.send(other)
	if h.m.guardSt != nil || h.m.pend.count() != 1 {
		t.Fatal("reopening the same collection asked or dropped the edits")
	}
	// :reload asks too.
	h.palette("reload")
	if h.m.guardSt == nil {
		t.Fatal(":reload did not ask")
	}
	h.keys("esc")
}

func TestInlineQuitAsksFirst(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.col("name")
	h.editCell("Grace")
	h.keys("q")
	if h.quitted() || h.m.guardSt == nil {
		t.Fatal("q quit with pending changes")
	}
	h.keys("n")
	h.send(ctrl('c'))
	if h.quitted() || h.m.guardSt == nil {
		t.Fatal("ctrl+c quit with pending changes")
	}
	h.keys("esc")
	h.palette("q")
	if h.m.guardSt == nil {
		t.Fatal(":q did not ask")
	}
	h.keys("y")
	if !h.quitted() {
		t.Fatal("y did not quit")
	}
}

func TestInlineCtrlCCancelsApply(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts []drivertest.FakeOption
		want string
	}{
		{"transaction", nil, "apply cancelled; nothing was applied"},
		{"sequential", []drivertest.FakeOption{drivertest.WithoutCapabilities(driver.CapTransactor)}, "apply cancelled; applied 0 of 3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newEditHarness(t, false, nil, tt.opts...)
			h.openColl("local", "tobira_people")
			h.pendingSetup()
			h.fake.SetLatency(5 * time.Second)
			h.palette("apply")
			if h.m.apply == nil {
				t.Fatal("no apply running")
			}
			h.palette("apply")
			if f := h.flashText(); !strings.Contains(f, "changes are already being applied") {
				t.Fatalf("second apply: %q", f)
			}
			h.keys("e")
			if f := h.flashText(); !strings.Contains(f, "changes are being applied; ctrl+c cancels") {
				t.Fatalf("e during apply: %q", f)
			}
			start := time.Now()
			h.send(ctrl('c'))
			if h.quitted() {
				t.Fatal("ctrl+c quit during an apply")
			}
			h.pump(func() bool { return h.m.apply == nil })
			if time.Since(start) > 3*time.Second {
				t.Fatal("the apply was not cancelled")
			}
			if f := h.flashText(); !strings.Contains(f, tt.want) {
				t.Fatalf("flash = %q, want %q", f, tt.want)
			}
			if n := h.m.pend.count(); n != 3 {
				t.Fatalf("pending = %d", n)
			}
			h.fake.SetLatency(0)
			if r := fakeRow(t, h.fake, 1); r[1].String() == "Grace" {
				t.Fatal("a cancelled apply changed the data")
			}
		})
	}
}

func TestInlineOpenTxSeam(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	conn, _ := h.mgr.Conn("local")
	tx, err := conn.(driver.Transactor).Begin(context.Background(), driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	h.m.tx = &txState{conn: "local", tx: tx}
	h.col("name")
	h.editCell("InTx")
	before := len(mutationOps(h.fake))
	h.palette("apply")
	h.pump(func() bool { return h.m.apply == nil })
	ops := mutationOps(h.fake)[before:]
	if strings.Join(ops, ",") != "update" {
		t.Fatalf("mutations through the open tx = %v (no begin or commit expected)", ops)
	}
	if r := fakeRow(t, h.fake, 1); r[1].String() == "InTx" {
		t.Fatal("the open transaction was committed")
	}
}

func TestInlinePendingGolden(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.pendingSetup()
	h.row(1)
	h.col("id")
	h.drain(20 * time.Millisecond)
	checkFrame(t, h, winW, winH)
	golden(t, "edit_pending", h.text())
	lines := strings.Split(h.raw(), "\n")
	var warn, struck bool
	for _, l := range lines {
		if regexp.MustCompile(`48;2;251;191;36`).MatchString(l) && strings.Contains(ansi.Strip(l), "Grace") {
			warn = true
		}
		if regexp.MustCompile(`\x1b\[(?:[0-9;]*;)?9(?:;[0-9;]*)?m`).MatchString(l) && strings.Contains(ansi.Strip(l), "Conan") {
			struck = true
		}
	}
	if !warn || !struck {
		t.Fatalf("highlight %v, strikethrough %v", warn, struck)
	}
	// The scalar cell editor at the bottom.
	h.col("name")
	h.keys("e")
	golden(t, "edit_cell", h.text())
	checkFrame(t, h, winW, winH)
}

func TestInlineTinyWindows(t *testing.T) {
	for _, sz := range [][2]int{{20, 5}, {1, 1}, {59, 10}, {100, 4}} {
		t.Run(itoa(sz[0])+"x"+itoa(sz[1]), func(t *testing.T) {
			h := newEditHarness(t, false, nil)
			h.openColl("local", "tobira_people")
			h.send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
			h.m.focus = focusGrid
			h.keys("i")
			checkFrame(t, h, sz[0], sz[1])
			h.keys("e")
			checkFrame(t, h, sz[0], sz[1])
			h.keys("esc")
			h.col("tags")
			h.keys("e")
			checkFrame(t, h, sz[0], sz[1])
			h.keys("esc")
			h.keys("q")
			checkFrame(t, h, sz[0], sz[1])
		})
	}
}

// TestProgramInlineEdit runs the real program under teatest: edit a cell,
// apply it and quit.
func TestProgramInlineEdit(t *testing.T) {
	f := drivertest.NewFake(drivertest.WithPeople())
	reg := driver.NewRegistry()
	_ = reg.Add(f.Driver())
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "local", Driver: "fake", DSN: "fake://local"}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark"})
	if err != nil {
		t.Fatal(err)
	}
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(winW, winH))
	waitFor := func(s string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return strings.Contains(ansi.Strip(string(b)), s) }, teatest.WithDuration(5*time.Second))
	}
	send := func(s string) {
		for _, r := range s {
			tm.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
	waitFor("connections")
	tm.Send(key("enter"))
	waitFor("main")
	tm.Send(key("down"))
	tm.Send(key("enter"))
	waitFor("tobira_people")
	tm.Send(key("down"))
	tm.Send(key("enter"))
	waitFor("Ada Lovelace")
	tm.Send(key("right"))
	// e is refused until the schema read is back: retry until it opens.
	out := tm.Output()
	seen := ""
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(seen, "edit name") {
		if time.Now().After(deadline) {
			t.Fatal("the cell editor never opened")
		}
		send("e")
		end := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(end) && !strings.Contains(seen, "edit name") {
			b, _ := io.ReadAll(out)
			seen += ansi.Strip(string(b))
			time.Sleep(20 * time.Millisecond)
		}
	}
	tm.Send(ctrl('u'))
	send("Teatest")
	tm.Send(key("enter"))
	waitFor("1 pending")
	send(":apply")
	tm.Send(key("enter"))
	waitFor("applied 1 change")
	send("q")
	tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second))
	if r := fakeRow(t, f, 1); r == nil || r[1].String() != "Teatest" {
		t.Fatalf("row 1 = %v", r)
	}
}

// A config write or reload in flight refuses edits and :apply.
func TestInlineRefusedDuringAConfigWrite(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.pump(func() bool { return h.m.src.Described() })
	h.startSlowRun()
	h.keys("esc", ":")
	h.typ("reload")
	h.keys("enter")
	h.pump(func() bool { return h.m.saving })
	h.m.active = 0 // the browse tab
	h.keys("e")
	if f := h.flashText(); !strings.Contains(f, busyText) || h.m.cell != nil {
		t.Fatalf("e during a reload: %q", f)
	}
	h.fake.SetLatency(0)
	h.pump(func() bool { return !h.m.saving })
	// :apply too.
	h.pump(func() bool { return h.m.src != nil && h.m.src.Described() && h.m.gr.Status().Loaded > 0 })
	h.col("name")
	h.editCell("Grace")
	h.m.saving = true
	h.send(palette.ApplyCmd{})
	if f := h.flashText(); !strings.Contains(f, busyText) || h.m.apply != nil {
		t.Fatalf(":apply during a config write: %q", f)
	}
	h.m.saving = false
}

// An apply on a connection about to be closed is cancelled and waited
// for, like an editor run.
func TestInlineApplyStoppedBeforeAConnectionClose(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.col("name")
	h.editCell("Grace")
	h.fake.SetLatency(5 * time.Second)
	h.send(palette.ApplyCmd{})
	h.pump(func() bool {
		calls := h.fake.Calls()
		return len(calls) > 0 && calls[len(calls)-1].Op == "begin"
	})
	h.m.pauseGridOf("local")
	// The write waits on the apply's done channel (it may already be
	// closed, the cancel being quick).
	if len(h.m.stopping) != 1 || !h.m.apply.cancelled {
		t.Fatalf("stopping = %d, cancelled = %v; want the apply", len(h.m.stopping), h.m.apply.cancelled)
	}
	stopping := h.m.takeStopping()
	start := time.Now()
	waitStopped(stopping)
	if time.Since(start) > 2*time.Second {
		t.Fatal("the apply was not cancelled")
	}
	calls := h.fake.Calls()
	if c := calls[len(calls)-1]; c.Op != "begin" || !errors.Is(c.Err, context.Canceled) {
		t.Fatalf("last call = %s %v, want a cancelled begin", c.Op, c.Err)
	}
	h.pump(func() bool { return h.m.apply == nil })
	if n := h.m.pend.count(); n != 1 {
		t.Fatalf("pending = %d", n)
	}
}

// A late result of a cancelled read does not cancel the newer one.
func TestInlineStaleDescribeKeepsTheNewRead(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	first := h.m.describe(h.m.src)
	second := h.m.describe(h.m.src) // cancels the first
	h.send(first())
	if h.m.descCancel == nil {
		t.Fatal("the stale result cancelled the newer read")
	}
	h.send(second())
	if h.m.descCancel != nil || !h.m.src.Described() {
		t.Fatal("the newer read was not recorded")
	}
}

// A failed read is not kept; the next edit key reads again.
func TestInlineDescribeErrorIsRetried(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.fake.FailNext("describe", errors.New("flaky \x1b[2J"))
	if _, err := h.mgr.Open(context.Background(), "local"); err != nil {
		t.Fatal(err)
	}
	h.send(openMsg("local", "tobira_people"))
	h.until("Ada Lovelace")
	h.pump(func() bool { return h.m.descCancel == nil })
	h.keys("e")
	if f := h.flashText(); !strings.Contains(f, "could not read the collection's schema: ") || !strings.Contains(f, `flaky \x1b[2J`) {
		t.Fatalf("flash = %q", f)
	}
	h.pump(func() bool { return h.m.src.Described() })
	h.keys("e")
	if h.m.cell == nil {
		t.Fatalf("e after a successful re-read: %q", h.flashText())
	}
}

// An apply stuck in a driver that ignores cancellation does not trap
// the user.
func TestInlineQuitWithAStuckApply(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{}, 1)
	t.Cleanup(func() { close(release) })
	h := newEditHarness(t, false, func(d driver.Driver) driver.Driver { return stuckDriver{d, release, entered} })
	h.openColl("local", "tobira_people")
	h.col("name")
	h.editCell("Grace")
	h.send(palette.ApplyCmd{})
	if h.m.apply == nil {
		t.Fatal("no apply")
	}
	<-entered // the driver is in Update, ignoring its context
	h.send(ctrl('c'))
	if f := h.flashText(); !strings.Contains(f, "cancelling the apply") {
		t.Fatalf("flash = %q", f)
	}
	h.keys("q")
	if h.quitted() {
		t.Fatal("q quit right after the cancel")
	}
	h.send(ctrl('c'))
	if h.quitted() {
		t.Fatal("ctrl+c quit right after the cancel")
	}
	later := time.Now().Add(stuckAfter + time.Second)
	h.m.now = func() time.Time { return later }
	h.keys("q")
	if !h.quitted() {
		t.Fatal("q did not quit with a stuck apply")
	}
	const want = "quitting with a stuck apply; changes already applied stay applied" // no Transactor: one at a time
	if f := h.flashText(); !strings.Contains(f, want) || h.m.exitNotice != want {
		t.Fatalf("flash = %q, exit notice = %q", f, h.m.exitNotice)
	}
}

// stuckDriver's Update ignores its context until release is closed.
type stuckDriver struct {
	driver.Driver
	release chan struct{}
	entered chan struct{}
}

func (d stuckDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &stuckConn{Conn: c, Browser: c.(driver.Browser), Mutator: c.(driver.Mutator), release: d.release, entered: d.entered}, nil
}

type stuckConn struct {
	driver.Conn
	driver.Browser
	driver.Mutator
	release chan struct{}
	entered chan struct{}
}

func (c *stuckConn) Update(context.Context, driver.CollectionRef, driver.Object, driver.Object) error {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-c.release
	return errors.New("released")
}

// A failed rollback is reported with the error that caused it.
func TestInlineRollbackErrorIsShown(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.pendingSetup()
	h.fake.FailNext("insert", errors.New("boom"))
	h.fake.FailNext("rollback", errors.New("rb for fake://u:hunter22@db"))
	h.send(palette.ApplyCmd{})
	h.pump(func() bool { return h.m.apply == nil })
	f := h.flashText()
	if !strings.Contains(f, "insert row +1: boom; rollback failed: rb for") || strings.Contains(f, "hunter22") {
		t.Fatalf("flash = %q", f)
	}
}

// The remaining guarded routes.
func TestInlineGuardedConnectionRoutes(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.col("name")
	h.editCell("Grace")
	for _, tt := range []struct {
		name string
		do   func()
	}{
		{":connect ro", func() { h.palette("connect ro") }},
		{":connection edit local", func() { h.palette("connection edit local") }},
		{":connection rm local", func() { h.palette("connection rm local") }},
		{"E on local", func() { h.m.sb = h.m.sb.Select("local"); h.m.focus = focusSidebar; h.keys("E") }},
		{"D on local", func() { h.m.sb = h.m.sb.Select("local"); h.m.focus = focusSidebar; h.keys("D") }},
	} {
		tt.do()
		if h.m.guardSt == nil {
			t.Fatalf("%s did not ask:\n%s", tt.name, h.text())
		}
		h.keys("n")
		h.m.focus = focusGrid
	}
	// The other connection's entry does not touch the edits.
	h.palette("connection edit ro")
	if h.m.guardSt != nil {
		t.Fatal("editing another connection asked")
	}
	if h.m.pend.count() != 1 {
		t.Fatal("the edits were dropped")
	}
}

// A primary key edit updates the row found by its key as loaded.
func TestInlinePrimaryKeyEditUsesTheOriginalKey(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.col("id")
	h.editCell("500")
	h.send(palette.ApplyCmd{})
	h.pump(func() bool { return h.m.apply == nil })
	if fakeRow(t, h.fake, 1) != nil {
		t.Fatal("row 1 still exists")
	}
	if r := fakeRow(t, h.fake, 500); r == nil || r[1].String() != "Ōtani Shōhei" {
		t.Fatalf("row 500 = %v", r)
	}
}

// An empty edit is the empty string; untouched input over NULL or an
// unset field changes nothing; NULL is only set with ctrl+n.
func TestInlineEmptyStringAndNull(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.col("name")
	h.keys("e")
	h.send(ctrl('n'))
	h.keys("e")
	if first := strings.Split(ansi.Strip(h.m.cellView(80)), "\n")[0]; !strings.Contains(first, "> NULL") && !strings.Contains(first, ">  NULL") {
		t.Fatalf("no NULL placeholder: %q", first)
	}
	h.keys("enter") // untouched: still NULL
	if v := h.m.pend.changes()[0].values[0].Value; !v.IsNull() {
		t.Fatalf("untouched enter changed NULL to %v", v)
	}
	h.keys("e", "x")
	h.send(tea.KeyPressMsg{Code: tea.KeyBackspace})
	h.keys("enter")
	if v := h.m.pend.changes()[0].values[0].Value; v.Type() != driver.TypeString || v.String() != "" {
		t.Fatalf("an emptied input = %#v, want ''", v)
	}
	// An inserted row: untouched enter leaves the field unset.
	h.keys("i")
	h.col("name")
	h.keys("e")
	if first := strings.Split(ansi.Strip(h.m.cellView(80)), "\n")[0]; !strings.Contains(first, "default") {
		t.Fatalf("no default placeholder: %q", first)
	}
	h.keys("enter")
	if len(h.m.pend.inserts[0].values) != 0 {
		t.Fatalf("untouched enter set %v", h.m.pend.inserts[0].values)
	}
}

// An auto-increment key is never required on insert.
func TestInlineInsertSkipsAutoIncrement(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people") // the Fake reports id AutoIncrement
	h.keys("i")
	h.col("name")
	h.editCell("Auto")
	h.send(palette.ApplyCmd{})
	h.pump(func() bool { return h.m.apply == nil })
	if f := h.flashText(); !strings.Contains(f, "applied 1 change") {
		t.Fatalf("flash = %q", f)
	}
	if r := fakeRow(t, h.fake, int64(len(drivertest.People.Rows)+1)); r == nil || r[1].String() != "Auto" {
		t.Fatalf("new row = %v", r)
	}
}

func openMsg(conn, name string) tea.Msg {
	return sidebar.OpenCollectionMsg{Conn: conn, Ref: driver.CollectionRef{Namespace: "main", Name: name}}
}

// A stuck apply in a transaction says the transaction is
// rolled back, and Run prints the notice on stderr after the program exits.
func TestInlineStuckApplyNoticeInATransaction(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.col("name")
	h.editCell("Grace")
	now := time.Now()
	h.m.now = func() time.Time { return now }
	h.m.apply = &applyRun{id: 99, cancel: func() {}, done: make(chan struct{}), mode: applyTx, conn: "local",
		cancelled: true, cancelAt: now.Add(-stuckAfter - time.Second)}
	h.keys("q")
	if !h.quitted() {
		t.Fatal("q did not quit")
	}
	const want = "quitting with a stuck apply; the transaction is rolled back when the connection closes"
	if h.m.exitNotice != want {
		t.Fatalf("exit notice = %q", h.m.exitNotice)
	}
	var buf strings.Builder
	reportExit(&buf, h.m)
	if buf.String() != "warning: "+want+"\n" {
		t.Fatalf("stderr = %q", buf.String())
	}
	buf.Reset()
	h.m.exitNotice = ""
	reportExit(&buf, h.m)
	reportExit(nil, h.m)
	if buf.Len() != 0 {
		t.Fatalf("stderr without a notice = %q", buf.String())
	}
}

// A read-only refusal from the driver during :apply
// (a standby, a role with default_transaction_read_only) is worded like the
// editor's, not shown as the driver's raw text.
func TestApplyDriverReadOnlyRefusalIsWordedLikeTheEditor(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.col("name")
	h.editCell("Standby")
	h.fake.FailNext("update", fmt.Errorf("fake: ERROR: cannot execute UPDATE in a read-only transaction: %w", driver.ErrReadOnly))
	h.send(palette.ApplyCmd{})
	h.pump(func() bool { return h.m.apply == nil })
	f := h.flashText()
	if !strings.Contains(f, "🔒 local is read-only; the edit was refused") || strings.Contains(f, "cannot execute") {
		t.Fatalf("flash = %q", f)
	}
}
