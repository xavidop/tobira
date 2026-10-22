//go:build integration

package redis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestMutatorByType(t *testing.T) {
	ctx := context.Background()
	const db = 5
	cl := admin(t, db)
	t.Cleanup(func() { _ = cl.FlushDB(context.Background()).Err() })
	c := openTest(t, db, false)
	all := driver.CollectionRef{Namespace: "5", Name: "*"}
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	str := driver.String
	key := func(k string) driver.Object { return driver.Object{m("_key", str(k))} }

	inserts := []struct {
		values driver.Object
		check  func() error
	}{
		{driver.Object{m("_key", str("m:s")), m("value", str("hello")), m("ttl", driver.Int(100))}, func() error {
			if v := cl.Get(ctx, "m:s").Val(); v != "hello" {
				return errors.New("string " + v)
			}
			if d := cl.TTL(ctx, "m:s").Val(); d <= 0 || d > 100*time.Second {
				return errors.New("ttl " + d.String())
			}
			return nil
		}},
		{driver.Object{m("_key", str("m:h")), m("value", driver.ObjectValue(m("a", driver.Int(1)), m("b", str("x"))))}, func() error {
			if v := cl.HGetAll(ctx, "m:h").Val(); v["a"] != "1" || v["b"] != "x" {
				return errors.New("hash")
			}
			return nil
		}},
		{driver.Object{m("_key", str("m:l")), m("value", driver.List(str("a"), str("b")))}, func() error {
			if v := cl.LRange(ctx, "m:l", 0, -1).Val(); len(v) != 2 || v[1] != "b" {
				return errors.New("list")
			}
			return nil
		}},
		{driver.Object{m("_key", str("m:set")), m("type", str("set")), m("value", driver.List(str("a"), str("a"), str("b")))}, func() error {
			if n := cl.SCard(ctx, "m:set").Val(); n != 2 {
				return errors.New("set")
			}
			return nil
		}},
		{driver.Object{m("_key", str("m:z")), m("type", str("zset")), m("value", driver.List(driver.ObjectValue(m("member", str("a")), m("score", driver.Float(2.5)))))}, func() error {
			if s := cl.ZScore(ctx, "m:z", "a").Val(); s != 2.5 {
				return errors.New("zset")
			}
			return nil
		}},
		{driver.Object{m("_key", str("m:j")), m("type", str("ReJSON-RL")), m("value", driver.JSON([]byte(`{"a":[1]}`)))}, func() error {
			if v, _ := cl.Do(ctx, "JSON.GET", "m:j", "$.a").Text(); v != "[[1]]" {
				return errors.New("json " + v)
			}
			return nil
		}},
	}
	for _, in := range inserts {
		got, err := c.Insert(ctx, all, in.values)
		if err != nil {
			t.Errorf("Insert(%v): %v", in.values, err)
			continue
		}
		k, _ := in.values.Get("_key")
		if gk, _ := got.Get("_key"); !drivertest.EqualValue(k, gk) {
			t.Errorf("Insert returned key %v", got)
		}
		if err := in.check(); err != nil {
			t.Errorf("after Insert(%v): %v", in.values, err)
		}
	}
	if _, err := c.Insert(ctx, all, driver.Object{m("_key", str("m:s")), m("value", str("again"))}); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("Insert over an existing key = %v, want ErrConstraint", err)
	}
	if _, err := c.Insert(ctx, all, driver.Object{m("value", str("x"))}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Insert without _key in * = %v, want ErrSyntax", err)
	}

	// Replacing a value keeps the expiry; ttl -1 removes it.
	if err := c.Update(ctx, all, key("m:s"), driver.Object{m("value", str("bye"))}); err != nil {
		t.Fatal(err)
	}
	if cl.Get(ctx, "m:s").Val() != "bye" || cl.TTL(ctx, "m:s").Val() <= 0 {
		t.Errorf("value replace lost the value or the ttl")
	}
	if err := c.Update(ctx, all, key("m:s"), driver.Object{m("ttl", driver.Int(-1))}); err != nil || cl.TTL(ctx, "m:s").Val() != -1 {
		t.Errorf("ttl -1: %v, ttl now %v", err, cl.TTL(ctx, "m:s").Val())
	}
	if err := c.Update(ctx, all, key("m:h"), driver.Object{m("a", driver.Null()), m("c", driver.Int(3))}); err != nil {
		t.Fatal(err)
	}
	if v := cl.HGetAll(ctx, "m:h").Val(); v["a"] != "" || v["c"] != "3" || v["b"] != "x" {
		t.Errorf("field update: %v", v)
	}
	if err := c.Update(ctx, all, key("m:l"), driver.Object{m("type", str("set")), m("value", driver.List(str("z")))}); err != nil || cl.Type(ctx, "m:l").Val() != "set" {
		t.Errorf("type change with a value: %v", err)
	}
	for name, tc := range map[string]struct {
		key     driver.Object
		changes driver.Object
		kind    error
	}{
		"missing":       {key("m:nope"), driver.Object{m("value", str("x"))}, driver.ErrNotFound},
		"field on list": {key("m:set"), driver.Object{m("f", str("x"))}, driver.ErrConflict},
		"type only":     {key("m:s"), driver.Object{m("type", str("list"))}, driver.ErrSyntax},
		"rename":        {key("m:s"), driver.Object{m("_key", str("m:other"))}, driver.ErrUnsupported},
		"bad ttl":       {key("m:s"), driver.Object{m("ttl", driver.Int(0))}, driver.ErrSyntax},
		"two members":   {driver.Object{m("a", str("1")), m("b", str("2"))}, driver.Object{m("value", str("x"))}, driver.ErrSyntax},
	} {
		if err := c.Update(ctx, all, tc.key, tc.changes); !errors.Is(err, tc.kind) {
			t.Errorf("Update %s = %v, want %v", name, err, tc.kind)
		}
	}
	if err := c.Delete(ctx, all, key("m:z")); err != nil || cl.Exists(ctx, "m:z").Val() != 0 {
		t.Errorf("Delete: %v", err)
	}
	if err := c.Delete(ctx, all, key("m:z")); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Delete missing = %v", err)
	}
	// In a prefix collection a key outside it is refused, and {id: v} means "<prefix>:v".
	pref := driver.CollectionRef{Namespace: "5", Name: "m"}
	if err := c.Delete(ctx, pref, key("other:1")); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Delete outside the collection = %v", err)
	}
	if err := c.Delete(ctx, pref, driver.Object{m("id", str("h"))}); err != nil || cl.Exists(ctx, "m:h").Val() != 0 {
		t.Errorf("Delete by id: %v", err)
	}
	ro := openTest(t, db, true)
	if err := ro.Delete(ctx, all, key("m:s")); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Delete on a read-only conn = %v", err)
	}
}

func TestWatchEvents(t *testing.T) {
	const db = 6
	cl := admin(t, db)
	t.Cleanup(func() { _ = cl.FlushDB(context.Background()).Err() })
	c := openTest(t, db, true) // watching is a read
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := c.Watch(ctx, driver.WatchRequest{Collection: &driver.CollectionRef{Namespace: "6", Name: "w"}})
	if err != nil {
		t.Fatal(err)
	}
	cl.Set(ctx, "other:1", "x", 0) // outside the collection: filtered
	cl.Set(ctx, "wx:1", "x", 0)    // shares the MATCH prefix only: filtered
	cl.Set(ctx, "w:1", "x", 0)
	cl.Del(ctx, "w:1")
	var got []driver.Event
	for len(got) < 2 {
		select {
		case ev := <-ch:
			if ev.Err != nil {
				t.Fatal(ev.Err)
			}
			got = append(got, ev)
		case <-ctx.Done():
			t.Fatalf("events so far %+v", got)
		}
	}
	// KEA (A excludes the "new" class) publishes "set", then "del".
	wantOps := []string{"update", "delete"}
	for i, ev := range got {
		if ev.Channel != "w:1" || ev.Op != wantOps[i] {
			t.Errorf("event %d = %+v, want %s on w:1", i, ev, wantOps[i])
		}
	}
	cancel()
	for range ch { // drain until closed
	}
}
