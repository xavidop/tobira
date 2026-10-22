//go:build integration

package redis

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// TestTruncatedPreview checks that a row whose value preview was cut says so
// in the _truncated column, and that the Mutator refuses to write such a
// preview back over the longer real value.
func TestTruncatedPreview(t *testing.T) {
	ctx := context.Background()
	const db = 7
	cl := admin(t, db)
	t.Cleanup(func() { _ = cl.FlushDB(context.Background()).Err() })
	long := strings.Repeat("x", previewBytes+10)
	items := make([]any, previewItems+50)
	fields := make([]any, 0, 2*(previewItems+1))
	for i := range items {
		items[i] = "e" + strconv.Itoa(i)
	}
	for i := range previewItems + 1 {
		fields = append(fields, "f"+strconv.Itoa(i), "v")
	}
	_, err := cl.Pipelined(ctx, func(p goredis.Pipeliner) error {
		p.Set(ctx, "t:long", long, 0)
		p.Set(ctx, "t:short", "abc", 0)
		p.RPush(ctx, "t:list", items...)
		p.RPush(ctx, "t:small", "a", "b")
		p.HSet(ctx, "t:hash", fields...)
		p.SAdd(ctx, "t:set", items...)
		p.ZAdd(ctx, "t:z", goredis.Z{Member: "m", Score: 1})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := openTest(t, db, false)
	ref := driver.CollectionRef{Namespace: "7", Name: "t"}
	cols, rows, _ := browseAll(t, c, ref, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "_key"}}})
	ti := -1
	for i, col := range cols {
		if col.Name == "_truncated" {
			ti = i
		}
	}
	if ti < 0 {
		t.Fatalf("no _truncated column in %v", colNames(cols))
	}
	want := map[string]bool{"t:long": true, "t:short": false, "t:list": true, "t:small": false, "t:hash": true, "t:set": true, "t:z": false}
	previews := map[string]driver.Value{}
	for _, r := range rows {
		k, _ := r[0].AsText()
		previews[k] = r[3]
		if got, ok := r[ti].AsBool(); !ok || got != want[k] {
			t.Errorf("%s: _truncated = %s, want %v", k, r[ti], want[k])
		}
	}
	key := func(k string) driver.Object { return driver.Object{{Key: "_key", Value: driver.String(k)}} }
	const tooLarge = "value is too large to edit inline; use a query command"
	for _, k := range []string{"t:long", "t:list", "t:hash", "t:set"} {
		for name, changes := range map[string]driver.Object{
			"preview": {{Key: "value", Value: previews[k]}},
			"edited":  {{Key: "value", Value: driver.String("edited…")}},
			"field":   {{Key: "f0", Value: driver.String("x")}},
		} {
			err := c.Update(ctx, ref, key(k), changes)
			if k != "t:hash" && name == "field" {
				continue // a field change on a non-hash fails for its own reason
			}
			if !errors.Is(err, driver.ErrConflict) || !strings.Contains(err.Error(), tooLarge) {
				t.Errorf("inline %s update of %s = %v, want ErrConflict %q", name, k, err, tooLarge)
			}
		}
	}
	if v := cl.Get(ctx, "t:long").Val(); v != long {
		t.Errorf("t:long changed to %d bytes", len(v))
	}
	if n := cl.LLen(ctx, "t:list").Val(); n != int64(len(items)) {
		t.Errorf("t:list has %d elements", n)
	}
	// A ttl change and a delete are still fine on a large key; small keys edit normally.
	if err := c.Update(ctx, ref, key("t:long"), driver.Object{{Key: "ttl", Value: driver.Int(100)}}); err != nil {
		t.Errorf("ttl on a large key: %v", err)
	}
	if err := c.Update(ctx, ref, key("t:short"), driver.Object{{Key: "value", Value: previews["t:short"]}}); err != nil {
		t.Errorf("small value: %v", err)
	}
	if err := c.Delete(ctx, ref, key("t:list")); err != nil {
		t.Errorf("delete a large key: %v", err)
	}
	// _truncated is read-only metadata: an insert ignores it rather than storing a field.
	if _, err := c.Insert(ctx, ref, driver.Object{{Key: "_key", Value: driver.String("t:new")}, {Key: "value", Value: driver.String("v")}, {Key: "_truncated", Value: driver.Bool(false)}}); err != nil {
		t.Errorf("insert with _truncated: %v", err)
	}
	if typ := cl.Type(ctx, "t:new").Val(); typ != "string" {
		t.Errorf("t:new type %s", typ)
	}
}
