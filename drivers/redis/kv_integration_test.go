//go:build integration

package redis

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"crypto/sha256"
	"encoding/hex"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func browseAll(t *testing.T, c *conn, ref driver.CollectionRef, opts driver.BrowseOptions) ([]driver.Column, []driver.Row, []int) {
	t.Helper()
	ctx := context.Background()
	var all []driver.Row
	var cols []driver.Column
	var sizes []int
	for page := 0; page < 10000; page++ {
		rs, err := c.Browse(ctx, ref, opts)
		if err != nil {
			t.Fatalf("Browse page %d: %v", page, err)
		}
		cols = rs.Columns()
		rows, st, err := driver.Collect(ctx, rs, 0)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, rows...)
		sizes = append(sizes, len(rows))
		if st.NextCursor == "" {
			return cols, all, sizes
		}
		opts.Cursor = st.NextCursor
	}
	t.Fatal("Browse never ended")
	return nil, nil, nil
}

func TestKeyValueCatalog(t *testing.T) {
	ctx := context.Background()
	c := openTest(t, dbMisc, false)
	nss, err := c.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nss) != 16 || nss[dbMisc].Name != "3" || nss[dbMisc].Label != "db" || !nss[dbMisc].Default || nss[0].Default {
		t.Errorf("namespaces = %+v", nss)
	}
	if nss[dbKV].Comment != "8 keys" {
		t.Errorf("db 2 comment = %q, want the key count", nss[dbKV].Comment)
	}
	cols, err := c.Collections(ctx, driver.NamespaceRef{Name: "2"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]driver.Collection{}
	var names []string
	for _, col := range cols {
		got[col.Ref.Name] = col
		names = append(names, col.Ref.Name)
	}
	// leaderboard has no ':': it is a key of "*", not a collection.
	want := []string{"*", "doc", "events", "queue", "session", "tags", "user"}
	if !slices.Equal(names, want) {
		t.Fatalf("collections = %q, want %q", names, want)
	}
	if got["*"].RowEstimate != 8 || got["user"].RowEstimate != 2 || got["user"].Type != driver.CollectionKeyPattern {
		t.Errorf("collections = %+v", cols)
	}
	if _, err := c.Collections(ctx, driver.NamespaceRef{Name: "x"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Collections(x) = %v, want ErrNotFound", err)
	}

	sch, err := c.Describe(ctx, driver.CollectionRef{Namespace: "2", Name: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if sch.Type != driver.CollectionKeyPattern || !sch.Schemaless || len(sch.Sample) != 2 || !slices.Equal(sch.PrimaryKey, []string{"_key"}) {
		t.Errorf("Describe(user) = %+v", sch)
	}
	if fs := sch.Fields; len(fs) != 5 || fs[0].Name != "_key" || fs[2].Type != driver.TypeInt || fs[4].Name != "_truncated" || fs[4].Type != driver.TypeBool || !fs[4].Generated {
		t.Errorf("Describe(user) fields = %+v", fs)
	}
}

// TestKeyValueBrowse compares Browse of "*" in the key-value database with
// drivertest.KeyValueDataset, value by value.
func TestKeyValueBrowse(t *testing.T) {
	c := openTest(t, dbKV, false)
	ref := driver.CollectionRef{Namespace: "2", Name: "*"}
	cols, rows, _ := browseAll(t, c, ref, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "_key"}}})
	if got := colNames(cols); !slices.Equal(got, []string{"_key", "type", "ttl", "value", "_truncated"}) {
		t.Fatalf("columns = %q", got)
	}
	byKey := map[string]driver.Row{}
	for _, r := range rows {
		k, _ := r[0].AsText()
		byKey[k] = r
	}
	for _, want := range drivertest.KeyValueDataset().Rows {
		k, _ := want[0].AsText()
		got, ok := byKey[k]
		if !ok {
			t.Errorf("key %s missing", k)
			continue
		}
		if !drivertest.EqualValue(want[1], got[1]) || !drivertest.EqualValue(want[3], got[3]) {
			t.Errorf("key %s: got %v, want %v", k, got, want)
		}
		wantTTL, _ := want[2].AsInt()
		gotTTL, _ := got[2].AsInt()
		if (wantTTL < 0) != (gotTTL < 0) || gotTTL > wantTTL {
			t.Errorf("key %s: ttl %d, want about %d", k, gotTTL, wantTTL)
		}
	}
	if r := byKey["events:s"]; r[1].String() != "stream" {
		t.Errorf("stream row = %v", r)
	}
	if r := byKey["doc:1"]; r[3].Type() != driver.TypeJSON {
		t.Errorf("json row = %v", r)
	}
	for _, tc := range []struct {
		opts driver.BrowseOptions
		want []string
	}{
		{driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "type", Op: driver.OpEq, Value: driver.String("hash")}}}}, []string{"user:1"}},
		{driver.BrowseOptions{Filter: driver.Filter{Raw: "user:*"}}, []string{"user:1", "user:1:name"}},
		{driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "ttl", Op: driver.OpGt, Value: driver.Int(0)}}}}, []string{"queue:jobs", "session:abc"}},
	} {
		tc.opts.Sort = []driver.SortKey{{Field: "_key"}}
		_, rows, _ := browseAll(t, c, ref, tc.opts)
		var keys []string
		for _, r := range rows {
			k, _ := r[0].AsText()
			keys = append(keys, k)
		}
		if !slices.Equal(keys, tc.want) {
			t.Errorf("Browse %+v = %q, want %q", tc.opts.Filter, keys, tc.want)
		}
	}
	if _, err := c.Browse(context.Background(), ref, driver.BrowseOptions{Cursor: "bogus"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("bad cursor = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Browse(ctx, ref, driver.BrowseOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Browse = %v", err)
	}
}

// TestBrowseLargeKeyspace pages through more keys than one SCAN batch
// returns, unsorted, and checks every key is seen exactly once; then a
// sorted browse beyond maxSortKeys is refused.
func TestBrowseLargeKeyspace(t *testing.T) {
	ctx := context.Background()
	const db, n = 4, maxSortKeys + 50
	cl := admin(t, db)
	t.Cleanup(func() { _ = cl.FlushDB(context.Background()).Err() })
	for start := 0; start < n; start += 1000 {
		_, err := cl.Pipelined(ctx, func(p goredis.Pipeliner) error {
			for i := start; i < min(start+1000, n); i++ {
				p.Set(ctx, "big:"+strconv.Itoa(i), "v", 0)
			}
			p.Set(ctx, "other:"+strconv.Itoa(start), "x", 0)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	c := openTest(t, db, false)
	ref := driver.CollectionRef{Namespace: "4", Name: "big"}
	_, rows, sizes := browseAll(t, c, ref, driver.BrowseOptions{Limit: 997})
	seen := map[string]int{}
	for _, r := range rows {
		k, _ := r[0].AsText()
		seen[k]++
	}
	if len(seen) != n {
		t.Errorf("saw %d distinct keys, want %d", len(seen), n)
	}
	for k, cnt := range seen {
		if cnt != 1 {
			t.Errorf("key %s seen %d times", k, cnt)
		}
	}
	for i, s := range sizes[:len(sizes)-1] {
		if s != 997 {
			t.Errorf("page %d has %d rows, want 997 (pages %v)", i, s, sizes)
			break
		}
	}
	_, err := c.Browse(ctx, ref, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "_key"}}})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("sorted browse over %d keys = %v, want ErrUnsupported", n, err)
	}
}

func TestQueryRendering(t *testing.T) {
	ctx := context.Background()
	c := openTest(t, dbKV, false)
	for _, tc := range []struct {
		text string
		cols []string
		rows int
	}{
		{"HGETALL user:1", []string{"field", "value"}, 2},
		{"ZRANGE leaderboard 0 -1 WITHSCORES", []string{"member", "score"}, 2},
		{"XRANGE events:s - +", []string{"id", "fields"}, 1},
		{"MGET user:1:name nope", []string{"key", "value"}, 2},
		{"GET nope", []string{"value"}, 1},
		{"JSON.GET doc:1", []string{"value"}, 1},
		{"SCAN 0 COUNT 100", []string{"key"}, 8},
		{"INFO keyspace", []string{"section", "key", "value"}, 3},
	} {
		rs, err := c.Query(ctx, driver.Query{Text: tc.text})
		if err != nil {
			t.Errorf("%s: %v", tc.text, err)
			continue
		}
		rows, _, err := driver.Collect(ctx, rs, 0)
		if err != nil || !slices.Equal(colNames(rs.Columns()), tc.cols) || len(rows) < 1 || (tc.rows != 3 && len(rows) != tc.rows) {
			t.Errorf("%s: cols %q rows %d (%v), want %q %d", tc.text, colNames(rs.Columns()), len(rows), err, tc.cols, tc.rows)
		}
	}
	// The namespace picks the database for one call.
	misc := openTest(t, dbMisc, false)
	rs, err := misc.Query(ctx, driver.Query{Text: "DBSIZE", Namespace: "2"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _ := driver.Collect(ctx, rs, 0)
	if len(rows) != 1 || !drivertest.EqualValue(driver.Int(8), rows[0][0]) {
		t.Errorf("DBSIZE in namespace 2 = %v, want 8", rows)
	}
	for text, kind := range map[string]error{
		"SELECT 4": driver.ErrUnsupported, "BLPOP l 0": driver.ErrUnsupported, "SUBSCRIBE c": driver.ErrUnsupported,
		"NOSUCHCOMMAND": driver.ErrSyntax, `GET "open`: driver.ErrSyntax, "": driver.ErrSyntax,
		"HGET leaderboard x": driver.ErrConflict,
	} {
		if _, err := c.Query(ctx, driver.Query{Text: text}); !errors.Is(err, kind) {
			t.Errorf("%q = %v, want %v", text, err, kind)
		}
	}
	if _, err := c.Query(ctx, driver.Query{Text: "GET x", Params: []driver.Value{driver.Int(1)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("params = %v", err)
	}
	rs, err = c.Query(ctx, driver.Query{Text: "LRANGE queue:jobs 0 -1", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	rows, st, _ := driver.Collect(ctx, rs, 0)
	if len(rows) != 2 || !st.Truncated {
		t.Errorf("Limit 2: %d rows, truncated %v", len(rows), st.Truncated)
	}
}

// TestCollectionsHideKeyNames checks Collections lists key prefixes only:
// keys without a ':' (here a session token, an API key id and an e-mail
// address, row data a model must not see under send_rows: never) stay in
// "*", and the prefix list stops at 500 with a note in the last comment.
func TestCollectionsHideKeyNames(t *testing.T) {
	const db = 7
	ctx := context.Background()
	a := admin(t, db)
	if err := a.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.FlushDB(context.Background()).Err() })
	p := a.Pipeline()
	for i := range 520 {
		p.Set(ctx, fmt.Sprintf("p%04d:x", i), "v", 0)
	}
	for _, k := range []string{"session_9f8e7d6c", "apikey-42", "alice@example.com"} {
		p.Set(ctx, k, "secret", 0)
	}
	if _, err := p.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	c := openTest(t, db, true)
	cols, err := c.Collections(ctx, driver.NamespaceRef{Name: strconv.Itoa(db)})
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 501 || cols[0].Ref.Name != "*" || cols[0].RowEstimate != 523 {
		t.Fatalf("%d collections (first %+v), want * (523 keys) and 500 prefixes", len(cols), cols[0])
	}
	for _, col := range cols {
		if !strings.HasPrefix(col.Ref.Name, "p") && col.Ref.Name != "*" {
			t.Errorf("collection %q lists a whole key name", col.Ref.Name)
		}
	}
	if last := cols[500]; last.Ref.Name != "p0499" || !strings.Contains(last.Comment, "20 more prefixes") {
		t.Errorf("last collection = %+v, want p0499 with a note on 20 more prefixes", last)
	}
}

// TestBareKeyStaysInAllKeys: a key named like a prefix (user, next to
// user:1) is in "*" only. Browse, the row estimate, the Mutator and Watch
// all agree that the user collection holds user:* and nothing else.
func TestBareKeyStaysInAllKeys(t *testing.T) {
	const db = 8
	ctx := context.Background()
	a := admin(t, db)
	if err := a.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.FlushDB(context.Background()).Err() })
	if err := a.HSet(ctx, "user:1", "name", "ada").Err(); err != nil {
		t.Fatal(err)
	}
	if err := a.HSet(ctx, "user:2", "name", "bob").Err(); err != nil {
		t.Fatal(err)
	}
	if err := a.Set(ctx, "user", "bare", 0).Err(); err != nil {
		t.Fatal(err)
	}
	c := openTest(t, db, false)
	ns := strconv.Itoa(db)
	cols, err := c.Collections(ctx, driver.NamespaceRef{Name: ns})
	if err != nil {
		t.Fatal(err)
	}
	var user *driver.Collection
	for i := range cols {
		if cols[i].Ref.Name == "user" {
			user = &cols[i]
		}
	}
	if user == nil || user.RowEstimate != 2 || user.Type != driver.CollectionCollection {
		t.Fatalf("user collection = %+v, want 2 hash records", user)
	}
	ref := driver.CollectionRef{Namespace: ns, Name: "user"}
	_, rows, _ := browseAll(t, c, ref, driver.BrowseOptions{Limit: 10})
	if len(rows) != int(user.RowEstimate) {
		t.Errorf("browse user: %d rows, want the estimate %d", len(rows), user.RowEstimate)
	}
	for _, r := range rows {
		if r[0].String() == "user" {
			t.Errorf("browse user holds the bare key user: %v", r)
		}
	}
	if err := c.Delete(ctx, ref, driver.Object{{Key: "_key", Value: driver.String("user")}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("delete the bare key through collection user: %v, want ErrSyntax", err)
	}
	if n, err := a.Exists(ctx, "user").Result(); err != nil || n != 1 {
		t.Errorf("bare key user: exists %d, %v", n, err)
	}
	_, all, _ := browseAll(t, c, driver.CollectionRef{Namespace: ns, Name: "*"}, driver.BrowseOptions{Limit: 10})
	if len(all) != 3 {
		t.Errorf("browse *: %d rows, want 3", len(all))
	}
}

// TestMixedCaseTypes: a type given in any case means the same Redis type.
func TestMixedCaseTypes(t *testing.T) {
	ctx := context.Background()
	const db = 8
	cl := admin(t, db)
	t.Cleanup(func() { _ = cl.FlushDB(context.Background()).Err() })
	c := openTest(t, db, false)
	all := driver.CollectionRef{Namespace: "8", Name: "*"}
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	key := func(k string) driver.Object { return driver.Object{m("_key", driver.String(k))} }
	if _, err := c.Insert(ctx, all, driver.Object{m("_key", driver.String("c:l")), m("type", driver.String("LIST")),
		m("value", driver.List(driver.String("a"), driver.String("a")))}); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, all, key("c:l"), driver.Object{m("type", driver.String("List")),
		m("value", driver.List(driver.String("b"), driver.String("b"), driver.String("c")))}); err != nil {
		t.Fatalf("Update type List: %v", err)
	}
	if typ := cl.Type(ctx, "c:l").Val(); typ != "list" || cl.LLen(ctx, "c:l").Val() != 3 {
		t.Errorf("List kept as %s with %d elements, want a list of 3", typ, cl.LLen(ctx, "c:l").Val())
	}
	cl.HSet(ctx, "c:h", "f", "1")
	if err := c.Update(ctx, all, key("c:h"), driver.Object{m("type", driver.String("Hash")), m("g", driver.String("2"))}); err != nil {
		t.Errorf("field update with type Hash: %v", err)
	}
	cl.Do(ctx, "JSON.SET", "c:j", "$", `{"a":1}`)
	if err := c.Update(ctx, all, key("c:j"), driver.Object{m("type", driver.String("json")), m("ttl", driver.Int(50))}); err != nil {
		t.Errorf("type json on a ReJSON-RL key: %v", err)
	}
	if err := c.Update(ctx, all, key("c:j"), driver.Object{m("type", driver.String("JSON")), m("value", driver.JSON([]byte(`{"b":2}`)))}); err != nil {
		t.Errorf("value with type JSON: %v", err)
	}
	if v, _ := cl.Do(ctx, "JSON.GET", "c:j", "$.b").Text(); v != "[2]" {
		t.Errorf("JSON.GET = %q", v)
	}
}

// TestUnboundedReadsRefused: whole-collection reads of a huge value are
// refused after a size check; ranged or counted reads still work.
func TestUnboundedReadsRefused(t *testing.T) {
	ctx := context.Background()
	const db = 9
	cl := admin(t, db)
	t.Cleanup(func() { _ = cl.FlushDB(context.Background()).Err() })
	n := maxReplyElements + 1
	for start := 0; start < n; start += 10000 {
		_, err := cl.Pipelined(ctx, func(p goredis.Pipeliner) error {
			items := make([]any, 0, 10000)
			fv := make([]any, 0, 20000)
			for i := start; i < min(start+10000, n); i++ {
				items = append(items, "e"+strconv.Itoa(i))
				fv = append(fv, "f"+strconv.Itoa(i), "v")
			}
			p.RPush(ctx, "u:l", items...)
			p.SAdd(ctx, "u:s", items...)
			p.HSet(ctx, "u:h", fv...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i += 5000 {
		_, err := cl.Pipelined(ctx, func(p goredis.Pipeliner) error {
			for j := i; j < min(i+5000, n); j++ {
				p.XAdd(ctx, &goredis.XAddArgs{Stream: "u:x", Values: []any{"f", "v"}})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	cl.Set(ctx, "u:big", strings.Repeat("x", maxReplyBytes+1), 0)
	for _, ro := range []bool{false, true} {
		c := openTest(t, db, ro)
		for _, text := range []string{"LRANGE u:l 0 -1", "SMEMBERS u:s", "HGETALL u:h", "HKEYS u:h", "XRANGE u:x - +", "GET u:big", "MGET u:big"} {
			_, err := c.Query(ctx, driver.Query{Text: text})
			if !errors.Is(err, driver.ErrLimit) || !strings.Contains(err.Error(), "too large; use HSCAN/SSCAN/ZSCAN or a ranged command") {
				t.Errorf("read-only=%v %q = %v, want ErrLimit too large", ro, text, err)
			}
		}
		for _, text := range []string{"LRANGE u:l 0 99", "XRANGE u:x - + COUNT 10", "SSCAN u:s 0 COUNT 100", "HLEN u:h", "GETRANGE u:big 0 99"} {
			if _, err := c.Query(ctx, driver.Query{Text: text}); err != nil {
				t.Errorf("read-only=%v %q: %v", ro, text, err)
			}
		}
	}
}

// TestNamespaceCap: a database number beyond the server's count is not found.
func TestNamespaceCap(t *testing.T) {
	c := openTest(t, dbMisc, false)
	if _, err := c.Query(context.Background(), driver.Query{Text: "DBSIZE", Namespace: "16"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("namespace 16 of 16 = %v, want ErrNotFound", err)
	}
	if _, err := c.Query(context.Background(), driver.Query{Text: "DBSIZE", Namespace: "15"}); err != nil {
		t.Errorf("namespace 15: %v", err)
	}
}

// TestCredentialRedactionLive: secrets never reach the grid.
func TestCredentialRedactionLive(t *testing.T) {
	ctx := context.Background()
	sum := sha256.Sum256([]byte(testPassword))
	c := openTest(t, dbMisc, false)
	for _, text := range []string{"CONFIG GET requirepass", "ACL LIST", "ACL GETUSER default"} {
		rs, err := c.Query(ctx, driver.Query{Text: text})
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		rows, _, _ := driver.Collect(ctx, rs, 0)
		flat := ""
		for _, r := range rows {
			for _, v := range r {
				flat += v.String() + " "
			}
		}
		if strings.Contains(flat, testPassword) || strings.Contains(flat, hex.EncodeToString(sum[:])) || !strings.Contains(flat, "xxxxx") {
			t.Errorf("%s leaks or does not mask: %s", text, flat)
		}
	}
}
