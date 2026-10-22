package chroma

import (
	"testing"

	"github.com/xavidop/tobira/driver"
)

// newTestConn returns a conn to a closed local port, never opened: any
// request fails with a connection error, so a test that sees another error
// proves the driver decided before the network.
func newTestConn(t *testing.T, readOnly bool) *conn {
	t.Helper()
	return newTestConnCfg(t, driver.ConnConfig{ReadOnly: readOnly})
}

func newTestConnCfg(t *testing.T, cfg driver.ConnConfig) *conn {
	t.Helper()
	cfg.DSN = "chroma://127.0.0.1:1"
	d, err := parseDSN(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := newConn(d, cfg)
	t.Cleanup(func() { _ = c.Close() })
	return c
}
