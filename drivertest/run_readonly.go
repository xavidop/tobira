package drivertest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// testReadOnlyGuard probes a driver that declares driver.CapReadOnly:
//   - every fixture write (Write, TxWrite, CompoundWrites) sent with
//     Query.ReadOnly on a writable connection must fail with ErrReadOnly
//     and leave People unchanged, and the connection must still serve
//     reads afterwards;
//   - when the connection is an Explainer, explaining Fixture.Write with
//     Query.ReadOnly must change nothing, and an ANALYZE of it must fail
//     (ErrReadOnly, or ErrUnsupported when the driver has no ANALYZE).
//
// A driver that does not declare it is skipped, Strict or not: the
// declaration is what the AI assistant relies on.
func testReadOnlyGuard(t *testing.T, c Config) {
	if !driver.HasCapability(c.Driver.Info(), driver.CapReadOnly) {
		t.Skip("driver does not declare readonly")
	}
	type probe struct {
		name string
		q    driver.Query
	}
	probes := []probe{{"Write", c.Fixture.Write}, {"TxWrite", c.Fixture.TxWrite}}
	for i, q := range c.Fixture.CompoundWrites {
		probes = append(probes, probe{fmt.Sprintf("CompoundWrites[%d]", i), q})
	}
	unchanged := func(t *testing.T, conn driver.Conn, after string) {
		t.Helper()
		cols, rows, _, err := runQuery(ctxTimeout(t), conn, c.Fixture.SelectAll)
		if err != nil {
			t.Fatalf("reading People after %s: %v", after, err)
		}
		comparePeople(t, c.Fixture, "People after "+after, cols, rows)
	}
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			if p.q.Text == "" {
				t.Skipf("Fixture.%s is not set", p.name)
			}
			conn := openConn(t, c.Open)
			q := p.q
			q.ReadOnly = true
			_, _, _, err := runQuery(ctxTimeout(t), conn, q)
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("Fixture.%s with Query.ReadOnly (driver declares readonly): want error matching driver.ErrReadOnly, got %v", p.name, err)
			}
			// Read back on the same connection: the refusal must not have
			// poisoned it, and nothing may have changed.
			unchanged(t, conn, "a refused read-only "+p.name)
		})
	}
	t.Run("Explain", func(t *testing.T) {
		conn := openConn(t, c.Open)
		x, ok := driver.As[driver.Explainer](conn)
		if !ok {
			t.Skip("driver does not explain")
		}
		q := c.Fixture.Write
		q.ReadOnly = true
		_, _ = x.Explain(ctxTimeout(t), q, driver.ExplainOptions{}) // a plan or an error; it must not write
		unchanged(t, conn, "explaining Fixture.Write with Query.ReadOnly")
		_, err := x.Explain(ctxTimeout(t), q, driver.ExplainOptions{Analyze: true})
		if !errors.Is(err, driver.ErrReadOnly) && !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("EXPLAIN ANALYZE of Fixture.Write with Query.ReadOnly: want ErrReadOnly (or ErrUnsupported), got %v", err)
		}
		unchanged(t, conn, "EXPLAIN ANALYZE of Fixture.Write with Query.ReadOnly")
	})
}
