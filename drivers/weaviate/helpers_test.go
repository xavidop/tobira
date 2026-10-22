package weaviate

import (
	"testing"

	"github.com/xavidop/tobira/driver"
)

// newTestConn returns a conn to a closed local port, never pinged: any
// request fails with a connection error, so a test that sees another error
// proves the driver decided before the network.
func newTestConn(t *testing.T, cfg driver.ConnConfig) *conn {
	t.Helper()
	cfg.DSN = "weaviate://127.0.0.1:1"
	d, err := parseDSN(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := newConn(d, cfg, "1.40.2")
	t.Cleanup(func() { _ = c.Close() })
	return c
}
