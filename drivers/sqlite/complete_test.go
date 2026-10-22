package sqlite

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

func completer(t *testing.T, c driver.Conn) driver.Completer {
	t.Helper()
	comp, ok := driver.As[driver.Completer](c)
	if !ok {
		t.Fatal("conn is not a Completer")
	}
	return comp
}

func completeAt(t *testing.T, c driver.Conn, text string) []string {
	t.Helper()
	i := strings.IndexByte(text, '|')
	got, err := completer(t, c).Complete(context.Background(), driver.CompletionRequest{Text: text[:i] + text[i+1:], Cursor: i})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(got))
	for i, g := range got {
		out[i] = g.Label
	}
	return out
}

func TestInfoAdvertisesCompleter(t *testing.T) {
	if !slices.Contains(New().Info().Capabilities, driver.CapCompleter) {
		t.Fatalf("Info().Capabilities lacks completer: %v", New().Info().Capabilities)
	}
}

func TestComplete(t *testing.T) {
	for _, ro := range []bool{false, true} {
		c := openTemp(t, ro)
		if got := completeAt(t, c, "select * from tobira_|"); !slices.Equal(got, []string{"tobira_people"}) {
			t.Errorf("ro=%v collections = %v", ro, got)
		}
		if got := completeAt(t, c, "select p.na| from tobira_people p"); !slices.Equal(got, []string{"name"}) {
			t.Errorf("ro=%v alias fields = %v", ro, got)
		}
		if got := completeAt(t, c, "select * from |"); !slices.Contains(got, "main") || !slices.Contains(got, "tobira_people") {
			t.Errorf("ro=%v from = %v", ro, got)
		}
		if got := completeAt(t, c, "select * from main.tobira_p|"); !slices.Equal(got, []string{"tobira_people"}) {
			t.Errorf("ro=%v ns = %v", ro, got)
		}
		if got := completeAt(t, c, "select * from tobira_people where 'x|"); len(got) != 0 {
			t.Errorf("inside string = %v", got)
		}
	}
}

func TestCompleteCacheAndInvalidate(t *testing.T) {
	c := openTemp(t, false)
	if got := completeAt(t, c, "select * from extra|"); len(got) != 0 {
		t.Fatalf("before create = %v", got)
	}
	rs, err := c.Query(context.Background(), driver.Query{Text: "create table extra_things (a integer)"})
	if err != nil {
		t.Fatal(err)
	}
	_ = rs.Close()
	if got := completeAt(t, c, "select * from extra|"); len(got) != 0 {
		t.Fatalf("catalog must be cached for 60 s, got %v", got)
	}
	inv, ok := c.(sqlcomplete.Invalidator)
	if !ok {
		t.Fatal("conn is not an Invalidator")
	}
	inv.Invalidate()
	if got := completeAt(t, c, "select * from extra|"); !slices.Equal(got, []string{"extra_things"}) {
		t.Fatalf("after Invalidate = %v", got)
	}
}

func TestCompleteClosedAndCancelled(t *testing.T) {
	c := openTemp(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := completer(t, c).Complete(ctx, driver.CompletionRequest{Text: "select * from "}); err == nil {
		t.Error("cancelled context must fail")
	}
	_ = c.Close()
	_, err := completer(t, c).Complete(context.Background(), driver.CompletionRequest{Text: "select * from ", Cursor: 14})
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("closed conn: err = %v, want ErrConnection", err)
	}
}

// Completion reads the catalog through the read-only handle only: closing the
// writable handle must not affect it, and a database attached on the writable
// handle must stay invisible.
func TestCompleteUsesReadOnlyHandle(t *testing.T) {
	c := openTemp(t, false)
	rs, err := c.Query(context.Background(), driver.Query{Text: "attach database ':memory:' as scratch"})
	if err != nil {
		t.Fatal(err)
	}
	_ = rs.Close()
	cn := c.(*conn)
	if err := cn.rw.Close(); err != nil {
		t.Fatal(err)
	}
	got := completeAt(t, c, "select * from |")
	if !slices.Contains(got, "tobira_people") {
		t.Fatalf("completion must work with the writable handle closed: %v", got)
	}
	if slices.Contains(got, "scratch") {
		t.Fatalf("a database attached on the writable handle leaked into the catalog: %v", got)
	}
	if got := completeAt(t, c, "select p.na| from tobira_people p"); !slices.Equal(got, []string{"name"}) {
		t.Fatalf("fields = %v", got)
	}
}
