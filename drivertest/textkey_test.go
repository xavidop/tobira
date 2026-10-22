package drivertest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// textIDs returns People's id as text, formatted by format, from Query and
// Browse: what a store whose keys must be strings (Cosmos DB) returns.
type textIDs struct {
	driver.Conn
	format string
}

func (c textIDs) convert(rs driver.ResultSet, err error) (driver.ResultSet, error) {
	if err != nil {
		return nil, err
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		return nil, err
	}
	for i, col := range cols {
		if col.Name != "id" {
			continue
		}
		cols[i].Type = driver.TypeString
		for _, r := range rows {
			if n, ok := r[i].AsInt(); ok {
				r[i] = driver.String(fmt.Sprintf(c.format, n))
			}
		}
	}
	return driver.NewSliceResultSet(cols, rows, st), nil
}

func (c textIDs) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return c.convert(c.Conn.Query(ctx, q))
}

func (c textIDs) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	return c.convert(c.Conn.(driver.Browser).Browse(ctx, ref, opts))
}

func TestHelperTextIDs(t *testing.T) {
	format := os.Getenv("TOBIRA_DRIVERTEST_TEXTID")
	if format == "" {
		t.Skip("helper process only")
	}
	f := NewFake(WithPeople())
	open := func(tb testing.TB) driver.Conn {
		c, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
		if err != nil {
			tb.Fatal(err)
		}
		return textIDs{Conn: c, format: format}
	}
	c := Config{Driver: f.Driver(), Open: open, OpenReadOnly: open, Strict: true, Fixture: Fixture{
		Namespace: "main", Collection: "tobira_people",
		SelectAll:     driver.Query{Text: "select * from tobira_people order by id"},
		Missing:       driver.Query{Text: "select * from nope"},
		Write:         driver.Query{Text: "delete from tobira_people where id = 1"},
		TypeOverrides: map[string]driver.Type{"id": driver.TypeString},
	}}
	for _, s := range []subtest{{"QueryRead", testQueryRead}, {"Browser", testBrowser}} {
		t.Run(s.name, func(t *testing.T) { s.fn(t, c) })
	}
}

// Under the String id override, exact decimal text passes the read and
// Browser checks, and padded text fails them.
func TestStringIDOverrideEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		format string
		pass   bool
	}{{"%d", true}, {"%03d", false}} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperTextIDs$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "TOBIRA_DRIVERTEST_TEXTID="+tc.format)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		switch {
		case tc.pass && err != nil:
			t.Errorf("ids as %q: the checks failed a correct driver: %v\n%s", tc.format, err, out)
		case !tc.pass && !errors.As(err, &exit):
			t.Errorf("ids as %q: the checks passed malformed ids (err=%v):\n%s", tc.format, err, out)
		case !tc.pass && (!strings.Contains(string(out), "--- FAIL: TestHelperTextIDs/QueryRead") || !strings.Contains(string(out), "--- FAIL: TestHelperTextIDs/Browser")):
			t.Errorf("ids as %q: want both QueryRead and Browser to fail:\n%s", tc.format, out)
		}
	}
}
