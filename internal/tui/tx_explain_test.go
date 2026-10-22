package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/tui/palette"
)

// explainBlockDriver is blockTxDriver whose Conn and Tx are Explainers; the
// Tx's Explain is recorded in the probe like every other Tx call.
type explainBlockDriver struct{ blockTxDriver }

func (d explainBlockDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.blockTxDriver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	bc := c.(*blockTxConn)
	return &explainBlockConn{blockTxConn: bc, ex: bc.Conn.(driver.Explainer)}, nil
}

type explainBlockConn struct {
	*blockTxConn
	ex driver.Explainer
}

func (c *explainBlockConn) Explain(ctx context.Context, q driver.Query, o driver.ExplainOptions) (*driver.Plan, error) {
	return c.ex.Explain(ctx, q, o)
}

func (c *explainBlockConn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.blockTxConn.Begin(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &explainBlockTx{tx.(*blockTx)}, nil
}

type explainBlockTx struct{ *blockTx }

func (t *explainBlockTx) Explain(ctx context.Context, q driver.Query, o driver.ExplainOptions) (*driver.Plan, error) {
	defer t.p.enter("explain")()
	return t.Tx.(driver.Explainer).Explain(ctx, q, o)
}

func newExplainTxHarness(t *testing.T, p *blockProbe) *harness {
	t.Helper()
	h := newEditHarness(t, false, func(d driver.Driver) driver.Driver { return explainBlockDriver{blockTxDriver{d, p}} })
	h.openColl("local", "tobira_people")
	h.beginTx()
	return h
}

// :explain inside :tx asks the Tx for the plan.
func TestTxExplainUsesTheTx(t *testing.T) {
	p := newBlockProbe()
	h := newExplainTxHarness(t, p)
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select * from tobira_people"})
	h.send(palette.ExplainCmd{})
	h.pump(func() bool { return h.m.running == nil })
	if !strings.Contains(h.text(), "FAKE SCAN tobira_people") {
		t.Fatalf("explain in a transaction:\n%s", h.text())
	}
	if got := p.ops(); got != "explain" {
		t.Fatalf("Tx calls = %q, want the explain to go to the Tx", got)
	}
}

// An explain inside the Tx waits for a cancelled run still inside it,
// like a run does; past the bound the Tx is TX! and the explain never ran.
func TestTxExplainWaitsForACancelledRun(t *testing.T) {
	p := newBlockProbe()
	h := newExplainTxHarness(t, p)
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: "select slow"})
	h.send(keyF5)
	<-p.entered
	h.send(ctrl('c'))
	if h.m.running != nil {
		t.Fatal("ctrl+c did not cancel the run")
	}
	h.m.txWaitFor = 50 * time.Millisecond
	h.m.ed.SetValue("select * from tobira_people")
	h.send(palette.ExplainCmd{})
	if h.m.running == nil {
		t.Fatal("the explain did not start")
	}
	h.pump(func() bool { return h.m.running == nil })
	if strings.Contains(p.ops(), "explain") || p.overlap.Load() {
		t.Fatalf("the explain reached the Tx while the cancelled statement was inside it: %s", p.ops())
	}
	if h.m.tx == nil || !h.m.tx.stuck {
		t.Fatalf("not TX! after the bound: %q", h.statusLine())
	}
	close(p.release)
}
