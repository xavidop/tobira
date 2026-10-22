//go:build integration

package mongodb

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func ctx30(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func collect(t *testing.T, c driver.Querier, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	ctx := ctx30(t)
	rs, err := c.Query(ctx, q)
	if err != nil {
		t.Fatalf("Query(%s): %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatalf("Query(%s) rows: %v", q.Text, err)
	}
	return cols, rows, st
}

func colNames(cols []driver.Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

func cell(t *testing.T, cols []driver.Column, row driver.Row, name string) driver.Value {
	t.Helper()
	i := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("no column %q in %v", name, colNames(cols))
	}
	return row[i]
}

func TestIntegrationQueryCommands(t *testing.T) {
	c := openTest(t, testDB, false)

	t.Run("find", func(t *testing.T) {
		cols, rows, st := collect(t, c, driver.Query{Text: `{"find":"tobira_people","filter":{"active":true},"sort":{"_id":1},"projection":{"tags":0}}`})
		if len(rows) != 38 || st.RowsAffected != -1 {
			t.Fatalf("got %d rows (affected %d), want 38", len(rows), st.RowsAffected)
		}
		if got := colNames(cols); got[0] != "_id" || slices.Contains(got, "tags") {
			t.Errorf("columns %v", got)
		}
		if cols[0].Source == nil || cols[0].Source.Name != "tobira_people" || cols[0].Source.Namespace != testDB || !cols[0].Key {
			t.Errorf("_id column %+v: want Source tobira.tobira_people and Key", cols[0])
		}
		if v := cell(t, cols, rows[0], "score"); v.Type() != driver.TypeDecimal || v.String() != "12.50" {
			t.Errorf("score = %v (%s)", v, v.Type())
		}
		if v := cell(t, cols, rows[0], "created_at"); v.Type() != driver.TypeTime {
			t.Errorf("created_at = %v (%s)", v, v.Type())
		}
	})
	t.Run("aggregate", func(t *testing.T) {
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"aggregate":"tobira_people","pipeline":[{"$group":{"_id":"$active","n":{"$sum":1}}},{"$sort":{"_id":1}}]}`})
		if len(rows) != 2 || cell(t, cols, rows[0], "n").String() != "19" || cell(t, cols, rows[1], "n").String() != "38" {
			t.Errorf("rows %v", rows)
		}
		if cols[0].Source != nil {
			t.Error("an aggregate's columns name no source")
		}
	})
	t.Run("count", func(t *testing.T) {
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"count":"tobira_people"}`})
		if len(rows) != 1 || cell(t, cols, rows[0], "n").String() != "57" || slices.Contains(colNames(cols), "ok") {
			t.Errorf("cols %v rows %v", colNames(cols), rows)
		}
	})
	t.Run("distinct", func(t *testing.T) {
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"distinct":"tobira_people","key":"active"}`})
		if len(rows) != 2 || colNames(cols)[0] != "active" {
			t.Errorf("cols %v rows %v", colNames(cols), rows)
		}
	})
	t.Run("listIndexes and dbStats", func(t *testing.T) {
		_, rows, _ := collect(t, c, driver.Query{Text: `{"listIndexes":"tobira_people"}`})
		if len(rows) != 4 {
			t.Errorf("listIndexes: %d rows, want 4", len(rows))
		}
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"dbStats":1}`})
		if len(rows) != 1 || cell(t, cols, rows[0], "db").String() != testDB {
			t.Errorf("dbStats: %v", rows)
		}
	})
	t.Run("namespace", func(t *testing.T) {
		cols, rows, _ := collect(t, c, driver.Query{Text: `{"listCollections":1,"nameOnly":true}`, Namespace: "tobira_other"})
		if len(rows) != 1 || cell(t, cols, rows[0], "name").String() != "things" {
			t.Errorf("rows %v", rows)
		}
	})
	t.Run("limit", func(t *testing.T) {
		_, rows, st := collect(t, c, driver.Query{Text: `{"find":"tobira_big","sort":{"_id":1}}`, Limit: 5})
		if len(rows) != 5 || !st.Truncated {
			t.Errorf("%d rows truncated %v", len(rows), st.Truncated)
		}
	})
	t.Run("fields after the prefetch window are reported", func(t *testing.T) {
		cols, rows, st := collect(t, c, driver.Query{Text: `{"find":"tobira_big","sort":{"_id":1}}`})
		if len(rows) != 1500 || slices.Contains(colNames(cols), "late") {
			t.Fatalf("%d rows, columns %v", len(rows), colNames(cols))
		}
		if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "500 fields") {
			t.Errorf("warnings %q", st.Warnings)
		}
	})
	t.Run("write commands report rows affected", func(t *testing.T) {
		_, rows, st := collect(t, c, driver.Query{Text: `{"insert":"scratch","documents":[{"_id":1},{"_id":2}]}`})
		if st.RowsAffected != 2 || len(rows) != 1 {
			t.Errorf("affected %d rows %v", st.RowsAffected, rows)
		}
		_, _, st = collect(t, c, driver.Query{Text: `{"delete":"scratch","deletes":[{"q":{},"limit":0}]}`})
		if st.RowsAffected != 2 {
			t.Errorf("delete affected %d", st.RowsAffected)
		}
	})
	t.Run("params are unsupported", func(t *testing.T) {
		_, err := c.Query(ctx30(t), driver.Query{Text: `{"find":"tobira_people"}`, Params: []driver.Value{driver.Int(1)}})
		if !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("want ErrUnsupported, got %v", err)
		}
	})
}

func TestIntegrationErrors(t *testing.T) {
	c := openTest(t, testDB, false)
	for _, tc := range []struct {
		name, text string
		kind       error
		code       string
	}{
		{"missing namespace", `{"listIndexes":"nope"}`, driver.ErrNotFound, "26"},
		{"unknown command", `{"noSuchCommand":1}`, driver.ErrSyntax, "59"},
		{"bad stage", `{"aggregate":"tobira_people","pipeline":[{"$nope":{}}]}`, driver.ErrSyntax, "40324"},
		{"duplicate key", `{"insert":"tobira_people","documents":[{"_id":1}]}`, driver.ErrConstraint, "11000"},
		{"invalid json", `{"find":`, driver.ErrSyntax, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Query(ctx30(t), driver.Query{Text: tc.text})
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
			var de *driver.Error
			if !errors.As(err, &de) || de.Code != tc.code || de.Driver != Name {
				t.Errorf("driver.Error %#v, want code %q", de, tc.code)
			}
		})
	}
	t.Run("bad password", func(t *testing.T) {
		_, err := New().Open(ctx30(t), driver.ConnConfig{DSN: dsnFor(testDB, "wrong-Secret-xyz")})
		if !errors.Is(err, driver.ErrAuth) {
			t.Fatalf("want ErrAuth, got %v", err)
		}
		if strings.Contains(err.Error(), "wrong-Secret-xyz") {
			t.Errorf("the error shows the password: %v", err)
		}
		_, err = New().Open(ctx30(t), driver.ConnConfig{DSN: dsnFor(testDB, ""), Password: driver.NewSecret("wrong-Secret-xyz")})
		if !errors.Is(err, driver.ErrAuth) || strings.Contains(err.Error(), "wrong-Secret-xyz") {
			t.Errorf("ConnConfig.Password: want a redacted ErrAuth, got %v", err)
		}
	})
	t.Run("password from ConnConfig", func(t *testing.T) {
		conn, err := New().Open(ctx30(t), driver.ConnConfig{DSN: dsnFor(testDB, "wrong"), Password: driver.NewSecret(testPassword)})
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	})
	t.Run("closed", func(t *testing.T) {
		conn := openTest(t, testDB, false)
		_ = conn.Close()
		if err := conn.Ping(ctx30(t)); !errors.Is(err, driver.ErrConnection) {
			t.Errorf("Ping after Close: want ErrConnection, got %v", err)
		}
	})
}

func TestIntegrationTypes(t *testing.T) {
	c := openTest(t, testDB, false)
	ctx := ctx30(t)
	oid := bson.NewObjectID()
	dec, _ := bson.ParseDecimal128("1234567890.123456789")
	when := time.Date(2026, 3, 4, 5, 6, 7, 8e6, time.UTC)
	doc := bson.D{
		{Key: "_id", Value: oid},
		{Key: "dec", Value: dec},
		{Key: "when", Value: bson.NewDateTimeFromTime(when)},
		{Key: "uuid", Value: bson.Binary{Subtype: bson.TypeBinaryUUID, Data: make([]byte, 16)}},
		{Key: "bin", Value: bson.Binary{Data: []byte{0xde, 0xad}}},
		{Key: "nested", Value: bson.D{{Key: "a", Value: bson.A{int32(1), bson.D{{Key: "b", Value: "c"}}}}}},
		{Key: "long", Value: int64(1) << 40},
		{Key: "ts", Value: bson.Timestamp{T: 100, I: 1}},
	}
	if _, err := admin.Database(testDB).Collection("types").InsertOne(ctx, doc); err != nil {
		t.Fatal(err)
	}
	cols, rows, _ := collect(t, c, driver.Query{Text: `{"find":"types"}`})
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	row := rows[0]
	check := func(name string, typ driver.Type, display string) {
		t.Helper()
		v := cell(t, cols, row, name)
		if v.Type() != typ || (display != "" && v.String() != display) {
			t.Errorf("%s = %v (%s), want %s %q", name, v, v.Type(), typ, display)
		}
	}
	check("_id", driver.TypeUnknown, oid.Hex())
	check("dec", driver.TypeDecimal, "1234567890.123456789")
	check("when", driver.TypeTime, when.Format(time.RFC3339Nano))
	check("uuid", driver.TypeUnknown, "00000000-0000-0000-0000-000000000000")
	check("bin", driver.TypeBytes, `\xdead`)
	check("nested", driver.TypeObject, `{"a":[1,{"b":"c"}]}`)
	check("long", driver.TypeInt, "1099511627776")
	check("ts", driver.TypeUnknown, "")
	if cols[0].NativeType != "objectId" {
		t.Errorf("_id native type %q", cols[0].NativeType)
	}
}

func TestIntegrationCatalog(t *testing.T) {
	c := openTest(t, testDB, false)
	ctx := ctx30(t)
	nss, err := c.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ns := range nss {
		names = append(names, ns.Name)
		if ns.Name == testDB && !ns.Default {
			t.Error("the DSN's database is not the default")
		}
	}
	for _, sys := range []string{"admin", "local", "config"} {
		if slices.Contains(names, sys) {
			t.Errorf("namespaces include %s: %v", sys, names)
		}
	}
	if !slices.Contains(names, testDB) || !slices.Contains(names, "tobira_other") {
		t.Errorf("namespaces %v", names)
	}

	colls, err := c.Collections(ctx, driver.NamespaceRef{Name: testDB})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]driver.CollectionType{}
	for _, col := range colls {
		types[col.Ref.Name] = col.Type
		if strings.HasPrefix(col.Ref.Name, "system.") {
			t.Errorf("system collection listed: %s", col.Ref.Name)
		}
	}
	if types["tobira_people"] != driver.CollectionCollection || types["people_view"] != driver.CollectionView || types["docs"] != driver.CollectionCollection {
		t.Errorf("collections %v", types)
	}

	t.Run("describe people", func(t *testing.T) {
		sch, err := c.Describe(ctx, driver.CollectionRef{Name: "tobira_people"})
		if err != nil {
			t.Fatal(err)
		}
		if sch.Ref.Namespace != testDB || !sch.Schemaless || len(sch.Sample) != 57 || sch.RowEstimate != 57 {
			t.Errorf("schema %+v", sch)
		}
		if !slices.Equal(sch.PrimaryKey, []string{"_id"}) {
			t.Errorf("primary key %v", sch.PrimaryKey)
		}
		want := map[string]driver.Type{"_id": driver.TypeInt, "age": driver.TypeInt, "score": driver.TypeDecimal, "tags": driver.TypeList, "created_at": driver.TypeTime}
		for _, f := range sch.Fields {
			if typ, ok := want[f.Name]; ok && f.Type != typ {
				t.Errorf("field %s type %s, want %s", f.Name, f.Type, typ)
			}
			if f.Name == "age" && !f.Nullable {
				t.Error("age holds nulls: it is nullable")
			}
		}
		idx := map[string]driver.Index{}
		for _, ix := range sch.Indexes {
			idx[ix.Name] = ix
		}
		if !idx["_id_"].Primary || !idx["email_unique"].Unique || !slices.Equal(idx["age_name"].Fields, []string{"age", "name"}) || idx["name_text"].Method != "text" {
			t.Errorf("indexes %+v", sch.Indexes)
		}
	})
	t.Run("describe documents", func(t *testing.T) {
		sch, err := c.Describe(ctx, driver.CollectionRef{Namespace: testDB, Name: "docs"})
		if err != nil {
			t.Fatal(err)
		}
		fields := map[string]driver.Field{}
		for _, f := range sch.Fields {
			fields[f.Name] = f
		}
		if f := fields["age"]; f.Type != driver.TypeUnknown || f.NativeType != "int|string" || !f.Nullable {
			t.Errorf("age %+v", f)
		}
		addr := fields["address"]
		if addr.Type != driver.TypeObject || len(addr.Fields) != 3 {
			t.Errorf("address %+v", addr)
		}
		if len(sch.Fields) == 0 || sch.Fields[0].Name != "_id" {
			t.Errorf("fields %+v", sch.Fields)
		}
	})
	t.Run("describe view", func(t *testing.T) {
		sch, err := c.Describe(ctx, driver.CollectionRef{Name: "people_view"})
		if err != nil {
			t.Fatal(err)
		}
		if sch.Type != driver.CollectionView || len(sch.Sample) != 57 || sch.PrimaryKey != nil {
			t.Errorf("view schema %+v", sch)
		}
	})
	t.Run("describe missing", func(t *testing.T) {
		_, err := c.Describe(ctx, driver.CollectionRef{Name: "nope"})
		if !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("want ErrNotFound, got %v", err)
		}
	})
	_ = drivertest.People
}
