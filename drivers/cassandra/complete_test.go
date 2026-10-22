package cassandra

import (
	"context"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

type fakeCatalog struct{}

func (fakeCatalog) Namespaces(context.Context) ([]string, error) {
	return []string{"shop", "Other"}, nil
}
func (fakeCatalog) Collections(_ context.Context, ns string) ([]string, error) {
	return []string{"orders", "tobira_people", "MixedCase"}, nil
}
func (fakeCatalog) Fields(_ context.Context, ns, coll string) ([]string, error) {
	return []string{"id", "status"}, nil
}

func labels(cs []driver.Completion) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Label
	}
	return out
}

func TestCompleteCQL(t *testing.T) {
	run := func(text string) []driver.Completion {
		t.Helper()
		got, err := sqlcomplete.Complete(context.Background(), fakeCatalog{}, cqlDialect(), driver.CompletionRequest{Text: text, Cursor: len(text), Namespace: "shop"})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := labels(run("SELECT * FROM ")); !slices.Contains(got, "tobira_people") {
		t.Errorf("after FROM: %q, want the tables", got)
	}
	if got := labels(run("SELECT * FROM orders WHERE ")); !slices.Contains(got, "status") {
		t.Errorf("after WHERE: %q, want the columns", got)
	}
	for _, kw := range []string{"ALLOW", "FILTERING", "TOKEN", "WRITETIME"} {
		prefix := kw[:3]
		if got := labels(run("SELECT * FROM orders " + prefix)); !slices.Contains(got, kw) {
			t.Errorf("keywords for %q: %q lacks %s", prefix, got, kw)
		}
	}
	// A mixed-case name must be quoted: CQL folds unquoted names to lower case.
	for _, c := range run("SELECT * FROM Mix") {
		if c.Label == "MixedCase" && c.Insert != `"MixedCase"` {
			t.Errorf("MixedCase inserts %q, want it quoted", c.Insert)
		}
	}
	// Nothing is offered inside a $$ string.
	if got := run("SELECT * FROM t WHERE s = $$FR"); len(got) != 0 {
		t.Errorf("inside $$: %q, want nothing", labels(got))
	}
}
