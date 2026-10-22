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

// The Fake declares CapReadOnly by default, and WithoutCapabilities removes it
// from Info like any other capability.
func TestFakeDeclaresReadOnly(t *testing.T) {
	if !driver.HasCapability(NewFake().Driver().Info(), driver.CapReadOnly) {
		t.Fatal("the default Fake must declare CapReadOnly")
	}
	f := NewFake(WithPeople(), WithoutCapabilities(driver.CapReadOnly))
	if driver.HasCapability(f.Driver().Info(), driver.CapReadOnly) {
		t.Fatal("WithoutCapabilities(CapReadOnly) left it in Info")
	}
	// Removing the declaration does not remove the other capabilities.
	if !driver.HasCapability(f.Driver().Info(), driver.CapBrowser) {
		t.Fatal("WithoutCapabilities(CapReadOnly) removed Browser")
	}
}

// OnDescribe lets a test add what the Fake's Describe cannot infer: foreign
// keys, comments, defaults and DDL.
func TestFakeOnDescribe(t *testing.T) {
	f, c := openFake(t, driver.ConnConfig{})
	f.AddCollection("main", "orders", []driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "person_id", Type: driver.TypeInt}}, nil, "id")
	f.OnDescribe("main", "orders", func(s *driver.CollectionSchema) {
		s.Comment = "customer orders"
		s.ForeignKeys = []driver.ForeignKey{{Name: "orders_person", Fields: []string{"person_id"}, References: driver.CollectionRef{Namespace: "main", Name: "tobira_people"}, RefFields: []string{"id"}}}
	})
	s, err := c.Describe(context.Background(), driver.CollectionRef{Namespace: "main", Name: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Comment != "customer orders" || len(s.ForeignKeys) != 1 || s.ForeignKeys[0].References.Name != "tobira_people" {
		t.Fatalf("hook not applied: %+v", s)
	}
	p, err := c.Describe(context.Background(), driver.CollectionRef{Namespace: "main", Name: "tobira_people"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Comment != "" || len(p.ForeignKeys) != 0 {
		t.Fatalf("hook leaked to another collection: %+v", p)
	}
}

// leakyDriver declares CapReadOnly but ignores Query.ReadOnly: the guard
// subtest must catch it.
type leakyDriver struct{ f *Fake }

func (d leakyDriver) Info() driver.Info { return d.f.Driver().Info() }

func (d leakyDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.f.Driver().Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return leakyConn{c}, nil
}

type leakyConn struct{ driver.Conn }

func (c leakyConn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	q.ReadOnly = false
	return c.Conn.Query(ctx, q)
}

func guardConfig(d driver.Driver, f *Fake) Config {
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
			Namespace: "main", Collection: peopleCollection,
			SelectAll: driver.Query{Text: "select * from tobira_people order by id"},
			Missing:   driver.Query{Text: "select * from nope"},
			Write:     driver.Query{Text: "delete from tobira_people where id = 1"},
			TxWrite:   driver.Query{Text: "delete from tobira_people where id = 2"},
			CompoundWrites: []driver.Query{
				{Text: "select * from tobira_people; delete from tobira_people where id = 3"},
			},
		},
	}
}

// TestHelperLeakyReadOnly runs only as the child of TestReadOnlyGuardCatchesLeak.
func TestHelperLeakyReadOnly(t *testing.T) {
	if os.Getenv("TOBIRA_DRIVERTEST_HELPER") != "leaky-readonly" {
		t.Skip("helper process only")
	}
	f := NewFake(WithPeople())
	testReadOnlyGuard(t, guardConfig(leakyDriver{f}, f))
}

func TestReadOnlyGuardCatchesLeak(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLeakyReadOnly$", "-test.count=1")
	cmd.Env = append(os.Environ(), "TOBIRA_DRIVERTEST_HELPER=leaky-readonly")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the guard subtest passed a driver that ignores Query.ReadOnly (err=%v):\n%s", err, out)
	}
	if !strings.Contains(string(out), "ErrReadOnly") {
		t.Fatalf("unexpected failure output:\n%s", out)
	}
}

// The subtest passes for the honest Fake, and skips for one that does not
// declare CapReadOnly.
func TestReadOnlyGuardHonestAndUndeclared(t *testing.T) {
	f := NewFake(WithPeople())
	t.Run("declared", func(t *testing.T) { testReadOnlyGuard(t, guardConfig(f.Driver(), f)) })
	g := NewFake(WithPeople(), WithoutCapabilities(driver.CapReadOnly))
	skipped := false
	t.Run("undeclared", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		testReadOnlyGuard(t, guardConfig(leakyDriver{g}, g))
	})
	if !skipped {
		t.Error("the guard subtest ran for a driver that does not declare CapReadOnly")
	}
}

// leakyExplainDriver declares readonly and guards Query, but its Explain
// runs an ANALYZE of a write unguarded, as a careless "EXPLAIN ANALYZE " +
// text would.
type leakyExplainDriver struct{ f *Fake }

func (d leakyExplainDriver) Info() driver.Info { return d.f.Driver().Info() }

func (d leakyExplainDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.f.Driver().Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return leakyExplainConn{c}, nil
}

type leakyExplainConn struct{ driver.Conn }

func (c leakyExplainConn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	if opts.Analyze {
		q.ReadOnly = false
		rs, err := c.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		_ = rs.Close()
		return &driver.Plan{Format: "text", Text: "ran"}, nil
	}
	return c.Conn.(driver.Explainer).Explain(ctx, q, opts)
}

// leakyCompoundDriver guards a lone write but runs a compound one unguarded.
type leakyCompoundDriver struct{ f *Fake }

func (d leakyCompoundDriver) Info() driver.Info { return d.f.Driver().Info() }

func (d leakyCompoundDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.f.Driver().Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return leakyCompoundConn{c}, nil
}

type leakyCompoundConn struct{ driver.Conn }

func (c leakyCompoundConn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	if strings.Contains(q.Text, ";") {
		q.ReadOnly = false
	}
	return c.Conn.Query(ctx, q)
}

func TestHelperLeakyVariants(t *testing.T) {
	f := NewFake(WithPeople())
	switch os.Getenv("TOBIRA_DRIVERTEST_HELPER") {
	case "leaky-explain":
		testReadOnlyGuard(t, guardConfig(leakyExplainDriver{f}, f))
	case "leaky-compound":
		testReadOnlyGuard(t, guardConfig(leakyCompoundDriver{f}, f))
	default:
		t.Skip("helper process only")
	}
}

// The guard also probes Explain (an ANALYZE of a write must be refused and
// change nothing) and the fixture's compound writes.
func TestReadOnlyGuardCatchesExplainAndCompoundLeaks(t *testing.T) {
	for _, variant := range []string{"leaky-explain", "leaky-compound"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLeakyVariants$", "-test.count=1")
		cmd.Env = append(os.Environ(), "TOBIRA_DRIVERTEST_HELPER="+variant)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Errorf("%s: the guard passed (err=%v):\n%s", variant, err, out)
		}
	}
}
