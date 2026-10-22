package sdk_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// everyValue holds at least one value of every driver.Type, with the edge
// cases that a lossy encoding would break.
func everyValue() []driver.Value {
	cest := time.FixedZone("CEST", 2*3600)
	return []driver.Value{
		driver.Null(),
		driver.Bool(true),
		driver.Bool(false),
		driver.Int(math.MinInt64),
		driver.Int(math.MaxInt64),
		driver.Float(math.NaN()),
		driver.Float(math.Inf(-1)),
		driver.Float(math.Copysign(0, -1)),
		driver.Float(0.1),
		driver.Decimal("18446744073709551616.000001"),
		driver.String("héllo 世界"),
		driver.String("bad utf8 \xff\xfe"),
		driver.Bytes([]byte{0, 1, 0xff}),
		driver.Bytes(nil),
		driver.Time(time.Date(2026, 10, 21, 1, 2, 3, 456789012, time.UTC)),
		driver.Time(time.Date(2026, 7, 1, 9, 0, 0, 1, cest)),
		driver.Time(time.Time{}),
		driver.JSON([]byte(`{"a":[1,true,null]}`)),
		driver.JSON([]byte(`{not json`)),
		driver.Vector([]float32{1, -2.5, float32(math.NaN()), float32(math.Inf(1))}),
		driver.Vector(nil),
		driver.List(),
		driver.List(driver.Int(1), driver.Null(), driver.List(driver.String("nested"))),
		driver.ObjectValue(),
		driver.ObjectValue(
			driver.Member{Key: "z", Value: driver.Int(1)},
			driver.Member{Key: "a", Value: driver.ObjectValue(driver.Member{Key: "t", Value: driver.Time(time.Date(2001, 2, 3, 4, 5, 6, 7, cest))})},
			driver.Member{Key: "z", Value: driver.Bytes([]byte("dup key"))},
		),
		driver.Unknown("geometry", []byte{1, 2, 3}, "POINT(1 2)"),
		driver.Unknown("", nil, ""),
	}
}

func TestEveryValueTypeRoundTrips(t *testing.T) {
	vals := everyValue()
	seen := map[driver.Type]bool{}
	for _, v := range vals {
		seen[v.Type()] = true
	}
	for typ := driver.TypeNull; typ <= driver.TypeUnknown; typ++ {
		if !seen[typ] {
			t.Fatalf("everyValue lacks a %v", typ)
		}
	}

	// The query's parameters travel host -> plugin; the handler returns them
	// as one row and the stream carries them back plugin -> host.
	f := newFake()
	var gotParams []driver.Value
	f.OnQuery("echo", func(q driver.Query) (driver.ResultSet, error) {
		gotParams = q.Params
		cols := make([]driver.Column, len(q.Params))
		for i, p := range q.Params {
			cols[i] = driver.Column{Name: "c" + itoa(i), Type: p.Type()}
		}
		return driver.NewSliceResultSet(cols, []driver.Row{q.Params}, driver.Stats{RowsAffected: -1}), nil
	})
	c, _ := start(t, f.Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()
	stream, err := c.Query(context.Background(), &pb.QueryRequest{Conn: h, Query: query(t, driver.Query{Text: "echo", Params: vals})})
	if err != nil {
		t.Fatal(err)
	}
	res := collect(t, stream.Recv)

	sameRows(t, "params seen by the driver", []driver.Row{gotParams}, []driver.Row{vals})
	if len(res.rows) != 1 {
		t.Fatalf("%d rows, want 1", len(res.rows))
	}
	sameRows(t, "row seen by the host", res.rows, []driver.Row{vals})
	for i, col := range res.cols {
		if col.Type != vals[i].Type() {
			t.Fatalf("column %d type %v, want %v", i, col.Type, vals[i].Type())
		}
	}
}

func TestEveryValueTypeRoundTripsThroughMutator(t *testing.T) {
	// Each value is written with Insert and read back with Query, so the
	// Object encoding (keys, changes) is covered too.
	f := newFake()
	vals := everyValue()
	cols := []driver.Column{{Name: "id", Type: driver.TypeInt}}
	for i, v := range vals {
		cols = append(cols, driver.Column{Name: "v" + itoa(i), Type: v.Type()})
	}
	f.AddCollection("main", "every", cols, nil, "id")
	c, _ := start(t, f.Driver())
	h := open(t, c, &pb.ConnConfig{}).GetConn()

	row := driver.Object{{Key: "id", Value: driver.Int(1)}}
	for i, v := range vals {
		row = append(row, driver.Member{Key: "v" + itoa(i), Value: v})
	}
	every := driver.CollectionRef{Namespace: "main", Name: "every"}
	if _, err := c.Insert(context.Background(), &pb.InsertRequest{Conn: h, Ref: ref(every), Values: obj(t, row)}); err != nil {
		t.Fatal(err)
	}
	rows := queryRows(t, c, h, "select * from every")
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	sameRows(t, "inserted row", []driver.Row{rows[0][1:]}, []driver.Row{vals})
}
