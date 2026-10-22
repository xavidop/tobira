package pinecone

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/xavidop/tobira/driver"
)

// newTestConn returns a host-mode conn (index "default", euclidean) to a
// closed local port, never pinged: any RPC fails with a connection error,
// so a test that sees another error proves the driver decided before the
// network.
func newTestConn(t *testing.T, readOnly bool) *conn {
	t.Helper()
	return newTestConnCfg(t, driver.ConnConfig{ReadOnly: readOnly})
}

func newTestConnCfg(t *testing.T, cfg driver.ConnConfig) *conn {
	t.Helper()
	if cfg.DSN == "" {
		cfg.DSN = "pinecone://?host=127.0.0.1:1&tls=false&metric=euclidean"
	}
	d, err := parseDSN(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newConn(d, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// jsonOf renders a filter with sorted keys.
func jsonOf(t *testing.T, s *structpb.Struct) string {
	t.Helper()
	b, err := json.Marshal(s.AsMap())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
