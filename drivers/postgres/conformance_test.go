//go:build integration

package postgres

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// conformanceDB is the database the conformance kit runs against. It is
// separate from the shared one so the kit's writes can never disturb the
// driver's own tests.
const conformanceDB = "conformance"

// conformanceDSN creates and seeds the conformance database and returns its
// DSN. The database has the pgvector extension and the drivertest vector
// dataset (table vecs), so the kit runs the VectorSearcher checks.
func conformanceDSN(t *testing.T) string {
	t.Helper()
	return seededDB(t, conformanceDB)
}

// seededDB creates database name on the shared container, dropped on
// cleanup, seeds People, pgvector and the vector dataset, and returns its DSN.
func seededDB(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	dsn := newDatabase(t, name)
	seedConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seedConn.Close(ctx) }()
	for _, s := range append(drivertest.SQLSeed("postgres"), "CREATE EXTENSION vector") {
		if _, err := seedConn.Exec(ctx, s); err != nil {
			t.Fatalf("seed %.60s: %v", s, err)
		}
	}
	seedVectors(t, dsn, drivertest.VectorCollection, "vector")
	return dsn
}

// newDatabase creates an empty database name on the shared container,
// dropped on cleanup, and returns its DSN.
func newDatabase(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), testDSN)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func TestConformance(t *testing.T) {
	dsn := conformanceDSN(t)
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn, ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         open(false),
		OpenReadOnly: open(true),
		OpenBadAuth: func(ctx context.Context) error {
			c, err := New().Open(ctx, driver.ConnConfig{DSN: dsn, Password: driver.NewSecret("definitely-wrong")})
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		Strict:  true,
		Fixture: peopleFixture(),
	})
}

// peopleFixture is the conformance fixture of the People table in schema
// public, with the vector dataset and a LISTEN channel. Flavors without a
// capability simply never reach its part.
func peopleFixture() drivertest.Fixture {
	return drivertest.Fixture{
		AutoIncrementKey: drivertest.Bool(false), // id integer, no sequence
		Namespace:        "public",
		Collection:       "tobira_people",
		SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
		Missing:          driver.Query{Text: "select * from nope"},
		Write:            driver.Query{Text: "delete from tobira_people where id = 1"},
		// A read-only run takes one statement (several are a syntax error), so
		// the compound probe is a writable CTE.
		CompoundWrites: []driver.Query{
			{Text: "with d as (delete from tobira_people where id = 1 returning id) select * from d"},
		},
		Slow:       driver.Query{Text: "select pg_sleep(10)"},
		ParamQuery: driver.Query{Text: "select * from tobira_people where id = $1", Params: []driver.Value{driver.Int(7)}},
		TxWrite:    driver.Query{Text: "update tobira_people set name = 'tx' where id = 1"},
		CompleteAt: driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
		Watch:      &driver.WatchRequest{Channel: "tobira_conformance"},
		TxRestore:  driver.Query{Text: "update tobira_people set name = 'Ōtani Shōhei' where id = 1"},
		Vector:     drivertest.KindVectorFixture(driver.CollectionRef{Namespace: "public", Name: drivertest.VectorCollection}),
	}
}
