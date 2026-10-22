package milvus

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestMutatorReadOnlyBeforeNetwork(t *testing.T) {
	c, fake := newFakeConn(t, true)
	ctx := context.Background()
	ref := driver.CollectionRef{Name: "people"}
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	_, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(9)}})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Insert (ro) = %v", err)
	}
	if err := c.Update(ctx, ref, key, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Update (ro) = %v", err)
	}
	if err := c.Delete(ctx, ref, key); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Delete (ro) = %v", err)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("read-only writes reached the server: %v", calls)
	}
}

func TestKeyValue(t *testing.T) {
	s := peopleSchema()
	for _, tc := range []struct {
		key  driver.Object
		ok   bool
		want driver.Value
	}{
		{driver.Object{{Key: "id", Value: driver.Int(5)}}, true, driver.Int(5)},
		{driver.Object{{Key: "id", Value: driver.String("5")}}, true, driver.Int(5)},
		{driver.Object{{Key: "id", Value: driver.Decimal("5")}}, true, driver.Int(5)},
		{driver.Object{{Key: "id", Value: driver.String("x")}}, false, driver.Null()},
		{driver.Object{{Key: "id", Value: driver.Null()}}, false, driver.Null()},
		{driver.Object{{Key: "name", Value: driver.String("x")}}, false, driver.Null()},
		{driver.Object{{Key: "id", Value: driver.Int(1)}, {Key: "name", Value: driver.String("x")}}, false, driver.Null()},
		{driver.Object{}, false, driver.Null()},
	} {
		got, err := keyValue(s, tc.key, "update")
		if tc.ok {
			if err != nil || got.String() != tc.want.String() || got.Type() != tc.want.Type() {
				t.Errorf("keyValue(%v) = %v, %v; want %v", tc.key, got, err, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("keyValue(%v) succeeded, want an error", tc.key)
		}
	}
}

func TestMutatorRefusalsBeforeWrite(t *testing.T) {
	c, fake := newFakeConn(t, false)
	fake.describe = describeOK(peopleProto())
	ctx := context.Background()
	ref := driver.CollectionRef{Name: "people"}
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	// Changing the key, writing a function output or an unknown field on a
	// collection without the dynamic field never reach a write RPC.
	if err := c.Update(ctx, ref, key, driver.Object{{Key: "id", Value: driver.Int(2)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Update of the key = %v, want ErrUnsupported", err)
	}
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}, {Key: "bad key!", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Insert of an unusable dynamic key = %v, want ErrUnsupported", err)
	}
	for _, call := range fake.Calls() {
		if call == "Insert" || call == "Upsert" || call == "Delete" {
			t.Errorf("a refused edit sent %s", call)
		}
	}
}

// Update refuses an auto-id collection before any RPC but
// the schema read: Milvus would give the upserted entity a new key.
func TestUpdateRefusesAutoID(t *testing.T) {
	c, fake := newFakeConn(t, false)
	p := peopleProto()
	p.Fields[0].AutoID = true
	fake.describe = describeOK(p)
	err := c.Update(context.Background(), driver.CollectionRef{Name: "people"}, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "name", Value: driver.String("x")}})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Update on an auto-id collection = %v, want ErrUnsupported", err)
	}
	for _, call := range fake.Calls() {
		if call != "DescribeCollection" {
			t.Errorf("a refused update sent %s", call)
		}
	}
}
