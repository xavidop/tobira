package drivertest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// The defaults follow the People members, which they never change.
func TestProbeRowInsertDefaults(t *testing.T) {
	fx := Fixture{InsertDefaults: map[string]driver.Value{"values": driver.Vector([]float32{1, 0}), "a_extra": driver.Int(7)}}
	plain := Fixture{}.probeRow(1000, "Probe")
	got := fx.probeRow(1000, "Probe")
	if len(got) != len(plain)+2 {
		t.Fatalf("probe row = %v", got)
	}
	for i, m := range plain {
		if got[i].Key != m.Key || !EqualValue(got[i].Value, m.Value) {
			t.Errorf("member %d = %v, want %v", i, got[i], m)
		}
	}
	// Sorted by key, so the row is deterministic.
	if got[len(plain)].Key != "a_extra" || got[len(plain)+1].Key != "values" {
		t.Errorf("defaults = %v", got[len(plain):])
	}
	if err := (Fixture{InsertDefaults: map[string]driver.Value{"name": driver.String("x")}}).checkInsertDefaults(); err == nil {
		t.Error("a default naming a People column must be refused")
	}
}

// needsVector is a store that refuses an insert without an "embedding"
// member (and drops it before storing, since the Fake has no such
// column).
type needsVector struct{ driver.Conn }

func (c needsVector) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	var kept driver.Object
	found := false
	for _, m := range values {
		if m.Key == "embedding" {
			found = true
			continue
		}
		kept = append(kept, m)
	}
	if !found {
		return nil, driver.Wrap(driver.ErrSyntax, "needsvector", "insert", errors.New("a record needs its embedding"))
	}
	return c.Conn.(driver.Mutator).Insert(ctx, ref, kept)
}

func (c needsVector) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	return c.Conn.(driver.Mutator).Update(ctx, ref, key, changes)
}

func (c needsVector) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	return c.Conn.(driver.Mutator).Delete(ctx, ref, key)
}

func (c needsVector) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return c.Conn.(driver.Querier).Query(ctx, q)
}

// lostUpdate also ignores every Update.
type lostUpdate struct{ needsVector }

func (lostUpdate) Update(context.Context, driver.CollectionRef, driver.Object, driver.Object) error {
	return nil
}

func insertDefaultsConfig(wrap func(driver.Conn) driver.Conn, defaults map[string]driver.Value) Config {
	f := NewFake(WithPeople())
	d := brokenDriver{f: f, wrap: wrap}
	open := func(ro bool) func(testing.TB) driver.Conn {
		return func(tb testing.TB) driver.Conn {
			c, err := d.Open(context.Background(), driver.ConnConfig{ReadOnly: ro})
			if err != nil {
				tb.Fatal(err)
			}
			return c
		}
	}
	return Config{
		Driver: d, Open: open(false), OpenReadOnly: open(true), Strict: true,
		Fixture: Fixture{
			Namespace: "main", Collection: "tobira_people",
			SelectAll:      driver.Query{Text: "select * from tobira_people order by id"},
			Missing:        driver.Query{Text: "select * from nope"},
			Write:          driver.Query{Text: "delete from tobira_people where id = 1"},
			InsertDefaults: defaults,
		},
	}
}

var embedding = map[string]driver.Value{"embedding": driver.Vector([]float32{1, 0, 0, 0})}

// With InsertDefaults a store that needs an extra member passes the
// Mutator suite.
func TestInsertDefaultsMutator(t *testing.T) {
	testMutator(t, insertDefaultsConfig(func(c driver.Conn) driver.Conn { return needsVector{c} }, embedding))
}

func TestHelperInsertDefaults(t *testing.T) {
	switch os.Getenv("TOBIRA_DRIVERTEST_HELPER") {
	case "no-defaults":
		testMutator(t, insertDefaultsConfig(func(c driver.Conn) driver.Conn { return needsVector{c} }, nil))
	case "lost-update":
		testMutator(t, insertDefaultsConfig(func(c driver.Conn) driver.Conn { return lostUpdate{needsVector{c}} }, embedding))
	case "shadow-people":
		testMutator(t, insertDefaultsConfig(func(c driver.Conn) driver.Conn { return needsVector{c} },
			map[string]driver.Value{"embedding": driver.Vector([]float32{1}), "name": driver.String("Someone Else")}))
	default:
		t.Skip("helper process only")
	}
}

// InsertDefaults does not weaken the suite: without it the store fails,
// a lost update still fails with it, and a default may not replace a
// People value.
func TestInsertDefaultsKeepChecks(t *testing.T) {
	for variant, want := range map[string]string{
		"no-defaults":   "embedding",
		"lost-update":   "after Update",
		"shadow-people": "InsertDefaults",
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperInsertDefaults$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "TOBIRA_DRIVERTEST_HELPER="+variant)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Errorf("%s: the Mutator suite passed a wrong driver (err=%v):\n%s", variant, err, out)
			continue
		}
		if !strings.Contains(string(out), want) {
			t.Errorf("%s: failure output does not mention %q:\n%s", variant, want, out)
		}
	}
}
