package drivertest_test

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// newConformanceFake returns a Fake seeded with People, the per-kind
// fixtures (documents, key-value, vectors, time series, wide-column,
// search) and the collections the graph fixture points at. Restoring the
// row that TxWrite deletes is scripted through OnQuery, because the Fake's
// query language cannot insert.
func newConformanceFake(opts ...drivertest.FakeOption) (*drivertest.Fake, drivertest.Fixture) {
	f := drivertest.NewFake(append([]drivertest.FakeOption{
		drivertest.WithPeople(), drivertest.WithDocuments(), drivertest.WithKeyValues(),
		drivertest.WithVectors(), drivertest.WithTimeSeries(), drivertest.WithWideColumns(), drivertest.WithSearch(),
	}, opts...)...)
	f.AddCollection("main", "nodes",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "label", Type: driver.TypeString}},
		[]driver.Row{{driver.Int(1), driver.String("Person")}, {driver.Int(2), driver.String("Person")}}, "id")
	f.AddCollection("main", "edges",
		[]driver.Column{{Name: "id", Type: driver.TypeInt}, {Name: "from", Type: driver.TypeInt}, {Name: "to", Type: driver.TypeInt}, {Name: "type", Type: driver.TypeString}},
		[]driver.Row{{driver.Int(1), driver.Int(1), driver.Int(2), driver.String("KNOWS")}}, "id")

	const restore = "restore person 2"
	f.OnQuery(restore, func(driver.Query) (driver.ResultSet, error) {
		c, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
		if err != nil {
			return nil, err
		}
		defer func() { _ = c.Close() }()
		var row driver.Object
		for i, col := range drivertest.People.Columns {
			row = append(row, driver.Member{Key: col.Name, Value: drivertest.People.Rows[1][i]})
		}
		_, err = c.(driver.Mutator).Insert(context.Background(), driver.CollectionRef{Namespace: "main", Name: "tobira_people"}, row)
		if err != nil {
			return nil, err
		}
		return driver.NewSliceResultSet(nil, nil, driver.Stats{}), nil
	})

	people := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	return f, drivertest.Fixture{
		Namespace:  "main",
		Collection: "tobira_people",
		SelectAll:  driver.Query{Text: "select * from tobira_people order by id"},
		Missing:    driver.Query{Text: "select * from nope"},
		Write:      driver.Query{Text: "delete from tobira_people where id = 1"},
		CompoundWrites: []driver.Query{
			{Text: "select * from tobira_people; delete from tobira_people where id = 3"},
		},
		AutoIncrementKey: drivertest.Bool(true), // a missing single int key is assigned max+1
		Slow:             driver.Query{Text: "sleep 10s"},
		ParamQuery:       driver.Query{Text: "select * from tobira_people where id = $1", Params: []driver.Value{driver.Int(7)}},
		TxWrite:          driver.Query{Text: "delete from tobira_people where id = 2"},
		TxRestore:        driver.Query{Text: restore},
		CompleteAt:       driver.CompletionRequest{Text: "select * from tobira_", Cursor: 21},
		Watch:            &driver.WatchRequest{Collection: &people},
		Vector:           drivertest.KindVectorFixture(driver.CollectionRef{Namespace: "main", Name: drivertest.VectorCollection}),
		TimeSeries:       drivertest.KindTimeSeriesFixture(driver.CollectionRef{Namespace: "main", Name: drivertest.TimeSeriesCollection}),
		WideColumn: drivertest.KindWideColumnFixture(driver.CollectionRef{Namespace: "main", Name: drivertest.WideColumnCollection},
			driver.Query{Text: "select * from " + drivertest.WideColumnCollection + " where tenant = '" + drivertest.WideColumnPartition + "'"}),
		Search: drivertest.KindSearchFixture(driver.CollectionRef{Namespace: "main", Name: drivertest.SearchCollection},
			driver.Query{Text: "search " + drivertest.SearchCollection + " for '" + drivertest.SearchTerm + "'"}),
		Graph: &drivertest.GraphFixture{
			Query:    driver.GraphQuery{Start: driver.Int(1), Depth: 1},
			MinNodes: 2,
		},
	}
}

func openFunc(f *drivertest.Fake, cfg driver.ConnConfig) func(testing.TB) driver.Conn {
	return func(tb testing.TB) driver.Conn {
		tb.Helper()
		c, err := f.Driver().Open(context.Background(), cfg)
		if err != nil {
			tb.Fatalf("open fake: %v", err)
		}
		return c
	}
}

func TestFakeConformance(t *testing.T) {
	f, fx := newConformanceFake()
	drivertest.Run(t, drivertest.Config{
		Driver:       f.Driver(),
		Open:         openFunc(f, driver.ConnConfig{}),
		OpenReadOnly: openFunc(f, driver.ConnConfig{ReadOnly: true}),
		Fixture:      fx,
		Strict:       true,
	})
}

// TestFakeConformanceMinimal runs the suite against a Fake with every
// capability removed: the capability subtests must skip, not fail.
func TestFakeConformanceMinimal(t *testing.T) {
	f, fx := newConformanceFake(drivertest.WithoutCapabilities(
		driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor,
		driver.CapVectorSearcher, driver.CapTimeSeriesReader, driver.CapGraphReader,
		driver.CapCompleter, driver.CapExporter, driver.CapWatcher, driver.CapReadOnly))
	drivertest.Run(t, drivertest.Config{
		Driver:       f.Driver(),
		Open:         openFunc(f, driver.ConnConfig{}),
		OpenReadOnly: openFunc(f, driver.ConnConfig{ReadOnly: true}),
		Fixture:      fx,
	})
}
