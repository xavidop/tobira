package duckdb

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseDSN(t *testing.T) {
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	cases := []struct {
		dsn      string
		path     string
		memory   bool
		readOnly bool
		external bool
	}{
		{dsn: "duckdb:///tmp/x.db?external_access=1", path: "/tmp/x.db", external: true},
		{dsn: "duckdb:///tmp/x.db?external_access=true", path: "/tmp/x.db", external: true},
		{dsn: "duckdb:///tmp/x.db?external_access=0", path: "/tmp/x.db"},
		{dsn: "duckdb:///tmp/x.db", path: "/tmp/x.db"},
		{dsn: "duckdb:x.db", path: abs("x.db")},
		{dsn: "duckdb://x.db", path: abs("x.db")},
		{dsn: "duckdb://./data/x.duckdb", path: abs("data/x.duckdb")},
		{dsn: "/tmp/y.db", path: "/tmp/y.db"},
		{dsn: "duckdb:///tmp/x.db?access_mode=read_only", path: "/tmp/x.db", readOnly: true},
		{dsn: "duckdb:///tmp/x.db?access_mode=READ_ONLY", path: "/tmp/x.db", readOnly: true},
		{dsn: "duckdb:///tmp/x.db?access_mode=read_write", path: "/tmp/x.db"},
		{dsn: "duckdb://:memory:", memory: true},
		{dsn: "duckdb::memory:", memory: true},
		{dsn: ":memory:", memory: true},
	}
	for _, c := range cases {
		got, err := parseDSN(c.dsn)
		if err != nil {
			t.Errorf("parseDSN(%q): %v", c.dsn, err)
			continue
		}
		if got.memory != c.memory || got.readOnly != c.readOnly || got.external != c.external || (!c.memory && got.path != c.path) {
			t.Errorf("parseDSN(%q) = %+v, want path %q memory %v readOnly %v", c.dsn, got, c.path, c.memory, c.readOnly)
		}
	}
}

func TestParseDSNRefuses(t *testing.T) {
	cases := []struct {
		dsn  string
		kind error
	}{
		{"duckdb:", driver.ErrSyntax},
		{"duckdb://", driver.ErrSyntax},
		{"postgres://h/db", driver.ErrSyntax},
		// Configuration through the DSN would bypass the read-only layers.
		{"duckdb:///tmp/x.db?enable_external_access=true", driver.ErrUnsupported},
		{"duckdb:///tmp/x.db?threads=4", driver.ErrUnsupported},
		{"duckdb:///tmp/x.db?access_mode=automatic", driver.ErrUnsupported},
		{"duckdb:///tmp/x.db?access_mode=read_only&access_mode=read_write", driver.ErrUnsupported},
		{"duckdb:///tmp/x.db?access_mode=read_only;x", driver.ErrSyntax},
		// duckdb-go reads its DSN as a URL: a '#' would cut the
		// configuration off and '%' would be decoded.
		{"duckdb:///tmp/a#b.db", driver.ErrUnsupported},
		{"duckdb:///tmp/a%3fb.db", driver.ErrUnsupported},
		{"duckdb:///tmp/a\x00b.db", driver.ErrUnsupported},
		{"duckdb://:memory:?access_mode=read_only", driver.ErrUnsupported},
		// A read-only connection never allows external access.
		{"duckdb:///tmp/x.db?access_mode=read_only&external_access=1", driver.ErrUnsupported},
		{"duckdb:///tmp/x.db?external_access=yes", driver.ErrUnsupported},
	}
	for _, c := range cases {
		_, err := parseDSN(c.dsn)
		if !errors.Is(err, c.kind) {
			t.Errorf("parseDSN(%q) = %v, want %v", c.dsn, err, c.kind)
		}
		if err != nil && strings.Contains(err.Error(), "true") {
			t.Errorf("parseDSN(%q): the error echoes a parameter value: %v", c.dsn, err)
		}
	}
}
