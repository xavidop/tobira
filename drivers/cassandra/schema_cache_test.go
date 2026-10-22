package cassandra

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestSchemaCache(t *testing.T) {
	now := time.Unix(0, 0)
	sc := newSchemaCache(5 * time.Second)
	sc.now = func() time.Time { return now }
	ref := driver.CollectionRef{Namespace: "ks", Name: "t"}
	loads := 0
	load := func(context.Context) (*driver.CollectionSchema, error) {
		loads++
		return &driver.CollectionSchema{Ref: ref, Fields: []driver.Field{{Name: "id"}}, PrimaryKey: []string{"id"}}, nil
	}
	get := func() *driver.CollectionSchema {
		t.Helper()
		s, err := sc.get(context.Background(), ref, load)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := get()
	first.Fields[0].Name = "mutated" // callers get a copy
	first.PrimaryKey[0] = "mutated"
	if s := get(); loads != 1 || s.Fields[0].Name != "id" || s.PrimaryKey[0] != "id" {
		t.Errorf("a hit must not reload (loads %d) and must return an unshared copy: %+v", loads, s)
	}
	now = now.Add(6 * time.Second)
	get()
	if loads != 2 {
		t.Errorf("after the TTL: loads %d, want 2", loads)
	}
	sc.invalidate()
	get()
	if loads != 3 {
		t.Errorf("after invalidate: loads %d, want 3", loads)
	}
	other := driver.CollectionRef{Namespace: "ks", Name: "u"}
	failing := func(context.Context) (*driver.CollectionSchema, error) { return nil, driver.ErrNotFound }
	for range 2 {
		if _, err := sc.get(context.Background(), other, failing); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("error: %v", err)
		}
	}
	calls := 0
	if _, err := sc.get(context.Background(), other, func(ctx context.Context) (*driver.CollectionSchema, error) {
		calls++
		return load(ctx)
	}); err != nil || calls != 1 {
		t.Errorf("errors must not be cached: calls %d, err %v", calls, err)
	}
}

// A load that started before an invalidate is not stored.
func TestSchemaCacheInvalidateDuringLoad(t *testing.T) {
	sc := newSchemaCache(time.Minute)
	ref := driver.CollectionRef{Namespace: "ks", Name: "t"}
	loads := 0
	_, _ = sc.get(context.Background(), ref, func(context.Context) (*driver.CollectionSchema, error) {
		loads++
		sc.invalidate()
		return &driver.CollectionSchema{Ref: ref}, nil
	})
	_, _ = sc.get(context.Background(), ref, func(context.Context) (*driver.CollectionSchema, error) {
		loads++
		return &driver.CollectionSchema{Ref: ref}, nil
	})
	if loads != 2 {
		t.Errorf("loads = %d, want 2: the first answer predates the invalidate", loads)
	}
}
