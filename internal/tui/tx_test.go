package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"sync"
	"sync/atomic"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/tui/palette"
)

// beginTx runs :tx begin from the grid and waits for the transaction.
func (h *harness) beginTx() {
	h.t.Helper()
	h.palette("tx begin")
	h.pump(func() bool { return h.m.tx != nil && h.m.tx.tx != nil && h.m.tx.busy == "" })
}

// runInEditor runs text in the editor with F5, waits for the result and
// gives the focus back to the grid.
func (h *harness) runInEditor(text string) {
	h.t.Helper()
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: text})
	h.runUntilDone(keyF5)
	h.keys("esc")
}

// clearEditor empties the editor buffer and gives the focus back.
func (h *harness) clearEditor() {
	h.t.Helper()
	h.m.ed.SetValue("")
}

func TestTxBeginRunApplyCommit(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.beginTx()
	if !strings.Contains(h.statusLine(), "TX") {
		t.Fatalf("no TX in the status bar: %q", h.statusLine())
	}
	if h.m.tx.ro {
		t.Fatal("a writable connection got a read-only transaction")
	}
	// An editor run goes through the transaction: invisible outside it.
	h.runInEditor("delete from tobira_people where id = 5")
	if r := fakeRow(t, h.fake, 5); r == nil {
		t.Fatal("the run in the transaction was visible outside it")
	}
	h.clearEditor()
	h.m.active = 0 // back to the browse tab
	// :apply writes through the transaction and does not commit.
	h.col("name")
	h.editCell("InTx")
	before := len(mutationOps(h.fake))
	h.palette("apply")
	h.pump(func() bool { return h.m.apply == nil })
	if ops := mutationOps(h.fake)[before:]; strings.Join(ops, ",") != "update" {
		t.Fatalf("apply in the transaction: mutations = %v", ops)
	}
	if f := h.flashText(); !strings.Contains(f, "applied 1 change inside the open transaction") {
		t.Fatalf("apply flash = %q", f)
	}
	if r := fakeRow(t, h.fake, 1); r[1].String() == "InTx" {
		t.Fatal("the apply was committed")
	}
	h.palette("tx commit")
	h.pump(func() bool { return h.m.tx == nil })
	if f := h.flashText(); !strings.Contains(f, "committed") {
		t.Fatalf("commit flash = %q", f)
	}
	if r := fakeRow(t, h.fake, 5); r != nil {
		t.Fatal("the committed delete did not land")
	}
	if r := fakeRow(t, h.fake, 1); r[1].String() != "InTx" {
		t.Fatalf("row 1 after commit = %v", r)
	}
	if strings.Contains(h.statusLine(), "TX") {
		t.Fatalf("TX still shown after commit: %q", h.statusLine())
	}
	// The browse tab is refreshed and shows the committed data.
	h.pump(func() bool { return !h.m.gr.Loading() && strings.Contains(h.text(), "InTx") })
}

func TestTxRollbackLeavesDataUnchanged(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.beginTx()
	h.col("name")
	h.editCell("Gone")
	h.palette("apply")
	h.pump(func() bool { return h.m.apply == nil })
	h.palette("tx rollback")
	h.pump(func() bool { return h.m.tx == nil })
	if f := h.flashText(); !strings.Contains(f, "rolled back") {
		t.Fatalf("flash = %q", f)
	}
	if r := fakeRow(t, h.fake, 1); r[1].String() == "Gone" {
		t.Fatal("a rolled back change landed")
	}
	ops := mutationOps(h.fake)
	if ops[len(ops)-1] != "rollback" {
		t.Fatalf("mutations = %v", ops)
	}
}

func TestTxPartialApply(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.beginTx()
	h.pendingSetup() // update id=1, insert +1 (id 100), delete id=2
	h.fake.FailNext("insert", errors.New("boom"))
	h.palette("apply")
	h.pump(func() bool { return h.m.apply == nil })
	f := h.flashText()
	if !strings.Contains(f, "applied 1 of 3 — failed at insert +1: boom") ||
		!strings.Contains(f, "1 change written inside the open transaction; :tx rollback to undo") {
		t.Fatalf("flash = %q", f)
	}
	if n := h.m.pend.count(); n != 2 {
		t.Fatalf("pending = %d, want the insert and the delete", n)
	}
	if h.m.tx == nil {
		t.Fatal("the failure closed the transaction")
	}
	if r := fakeRow(t, h.fake, 1); r[1].String() == "Grace" {
		t.Fatal("the written change is visible outside the transaction")
	}
	h.palette("tx commit")
	h.pump(func() bool { return h.m.tx == nil })
	if r := fakeRow(t, h.fake, 1); r[1].String() != "Grace" {
		t.Fatalf("row 1 after commit = %v", r)
	}
	if fakeRow(t, h.fake, 100) != nil || fakeRow(t, h.fake, 2) == nil {
		t.Fatal("changes that were never written landed")
	}
}

func TestTxRefusals(t *testing.T) {
	t.Run("no transactor", func(t *testing.T) {
		h := newEditHarness(t, false, nil, drivertest.WithoutCapabilities(driver.CapTransactor))
		h.openColl("local", "tobira_people")
		h.palette("tx begin")
		h.pump(func() bool {
			return h.m.tx == nil && strings.Contains(h.flashText(), "transactions are not supported by fake")
		})
	})
	t.Run("second begin and nothing open", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		h.openColl("local", "tobira_people")
		h.palette("tx commit")
		if f := h.flashText(); !strings.Contains(f, "no transaction is open") {
			t.Fatalf("commit with nothing open: %q", f)
		}
		h.beginTx()
		first := h.m.tx.tx
		h.palette("tx begin")
		if f := h.flashText(); !strings.Contains(f, "a transaction is already open") || h.m.tx.tx != first {
			t.Fatalf("second begin: %q", f)
		}
	})
	t.Run("begin in flight", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		h.openColl("local", "tobira_people")
		h.fake.SetLatency(300 * time.Millisecond)
		h.palette("tx begin")
		if h.m.tx == nil || h.m.tx.busy != "begin" {
			t.Fatal("begin not in flight")
		}
		h.send(palette.TxCmd{Op: "commit"})
		if f := h.flashText(); !strings.Contains(f, "the transaction is still starting") {
			t.Fatalf("commit during begin: %q", f)
		}
		h.pump(func() bool { return h.m.tx != nil && h.m.tx.tx != nil })
	})
	t.Run("explain without a Tx Explainer", func(t *testing.T) {
		h := newEditHarness(t, false, nil, drivertest.WithoutCapabilities(driver.CapExplainer))
		h.openColl("local", "tobira_people")
		h.beginTx()
		h.send(ctrl('e'))
		h.send(tea.PasteMsg{Content: "select * from tobira_people"})
		h.send(palette.ExplainCmd{})
		h.pump(func() bool { return h.m.running == nil })
		if !strings.Contains(h.text(), "explain inside a transaction is not supported by fake") {
			t.Fatalf("explain in a transaction:\n%s", h.text())
		}
	})
	t.Run("explain", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		h.openColl("local", "tobira_people")
		h.beginTx()
		h.send(ctrl('e'))
		h.send(tea.PasteMsg{Content: "select * from tobira_people"})
		h.send(palette.ExplainCmd{})
		h.pump(func() bool { return h.m.running == nil })
		if !strings.Contains(h.text(), "FAKE SCAN tobira_people") || h.m.tx == nil {
			t.Fatalf("explain in a transaction:\n%s", h.text())
		}
	})
}

func TestTxCommitFailureClearsAndSaysSo(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.beginTx()
	h.fake.FailNext("commit", driver.Wrap(driver.ErrConflict, "fake", "commit", &textErr{"lost for fake://u:hunter22@db"}))
	h.palette("tx commit")
	h.pump(func() bool { return h.m.tx == nil })
	f := h.flashText()
	if !strings.Contains(f, "commit failed: ") || !strings.Contains(f, "the transaction was rolled back") || strings.Contains(h.raw(), "hunter22") {
		t.Fatalf("flash = %q", f)
	}
}

// TestTxRunErrorMayAbort: an error inside the transaction says it may be
// aborted (the simpler rule of the two in the decisions) and keeps TX.
func TestTxRunErrorMayAbort(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.beginTx()
	h.runInEditor("select * from nowhere")
	if !strings.Contains(h.text(), "the transaction may be aborted; :tx rollback to continue") {
		t.Fatalf("no abort note:\n%s", h.text())
	}
	if h.m.tx == nil || !strings.Contains(h.statusLine(), "TX") {
		t.Fatal("the transaction was dropped")
	}
}

func TestTxQuitPrompt(t *testing.T) {
	for _, tt := range []struct {
		name    string
		key     string
		quit    bool
		deleted bool
	}{
		{"commit", "c", true, true},
		{"rollback", "r", true, false},
		{"cancel", "x", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newEditHarness(t, false, nil)
			h.openColl("local", "tobira_people")
			h.beginTx()
			h.runInEditor("delete from tobira_people where id = 5")
			h.keys("q")
			if h.quitted() || h.m.ask == nil {
				t.Fatal("q quit with an open transaction")
			}
			if !strings.Contains(h.text(), "transaction open on local: [c]ommit · [r]ollback · cancel") {
				t.Fatalf("no prompt:\n%s", h.text())
			}
			h.keys(tt.key)
			if tt.quit {
				h.pump(func() bool { return h.didQuit })
			} else if h.quitted() || h.m.tx == nil {
				t.Fatal("cancel did not keep the session and the transaction")
			}
			if gone := fakeRow(t, h.fake, 5) == nil; gone != tt.deleted {
				t.Fatalf("row 5 deleted = %v, want %v", gone, tt.deleted)
			}
		})
	}
	t.Run("ctrl+c and :q ask too", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		h.openColl("local", "tobira_people")
		h.beginTx()
		h.send(ctrl('c'))
		if h.quitted() || h.m.ask == nil {
			t.Fatal("ctrl+c quit with an open transaction")
		}
		h.keys("esc")
		h.palette("q")
		if h.quitted() || h.m.ask == nil {
			t.Fatal(":q quit with an open transaction")
		}
	})
}

// TestTxPromptGuardsConnectionChanges: switching the editor's connection,
// editing or deleting it and :reload ask first; cancel leaves everything.
func TestTxPromptGuardsConnectionChanges(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  tea.Msg
	}{
		{"open another connection", openMsg("ro", "tobira_people")},
		{"edit", palette.ConnEditCmd{Name: "local"}},
		{"delete", palette.ConnRmCmd{Name: "local"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newEditHarness(t, false, nil)
			h.openColl("local", "tobira_people")
			h.beginTx()
			h.send(tt.msg)
			if h.m.ask == nil || h.m.ask.kind != askTx {
				t.Fatalf("no transaction prompt:\n%s", h.text())
			}
			h.keys("esc")
			if h.m.tx == nil || h.m.cur.conn != "local" || h.m.form != nil || h.m.confirm != nil {
				t.Fatal("cancel did not keep things as they were")
			}
		})
	}
	t.Run("commit then switch", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		if _, err := h.mgr.Open(context.Background(), "ro"); err != nil {
			t.Fatal(err)
		}
		h.openColl("local", "tobira_people")
		h.beginTx()
		h.send(openMsg("ro", "tobira_people"))
		h.keys("c")
		h.pump(func() bool { return h.m.tx == nil && h.m.cur.conn == "ro" })
	})
}

// TestQuitPromptOrder: pending edits first, then the transaction, then the
// unsaved buffer.
func TestQuitPromptOrder(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.beginTx()
	h.col("name")
	h.editCell("Pending")
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select 1"})
	h.keys("esc")
	h.keys("q")
	if h.m.guardSt == nil || h.m.ask != nil {
		t.Fatal("pending edits were not asked about first")
	}
	h.keys("y")
	h.pump(func() bool { return h.m.ask != nil })
	if h.m.ask.kind != askTx {
		t.Fatalf("second prompt is %q, want the transaction", h.m.ask.title)
	}
	h.keys("r")
	h.pump(func() bool { return h.m.tx == nil && h.m.ask != nil })
	if h.m.ask.title != "discard the unsaved query? y/N" {
		t.Fatalf("third prompt = %q", h.m.ask.title)
	}
	h.keys("y")
	h.pump(func() bool { return h.didQuit })
}

func TestQuitAsksAboutAnUnsavedBuffer(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select * from tobira_people"})
	h.keys("esc")
	h.keys("q")
	if h.quitted() || h.m.ask == nil || h.m.ask.title != "discard the unsaved query? y/N" {
		t.Fatal("q quit with an unsaved buffer")
	}
	h.keys("n")
	if h.quitted() {
		t.Fatal("n quit")
	}
	// Once run, the buffer is not unsaved any more.
	h.send(ctrl('e'))
	h.runUntilDone(keyF5)
	h.keys("esc")
	h.keys("q")
	if !h.quitted() {
		t.Fatal("q asked about a buffer that was run")
	}
}

func TestQuitAsksAboutAnotherConnectionsBuffer(t *testing.T) {
	h := newEditHarness(t, false, nil)
	if _, err := h.mgr.Open(context.Background(), "ro"); err != nil {
		t.Fatal(err)
	}
	h.openColl("local", "tobira_people")
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select 2"})
	h.keys("esc")
	h.send(openMsg("ro", "tobira_people")) // the editor switches to ro's empty buffer
	h.pump(func() bool { return h.m.edConn == "ro" && h.m.cur.conn == "ro" })
	h.keys("q")
	if h.quitted() || h.m.ask == nil {
		t.Fatal("q quit with local's unsaved buffer")
	}
}

// stuckTxDriver's transactions hang in Commit until release is closed.
type stuckTxDriver struct {
	driver.Driver
	release chan struct{}
}

func (d stuckTxDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &stuckTxConn{Conn: c, Browser: c.(driver.Browser), Mutator: c.(driver.Mutator), release: d.release}, nil
}

type stuckTxConn struct {
	driver.Conn
	driver.Browser
	driver.Mutator
	release chan struct{}
}

func (c *stuckTxConn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.Transactor).Begin(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &stuckTx{Tx: tx, Mutator: tx.(driver.Mutator), release: c.release}, nil
}

type stuckTx struct {
	driver.Tx
	driver.Mutator
	release chan struct{}
}

func (t *stuckTx) Commit(context.Context) error {
	<-t.release
	return errors.New("released")
}

// TestTxStuckCommitQuits: a commit that hangs past 5s does not trap the
// user; the notice goes to stderr.
func TestTxStuckCommitQuits(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h := newEditHarness(t, false, func(d driver.Driver) driver.Driver { return stuckTxDriver{d, release} })
	h.openColl("local", "tobira_people")
	h.beginTx()
	h.keys("q")
	h.keys("c")
	if h.m.tx == nil || h.m.tx.busy != "commit" {
		t.Fatal("commit not in flight")
	}
	h.keys("q")
	if h.quitted() {
		t.Fatal("q quit right after the commit started")
	}
	h.send(ctrl('c'))
	if h.quitted() {
		t.Fatal("ctrl+c quit right after the commit started")
	}
	later := time.Now().Add(stuckAfter + time.Second)
	h.m.now = func() time.Time { return later }
	h.keys("q")
	if !h.quitted() {
		t.Fatal("q did not quit with a stuck commit")
	}
	if !strings.Contains(h.m.exitNotice, "quitting with a stuck commit") {
		t.Fatalf("exit notice = %q", h.m.exitNotice)
	}
}

func TestTxGoldens(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.beginTx()
	checkFrame(t, h, winW, winH)
	golden(t, "tx_status", h.text())
	h.keys("q")
	checkFrame(t, h, winW, winH)
	golden(t, "tx_prompt", h.text())
}

func TestPromptsTinyWindows(t *testing.T) {
	for _, sz := range [][2]int{{20, 5}, {1, 1}, {10, 2}, {59, 10}, {5, 40}, {200, 3}} {
		h := newEditHarness(t, false, nil)
		h.openColl("local", "tobira_people")
		h.beginTx()
		h.send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkFrame(t, h, sz[0], sz[1])
		h.send(palette.QuitCmd{})
		if h.m.ask == nil {
			t.Fatalf("%v: no prompt", sz)
		}
		checkFrame(t, h, sz[0], sz[1])
		h.keys("esc")
		h.m.askYN("overwrite /a/very/long/path/\x1b[2J/that/wraps.csv? y/N", "", nil, "")
		checkFrame(t, h, sz[0], sz[1])
	}
}

// blockTxDriver's transactions run "slow" queries that ignore cancellation
// until release is closed. Every Tx call counts how many calls are inside
// the Tx at once, so a test can prove nothing overlapped.
type blockTxDriver struct {
	driver.Driver
	p *blockProbe
}

type blockProbe struct {
	release chan struct{}
	entered chan struct{}
	inside  atomic.Int32
	overlap atomic.Bool
	mu      sync.Mutex
	log     []string
}

func newBlockProbe() *blockProbe {
	return &blockProbe{release: make(chan struct{}), entered: make(chan struct{}, 4)}
}

// enter records a call into the Tx and returns its exit.
func (p *blockProbe) enter(op string) func() {
	if p.inside.Add(1) > 1 {
		p.overlap.Store(true)
	}
	p.mu.Lock()
	p.log = append(p.log, op)
	p.mu.Unlock()
	return func() { p.inside.Add(-1) }
}

func (p *blockProbe) ops() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.log, ",")
}

func (d blockTxDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &blockTxConn{Conn: c, Browser: c.(driver.Browser), Mutator: c.(driver.Mutator), p: d.p}, nil
}

type blockTxConn struct {
	driver.Conn
	driver.Browser
	driver.Mutator
	p *blockProbe
}

func (c *blockTxConn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.Transactor).Begin(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &blockTx{Tx: tx, mu: tx.(driver.Mutator), p: c.p}, nil
}

type blockTx struct {
	driver.Tx
	mu driver.Mutator
	p  *blockProbe
}

func (t *blockTx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	defer t.p.enter("query")()
	if strings.Contains(q.Text, "slow") {
		select {
		case t.p.entered <- struct{}{}:
		default:
		}
		<-t.p.release // ignores ctx, like a driver slow to honour cancellation
		return nil, errors.New("released")
	}
	return t.Tx.Query(ctx, q)
}

func (t *blockTx) Commit(ctx context.Context) error {
	defer t.p.enter("commit")()
	return t.Tx.Commit(ctx)
}

func (t *blockTx) Rollback(ctx context.Context) error {
	defer t.p.enter("rollback")()
	return t.Tx.Rollback(ctx)
}

func (t *blockTx) Insert(ctx context.Context, ref driver.CollectionRef, v driver.Object) (driver.Object, error) {
	defer t.p.enter("insert")()
	return t.mu.Insert(ctx, ref, v)
}

func (t *blockTx) Update(ctx context.Context, ref driver.CollectionRef, k, v driver.Object) error {
	defer t.p.enter("update")()
	return t.mu.Update(ctx, ref, k, v)
}

func (t *blockTx) Delete(ctx context.Context, ref driver.CollectionRef, k driver.Object) error {
	defer t.p.enter("delete")()
	return t.mu.Delete(ctx, ref, k)
}

// startCancelledSlowRun begins a Tx, runs a slow statement in it and
// cancels the run with ctrl+c; the driver call keeps going.
func startCancelledSlowRun(t *testing.T, p *blockProbe) *harness {
	t.Helper()
	h := newEditHarness(t, false, func(d driver.Driver) driver.Driver { return blockTxDriver{d, p} })
	h.openColl("local", "tobira_people")
	h.beginTx()
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select slow"})
	h.send(keyF5)
	<-p.entered
	h.send(ctrl('c'))
	if h.m.running != nil {
		t.Fatal("ctrl+c did not cancel the run")
	}
	h.keys("esc")
	h.m.active = 0
	h.m.ed.SetValue("")
	return h
}

// watchTxWait reports each call that starts waiting for the Tx's users.
func watchTxWait(h *harness) <-chan struct{} {
	waiting := make(chan struct{}, 8)
	h.m.txWaitHook = func() { waiting <- struct{}{} }
	return waiting
}

// awaitTxWait blocks until a call is waiting for the Tx's users.
func awaitTxWait(t *testing.T, waiting <-chan struct{}) {
	t.Helper()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("no call started waiting for the transaction")
	}
}

// A commit waits for the cancelled statement before it reaches the Tx.
func TestTxCommitWaitsForACancelledRun(t *testing.T) {
	p := newBlockProbe()
	h := startCancelledSlowRun(t, p)
	waiting := watchTxWait(h)
	h.palette("tx commit")
	if h.m.tx == nil || h.m.tx.busy != "commit" {
		t.Fatal("commit not started")
	}
	awaitTxWait(t, waiting)
	// The commit is waiting, and the cancelled statement cannot return
	// before release: nothing may have reached the Tx.
	if strings.Contains(p.ops(), "commit") {
		t.Fatal("the commit reached the Tx while the cancelled statement was inside it")
	}
	close(p.release)
	h.pump(func() bool { return h.m.tx == nil })
	if got := p.ops(); got != "query,commit" {
		t.Fatalf("Tx calls = %s", got)
	}
	if p.overlap.Load() {
		t.Fatal("two calls were inside the Tx at once")
	}
	if f := h.flashText(); !strings.Contains(f, "committed") {
		t.Fatalf("flash = %q", f)
	}
}

// Runs and :apply wait the same way.
func TestTxRunAndApplyWaitForACancelledRun(t *testing.T) {
	p := newBlockProbe()
	h := startCancelledSlowRun(t, p)
	h.col("name")
	h.editCell("Waited")
	waiting := watchTxWait(h)
	h.palette("apply")
	awaitTxWait(t, waiting)
	if strings.Contains(p.ops(), "update") {
		t.Fatal(":apply reached the Tx while the cancelled statement was inside it")
	}
	close(p.release)
	h.pump(func() bool { return h.m.apply == nil })
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select * from tobira_people"})
	h.runUntilDone(keyF5)
	if got := p.ops(); got != "query,update,query" || p.overlap.Load() {
		t.Fatalf("Tx calls = %s, overlap %v", got, p.overlap.Load())
	}
}

// Past the bound the Tx is TX! and only rollback is allowed.
func TestTxStuckCancelledRunAllowsOnlyRollback(t *testing.T) {
	p := newBlockProbe()
	h := startCancelledSlowRun(t, p)
	h.col("name")
	h.editCell("No") // pending: :apply must be refused once TX!
	h.m.txWaitFor = 100 * time.Millisecond
	h.palette("tx commit")
	h.pump(func() bool { return h.m.tx != nil && h.m.tx.busy == "" })
	if !h.m.tx.stuck || !strings.Contains(h.statusLine(), "TX!") {
		t.Fatalf("not TX!: %q", h.statusLine())
	}
	if f := h.flashText(); !strings.Contains(f, "only :tx rollback is allowed") {
		t.Fatalf("flash = %q", f)
	}
	h.palette("tx commit")
	if f := h.flashText(); !strings.Contains(f, "only :tx rollback is allowed") {
		t.Fatalf("second commit: %q", f)
	}
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select * from tobira_people"})
	h.send(keyF5)
	if h.m.running != nil {
		t.Fatal("a run started on a stuck Tx")
	}
	h.keys("esc")
	h.send(palette.ApplyCmd{})
	if h.m.apply != nil || !strings.Contains(h.flashText(), "only :tx rollback is allowed") {
		t.Fatalf(":apply on a stuck Tx: %q", h.flashText())
	}
	if strings.Contains(p.ops(), "commit") || strings.Contains(p.ops(), "update") {
		t.Fatalf("a call reached the stuck Tx: %s", p.ops())
	}
	close(p.release)
	h.send(palette.DiscardCmd{})
	h.keys("y")
	h.m.txWaitFor = 0
	h.palette("tx rollback")
	h.pump(func() bool { return h.m.tx == nil })
	if got := p.ops(); got != "query,rollback" || p.overlap.Load() {
		t.Fatalf("Tx calls = %s, overlap %v", got, p.overlap.Load())
	}
}

// The exit close waits for the cancelled statement, and past the bound
// leaves the Tx to the connection close.
func TestTxExitCloseWaitsForACancelledRun(t *testing.T) {
	p := newBlockProbe()
	h := startCancelledSlowRun(t, p)
	h.m.txWaitFor = 100 * time.Millisecond
	h.m.closeTxAtExit()
	if strings.Contains(p.ops(), "rollback") {
		t.Fatal("the exit rollback reached the Tx while the cancelled statement was inside it")
	}
	close(p.release)

	p2 := newBlockProbe()
	h2 := startCancelledSlowRun(t, p2)
	go func() { time.Sleep(50 * time.Millisecond); close(p2.release) }()
	h2.m.closeTxAtExit()
	if got := p2.ops(); got != "query,rollback" || p2.overlap.Load() {
		t.Fatalf("Tx calls = %s, overlap %v", got, p2.overlap.Load())
	}
}

// The stuck-commit notice says how many pending edits it drops.
func TestTxStuckCommitNoticeCountsPendingEdits(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h := newEditHarness(t, false, func(d driver.Driver) driver.Driver { return stuckTxDriver{d, release} })
	h.openColl("local", "tobira_people")
	h.beginTx()
	h.col("name")
	h.editCell("Kept")
	h.palette("tx commit")
	later := time.Now().Add(stuckAfter + time.Second)
	h.m.now = func() time.Time { return later }
	h.keys("q")
	if !h.quitted() {
		t.Fatal("q did not quit")
	}
	if !strings.Contains(h.m.exitNotice, "; 1 pending edit dropped") {
		t.Fatalf("exit notice = %q", h.m.exitNotice)
	}
}

// :reload without a config file closes nothing, so it does not ask.
func TestTxReloadWithoutConfigFileDoesNotAsk(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.beginTx()
	h.send(palette.ReloadCmd{})
	if h.m.ask != nil {
		t.Fatal(":reload without a config file asked about the transaction")
	}
	if h.m.tx == nil {
		t.Fatal("the transaction was dropped")
	}
}

func TestTxReloadWithConfigFileAsks(t *testing.T) {
	h, _ := newConnHarness(t)
	h.openPeople()
	h.beginTx()
	h.send(palette.ReloadCmd{})
	if h.m.ask == nil || h.m.ask.kind != askTx {
		t.Fatal(":reload with a config file did not ask")
	}
}

// A read-only refusal never reached the database: no abort note.
func TestTxReadOnlyRefusalHasNoAbortNote(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.palette("readonly on")
	h.beginTx() // read-only, as the connection is now
	h.runInEditor("delete from tobira_people where id = 5")
	if !strings.Contains(h.text(), "is read-only; the statement was refused") {
		t.Fatalf("no refusal:\n%s", h.text())
	}
	if strings.Contains(h.text(), "may be aborted") {
		t.Fatalf("abort note on a read-only refusal:\n%s", h.text())
	}
}

// A call waiting for a cancelled statement still inside the Tx stops
// waiting when it is cancelled itself: an :apply cancelled while it waits
// ends at once as cancelled, not TX! once the bound passed.
func TestTxApplyCancelledWhileWaitingEndsAtOnce(t *testing.T) {
	p := newBlockProbe()
	t.Cleanup(func() { close(p.release) })
	h := startCancelledSlowRun(t, p)
	h.m.txWaitFor = time.Hour
	h.col("name")
	h.editCell("Waiting")
	h.palette("apply")
	if h.m.apply == nil {
		t.Fatal(":apply did not start")
	}
	h.send(ctrl('c'))
	h.pump(func() bool { return h.m.apply == nil })
	if h.m.tx == nil || h.m.tx.stuck {
		t.Fatalf("a cancelled wait made the Tx TX!: %q", h.statusLine())
	}
	if f := h.flashText(); !strings.Contains(f, "apply cancelled") {
		t.Fatalf("flash = %q", f)
	}
	if strings.Contains(p.ops(), "update") {
		t.Fatalf("the cancelled apply reached the Tx: %s", p.ops())
	}
}

func TestWaitUsersHonoursContext(t *testing.T) {
	blocked := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := waitUsers(ctx, []<-chan struct{}{blocked}, time.Hour, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitUsers = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("waitUsers took %v after its context was cancelled", d)
	}
	if err := waitUsers(context.Background(), []<-chan struct{}{blocked}, time.Millisecond, nil); !errors.Is(err, errTxStuck) {
		t.Fatalf("waitUsers past the bound = %v, want errTxStuck", err)
	}
	close(blocked)
	if err := waitUsers(context.Background(), []<-chan struct{}{blocked}, time.Hour, nil); err != nil {
		t.Fatalf("waitUsers after the users returned = %v", err)
	}
}
