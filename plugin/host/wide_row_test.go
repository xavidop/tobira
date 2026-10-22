package host_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
)

// A row too wide for one gRPC message (about 18 MiB of 20 cells under the
// 1 MiB value cap) ends the result with driver.ErrLimit once the rows
// before it are delivered, instead of a transport failure; the connection
// and the plugin stay usable.
func TestWideRowIsErrLimitThroughTheHost(t *testing.T) {
	before := goleak.IgnoreCurrent()
	f := setup(t, baseEnv(""))
	defer checkNoLeak(t, f, before)
	conn := open(t, f, driver.ConnConfig{})

	rs, err := conn.Query(context.Background(), driver.Query{Text: "!wide"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = rs.Close() }()
	row, ok, err := rs.Next(context.Background())
	if err != nil || !ok || len(row) != 20 {
		t.Fatalf("first row: %v, %v, %v", len(row), ok, err)
	}
	_, ok, err = rs.Next(context.Background())
	if ok || !errors.Is(err, driver.ErrLimit) || driver.KindOf(err) != driver.ErrLimit {
		t.Fatalf("wide row: ok=%v err=%v, want driver.ErrLimit", ok, err)
	}
	if msg := err.Error(); !strings.Contains(msg, "row 1") || !strings.Contains(msg, "message cap") {
		t.Errorf("wide row error %q does not name the row and the cap", msg)
	}
	_ = rs.Close()
	if err := conn.Ping(context.Background()); err != nil {
		t.Errorf("Ping after the wide row: %v", err)
	}
	if rows, err := collect(context.Background(), conn, "select * from tobira_people"); err != nil || len(rows) == 0 {
		t.Errorf("query after the wide row: %d rows, %v", len(rows), err)
	}
}
