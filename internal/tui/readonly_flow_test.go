package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/tui/palette"
)

// browseSpyDriver records the ReadOnly flag of every Browse.
type browseSpyDriver struct {
	driver.Driver
	mu  *sync.Mutex
	ros *[]bool
}

func (d browseSpyDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &browseSpyConn{Conn: c, Mutator: c.(driver.Mutator), Transactor: c.(driver.Transactor), d: d}, nil
}

type browseSpyConn struct {
	driver.Conn
	driver.Mutator
	driver.Transactor
	d browseSpyDriver
}

func (c *browseSpyConn) Browse(ctx context.Context, ref driver.CollectionRef, o driver.BrowseOptions) (driver.ResultSet, error) {
	c.d.mu.Lock()
	*c.d.ros = append(*c.d.ros, o.ReadOnly)
	c.d.mu.Unlock()
	return c.Conn.(driver.Browser).Browse(ctx, ref, o)
}

func TestReadOnlyOnRefusesWrites(t *testing.T) {
	var mu sync.Mutex
	var ros []bool
	h := newEditHarness(t, false, func(d driver.Driver) driver.Driver { return browseSpyDriver{d, &mu, &ros} })
	h.openColl("local", "tobira_people")
	h.col("name")
	h.editCell("Pending") // made while writable: it stays, but :apply refuses
	if strings.Contains(h.statusLine(), "🔒") {
		t.Fatal("local starts read-only")
	}
	h.palette("readonly on")
	if !strings.Contains(h.statusLine(), "🔒") {
		t.Fatalf("no lock badge after :readonly on: %q", h.statusLine())
	}
	if h.m.pend.count() != 1 {
		t.Fatal("pending edits were dropped")
	}
	h.palette("apply")
	if f := h.flashText(); !strings.Contains(f, "🔒 local is read-only; the edit was refused") {
		t.Fatalf(":apply after on: %q", f)
	}
	if ops := mutationOps(h.fake); len(ops) != 0 {
		t.Fatalf("mutations = %v", ops)
	}
	// A run is sent read-only and the write refused.
	h.runInEditor("delete from tobira_people where id = 5")
	if q := h.lastQuery(); !q.Query.ReadOnly {
		t.Fatal("run after :readonly on was not read-only")
	}
	if !strings.Contains(h.text(), "🔒 local is read-only; the statement was refused") || fakeRow(t, h.fake, 5) == nil {
		t.Fatalf("the write was not refused:\n%s", h.text())
	}
	h.clearEditor()
	// The browse tab reads the flag at fetch time.
	h.m.active = 0 // back to the browse tab
	mu.Lock()
	browses := len(ros)
	mu.Unlock()
	h.send(palette.ReloadCmd{})
	h.keys("y") // the pending edit is dropped by the reload
	h.pump(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ros) > browses && !h.m.gr.Loading()
	})
	mu.Lock()
	last := ros[len(ros)-1]
	mu.Unlock()
	if !last {
		t.Fatal("browse after :readonly on was not read-only")
	}
	// A transaction begun now is read-only.
	h.beginTx()
	if !h.m.tx.ro {
		t.Fatal("transaction begun on a read-only connection is writable")
	}
	h.palette("tx rollback")
	h.pump(func() bool { return h.m.tx == nil })
	// Off clears the override: runs go back to ReadOnly=false.
	h.palette("readonly off")
	if strings.Contains(h.statusLine(), "🔒") {
		t.Fatalf("lock badge after :readonly off: %q", h.statusLine())
	}
	h.runInEditor("select * from tobira_people")
	if q := h.lastQuery(); q.Query.ReadOnly {
		t.Fatal("run after :readonly off was read-only")
	}
}

func TestReadOnlyOffRefusedWhenEnforced(t *testing.T) {
	t.Run("config", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		h.openColl("ro", "tobira_people")
		h.palette("readonly off")
		if f := h.flashText(); !strings.Contains(f, "read-only is enforced by config/flag") {
			t.Fatalf("flash = %q", f)
		}
		if !h.mgr.State("ro").ReadOnly {
			t.Fatal("off was not refused")
		}
	})
	t.Run("flag", func(t *testing.T) {
		h := newEditHarness(t, true, nil)
		h.openColl("local", "tobira_people")
		h.palette("readonly off")
		if f := h.flashText(); !strings.Contains(f, "read-only is enforced by config/flag") {
			t.Fatalf("flash = %q", f)
		}
		if !h.mgr.State("local").ReadOnly {
			t.Fatal("off was not refused")
		}
	})
	t.Run("allowed otherwise", func(t *testing.T) {
		h := newEditHarness(t, false, nil)
		h.openColl("local", "tobira_people")
		h.palette("readonly on")
		h.palette("readonly off")
		if h.mgr.State("local").ReadOnly {
			t.Fatal("off was refused on a writable connection")
		}
	})
}

func TestReadOnlyOnRefusedWithAWritableTx(t *testing.T) {
	h := newEditHarness(t, false, nil)
	h.openColl("local", "tobira_people")
	h.beginTx()
	h.palette("readonly on")
	if f := h.flashText(); !strings.Contains(f, "commit or roll back the open transaction first") {
		t.Fatalf("flash = %q", f)
	}
	if h.mgr.State("local").ReadOnly {
		t.Fatal(":readonly on applied with a writable transaction open")
	}
}

// slowRunDriver is stuckDriver whose Query blocks on a "slow" statement
// until release is closed (ignoring its context).
type slowRunDriver struct{ stuckDriver }

func (d slowRunDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.stuckDriver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &slowRunConn{c.(*stuckConn)}, nil
}

type slowRunConn struct{ *stuckConn }

func (c *slowRunConn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if strings.Contains(q.Text, "slow") {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		<-c.release
		return nil, errors.New("released")
	}
	return c.Conn.Query(ctx, q)
}

// :readonly on is refused while an :apply outside a transaction or an
// editor run is still writing on the connection; it is never cancelled
// implicitly.
func TestReadOnlyOnRefusedWhileWritesAreInFlight(t *testing.T) {
	const want = "wait for the running statement or apply to finish (ctrl+c cancels it)"
	newH := func(t *testing.T) (*harness, chan struct{}, chan struct{}) {
		release, entered := make(chan struct{}), make(chan struct{}, 1)
		h := newEditHarness(t, false, func(d driver.Driver) driver.Driver {
			return slowRunDriver{stuckDriver{d, release, entered}}
		})
		t.Cleanup(func() { close(release) }) // before the connections close
		h.openColl("local", "tobira_people")
		return h, release, entered
	}
	t.Run("apply", func(t *testing.T) {
		h, _, entered := newH(t)
		h.col("name")
		h.editCell("Writing")
		h.send(palette.ApplyCmd{})
		h.awaitEntered(entered)
		h.palette("readonly on")
		if f := h.flashText(); !strings.Contains(f, want) {
			t.Fatalf("flash = %q", f)
		}
		if h.mgr.State("local").ReadOnly || h.m.apply == nil || h.m.apply.cancelled {
			t.Fatalf("read-only %v, apply %+v", h.mgr.State("local").ReadOnly, h.m.apply)
		}
	})
	t.Run("run", func(t *testing.T) {
		h, _, entered := newH(t)
		h.send(ctrl('e'))
		h.send(tea.PasteMsg{Content: "update slow"})
		h.send(keyF5)
		h.awaitEntered(entered)
		h.keys("esc")
		h.palette("readonly on")
		if f := h.flashText(); !strings.Contains(f, want) {
			t.Fatalf("flash = %q", f)
		}
		if h.mgr.State("local").ReadOnly || h.m.running == nil {
			t.Fatal("the switch went through or cancelled the run")
		}
	})
	t.Run("other connection", func(t *testing.T) {
		h, _, entered := newH(t)
		h.col("name")
		h.editCell("Writing")
		h.send(palette.ApplyCmd{})
		h.awaitEntered(entered)
		h.palette("connect ro")
		h.palette("readonly off")
		if f := h.flashText(); strings.Contains(f, want) {
			t.Fatalf("a write on local held :readonly on another connection: %q", f)
		}
	})
}

// awaitEntered feeds command results until the write reached the driver.
func (h *harness) awaitEntered(entered <-chan struct{}) {
	h.t.Helper()
	in := false
	h.pump(func() bool {
		select {
		case <-entered:
			in = true
		default:
		}
		return in
	})
}
