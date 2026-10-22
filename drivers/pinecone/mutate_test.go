package pinecone

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// Writes on a read-only conn fail before any RPC.
func TestMutatorReadOnly(t *testing.T) {
	c := newTestConn(t, true)
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "default", Name: "people"}
	key := driver.Object{{Key: idColumn, Value: driver.String("1")}}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: valuesColumn, Value: driver.Vector([]float32{1})}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Insert: %v", err)
	}
	if err := c.Update(ctx, ref, key, driver.Object{{Key: "a", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Update: %v", err)
	}
	if err := c.Delete(ctx, ref, key); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Delete: %v", err)
	}
}

func TestRecordID(t *testing.T) {
	for _, tc := range []struct {
		in   driver.Value
		want string
	}{
		{driver.String("doc#1"), "doc#1"},
		{driver.Int(7), "7"},
		{driver.Int(-3), "-3"},
	} {
		got, err := recordID(tc.in, "insert")
		if err != nil || got != tc.want {
			t.Errorf("recordID(%s) = %q, %v", tc.in, got, err)
		}
	}
	long := make([]byte, 513)
	for i := range long {
		long[i] = 'x'
	}
	for _, bad := range []driver.Value{driver.String(""), driver.String(string(long)), driver.String("a\x00b"), driver.Float(1.5), driver.Bool(true), driver.Null()} {
		if _, err := recordID(bad, "insert"); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("recordID(%s) = %v, want ErrSyntax", bad, err)
		}
	}
}

func TestSplitMembers(t *testing.T) {
	vals, meta, err := splitMembers(driver.Object{
		{Key: idColumn, Value: driver.String("x")},
		{Key: valuesColumn, Value: driver.List(driver.Int(1), driver.Float(2.5))},
		{Key: "name", Value: driver.String("n")},
		{Key: "age", Value: driver.Null()},
	}, "insert")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(vals, []float32{1, 2.5}) {
		t.Errorf("values = %v", vals)
	}
	if len(meta.GetFields()) != 1 || meta.GetFields()["name"].GetStringValue() != "n" {
		t.Errorf("metadata = %v (a NULL member is left out on insert)", meta)
	}
	for name, obj := range map[string]driver.Object{
		"sparse":          {{Key: sparseColumn, Value: driver.ObjectValue()}},
		"score column":    {{Key: scoreColumn, Value: driver.Float(1)}},
		"distance column": {{Key: distanceColumn, Value: driver.Float(1)}},
		"bad vector":      {{Key: valuesColumn, Value: driver.String("x")}},
		"bad element":     {{Key: valuesColumn, Value: driver.List(driver.String("x"))}},
		"empty vector":    {{Key: valuesColumn, Value: driver.Vector(nil)}},
		"object value":    {{Key: "o", Value: driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)})}},
	} {
		if _, _, err := splitMembers(obj, "insert"); !errors.Is(err, driver.ErrUnsupported) && !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
}

// Update refuses what Pinecone cannot do before contacting the server.
func TestUpdateRefusesBeforeNetwork(t *testing.T) {
	c := newTestConn(t, false)
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "default", Name: "people"}
	key := driver.Object{{Key: idColumn, Value: driver.String("1")}}
	for name, changes := range map[string]driver.Object{
		"id change": {{Key: idColumn, Value: driver.String("2")}},
		"null":      {{Key: "name", Value: driver.Null()}},
		"sparse":    {{Key: sparseColumn, Value: driver.ObjectValue()}},
	} {
		if err := c.Update(ctx, ref, key, changes); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", name, err)
		}
	}
	if err := c.Update(ctx, ref, driver.Object{}, driver.Object{{Key: "a", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrUnsupported) && !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("empty key: %v", err)
	}
	if err := c.Update(ctx, ref, key, nil); err != nil {
		t.Errorf("no changes: %v", err)
	}
	if err := c.Delete(ctx, driver.CollectionRef{Namespace: "other", Name: "x"}, key); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("unknown index: %v", err)
	}
}

// An edit of a renamed column reaches the metadata key, never the vector.
func TestSplitMembersRenamedColumns(t *testing.T) {
	vec, meta, err := splitMembers(driver.Object{
		{Key: "meta_values", Value: driver.String("v")},
		{Key: "meta_meta_x", Value: driver.Int(1)},
		{Key: "meta__id", Value: driver.String("i")},
	}, "update")
	if err != nil {
		t.Fatal(err)
	}
	f := meta.GetFields()
	if vec != nil || f["values"].GetStringValue() != "v" || f["meta_x"].GetNumberValue() != 1 || f["_id"].GetStringValue() != "i" || len(f) != 3 {
		t.Errorf("vec %v meta %v", vec, meta)
	}
	if err := checkKey(driver.Object{{Key: "meta_values", Value: driver.String("v")}}, "delete"); err != nil {
		t.Errorf("a renamed column names a record: %v", err)
	}
}
