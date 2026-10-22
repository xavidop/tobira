package cassandra

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func eventsSchema() *driver.CollectionSchema {
	return &driver.CollectionSchema{
		Ref:  driver.CollectionRef{Namespace: "ks", Name: "events"},
		Type: driver.CollectionTable,
		Fields: []driver.Field{
			{Name: "tenant", Type: driver.TypeString}, {Name: "ts", Type: driver.TypeTime},
			{Name: "seq", Type: driver.TypeInt}, {Name: "kind", Type: driver.TypeString},
		},
		PrimaryKey: []string{"tenant", "ts", "seq"},
	}
}

func member(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }

func TestInsertStatement(t *testing.T) {
	sch := eventsSchema()
	vals := driver.Object{member("tenant", driver.String("a")), member("ts", driver.Int(0)), member("seq", driver.Int(1)), member("kind", driver.String("x"))}
	text, params, key, err := insertStatement(sch, vals)
	if err != nil {
		t.Fatal(err)
	}
	if want := `INSERT INTO "ks"."events" ("tenant", "ts", "seq", "kind") VALUES (?, ?, ?, ?) IF NOT EXISTS`; text != want {
		t.Errorf("text = %s, want %s", text, want)
	}
	if len(params) != 4 || len(key) != 3 || key[0].Key != "tenant" || key[2].Key != "seq" {
		t.Errorf("params %v key %v", params, key)
	}
	for _, bad := range []driver.Object{
		{member("tenant", driver.String("a")), member("ts", driver.Int(0))},                                                           // seq missing
		{member("tenant", driver.String("a")), member("ts", driver.Int(0)), member("seq", driver.Null())},                             // NULL key
		{member("tenant", driver.String("a")), member("ts", driver.Int(0)), member("seq", driver.Int(1)), member("x", driver.Int(1))}, // unknown column
		{member("tenant", driver.String("a")), member("tenant", driver.String("b")), member("ts", driver.Int(0)), member("seq", driver.Int(1))},
	} {
		if _, _, _, err := insertStatement(sch, bad); err == nil {
			t.Errorf("insertStatement(%v): accepted", bad)
		}
	}
}

func TestUpdateStatement(t *testing.T) {
	sch := eventsSchema()
	key := driver.Object{member("tenant", driver.String("a")), member("ts", driver.Int(0)), member("seq", driver.Int(1))}
	text, params, err := updateStatement(sch, key, driver.Object{member("kind", driver.String("y"))})
	if err != nil {
		t.Fatal(err)
	}
	if want := `UPDATE "ks"."events" SET "kind" = ? WHERE "tenant" = ? AND "ts" = ? AND "seq" = ? IF EXISTS`; text != want {
		t.Errorf("text = %s, want %s", text, want)
	}
	if len(params) != 4 {
		t.Errorf("params = %v", params)
	}
	for _, tc := range []struct {
		key, changes driver.Object
		kind         error
	}{
		{key, nil, driver.ErrSyntax},
		{key, driver.Object{member("seq", driver.Int(2))}, driver.ErrUnsupported},
		{key, driver.Object{member("nope", driver.Int(2))}, driver.ErrNotFound},
		{key[:2], driver.Object{member("kind", driver.String("y"))}, driver.ErrSyntax},
		{append(key[:3:3], member("kind", driver.String("x"))), driver.Object{member("kind", driver.String("y"))}, driver.ErrSyntax},
	} {
		if _, _, err := updateStatement(sch, tc.key, tc.changes); !errors.Is(err, tc.kind) {
			t.Errorf("updateStatement(%v, %v) = %v, want %v", tc.key, tc.changes, err, tc.kind)
		}
	}
}

func TestDeleteStatement(t *testing.T) {
	sch := eventsSchema()
	key := driver.Object{member("seq", driver.Int(1)), member("tenant", driver.String("a")), member("ts", driver.Int(0))}
	text, params, err := deleteStatement(sch, key)
	if err != nil {
		t.Fatal(err)
	}
	if want := `DELETE FROM "ks"."events" WHERE "tenant" = ? AND "ts" = ? AND "seq" = ? IF EXISTS`; text != want {
		t.Errorf("text = %s, want %s", text, want)
	}
	if len(params) != 3 {
		t.Errorf("params = %v", params)
	}
	if _, _, err := deleteStatement(sch, key[:1]); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("partial key: %v, want ErrSyntax", err)
	}
}

func TestMutateRefusesViews(t *testing.T) {
	sch := eventsSchema()
	sch.Type = driver.CollectionMaterializedView
	if err := checkWritable(sch); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("checkWritable(view) = %v, want ErrUnsupported", err)
	}
}

func TestMutateRefusesTupleColumns(t *testing.T) {
	sch := eventsSchema()
	sch.Fields = append(sch.Fields, driver.Field{Name: "tp", NativeType: "frozen<tuple<int, text>>", Type: driver.TypeList})
	if err := checkNoTuple(sch, "insert", driver.Object{member("tp", driver.List(driver.Int(1), driver.Null()))}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("a tuple column: %v, want ErrUnsupported", err)
	}
	if err := checkNoTuple(sch, "insert", driver.Object{member("kind", driver.String("x"))}); err != nil {
		t.Errorf("a plain column: %v", err)
	}
}

// Counter tables take neither INSERT nor lightweight transactions.
func TestMutateRefusesCounterTables(t *testing.T) {
	sch := &driver.CollectionSchema{
		Ref: driver.CollectionRef{Namespace: "ks", Name: "counters"}, Type: driver.CollectionTable,
		Fields:     []driver.Field{{Name: "id", NativeType: "int"}, {Name: "hits", NativeType: "counter"}},
		PrimaryKey: []string{"id"},
	}
	if err := checkWritable(sch); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("checkWritable(counter table) = %v, want ErrUnsupported", err)
	}
}
