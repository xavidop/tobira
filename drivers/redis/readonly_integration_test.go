//go:build integration

package redis

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"

	"github.com/xavidop/tobira/driver"
)

// seedMisc fills database dbMisc with one key of each type and returns a
// snapshot function: every key with its DUMP payload, the server config
// that matters, the function library and the search indexes.
func seedMisc(t *testing.T) func() map[string]string {
	t.Helper()
	ctx := context.Background()
	cl := admin(t, dbMisc)
	if err := cl.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	_, err := cl.TxPipelined(ctx, func(p goredis.Pipeliner) error {
		p.Set(ctx, "ro:s", "v", 0)
		p.HSet(ctx, "ro:h", "f", "1")
		p.RPush(ctx, "ro:l", "a", "b")
		p.SAdd(ctx, "ro:set", "m")
		p.ZAdd(ctx, "ro:z", goredis.Z{Member: "m", Score: 1})
		p.XAdd(ctx, &goredis.XAddArgs{Stream: "ro:x", ID: "1-0", Values: []any{"f", "v"}})
		p.Do(ctx, "JSON.SET", "ro:j", "$", `{"a":1}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return func() map[string]string {
		t.Helper()
		snap := map[string]string{}
		var cursor uint64
		for {
			keys, next, err := cl.Scan(ctx, cursor, "*", 1000).Result()
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range keys {
				d, err := cl.Dump(ctx, k).Result()
				if err != nil {
					t.Fatal(err)
				}
				snap["key "+k] = d
			}
			if cursor = next; cursor == 0 {
				break
			}
		}
		cfg, err := cl.ConfigGet(ctx, "notify-keyspace-events").Result()
		if err != nil {
			t.Fatal(err)
		}
		snap["config notify"] = cfg["notify-keyspace-events"]
		fl, err := cl.Do(ctx, "FUNCTION", "LIST").Slice()
		if err != nil {
			t.Fatal(err)
		}
		snap["functions"] = driver.List(replyValue(fl)).String()
		idx, err := admin(t, 0).Do(ctx, "FT._LIST").Slice()
		if err != nil {
			t.Fatal(err)
		}
		snap["indexes"] = replyValue(idx).String()
		return snap
	}
}

// liveProbes are writes, escapes and renaming tricks sent to a real server.
var liveProbes = []string{
	"SET ro:s x", "set ro:s x", `"SET" ro:s x`, "SeT ro:s x", "APPEND ro:s x", "INCR ro:n", "GETDEL ro:s",
	"GETEX ro:s EX 1", "SETRANGE ro:s 0 x", "DEL ro:s", "UNLINK ro:s", "EXPIRE ro:s 1", "PERSIST ro:s",
	"RENAME ro:s ro:s2", "COPY ro:s ro:s2", "MOVE ro:s 1", "RESTORE ro:new 0 x",
	"HSET ro:h f 2", "HDEL ro:h f", "HINCRBY ro:h f 1", "HGETDEL ro:h FIELDS 1 f",
	"LPUSH ro:l x", "RPOP ro:l", "LSET ro:l 0 x", "LTRIM ro:l 0 0", "LMOVE ro:l ro:l2 LEFT LEFT",
	"SADD ro:set x", "SPOP ro:set", "SREM ro:set m", "SINTERSTORE ro:d ro:set",
	"ZADD ro:z 2 x", "ZPOPMIN ro:z", "ZREM ro:z m", "ZRANGESTORE ro:d ro:z 0 -1",
	"XADD ro:x * f v", "XDEL ro:x 1-0", "XTRIM ro:x MAXLEN 0", "XGROUP CREATE ro:x g $",
	"XREADGROUP GROUP g c STREAMS ro:x >",
	"JSON.SET ro:j $ 2", "JSON.DEL ro:j", "JSON.NUMINCRBY ro:j $.a 1",
	"FT.CREATE ro_idx SCHEMA f TEXT", "FT.DROPINDEX " + peopleIndex, "FT.ALIASADD a " + peopleIndex,
	`EVAL "return redis.call('DEL','ro:s')" 0`, `EVAL_RO "return redis.call('DEL','ro:s')" 0`,
	`EVALSHA 0000000000000000000000000000000000000000 0`,
	`FUNCTION LOAD "#!lua name=x\nredis.register_function('f', function() return redis.call('DEL','ro:s') end)"`,
	"FCALL f 0", "FCALL_RO f 0", "FUNCTION FLUSH", "SCRIPT FLUSH",
	"MULTI", "EXEC", "SELECT 1", "SWAPDB 0 3",
	"CONFIG SET notify-keyspace-events \"\"", "CONFIG RESETSTAT", "MODULE LIST", "MODULE UNLOAD search",
	"DEBUG SLEEP 0", "FLUSHALL", "FLUSHDB", "FLUSHALL ASYNC", "BGSAVE", "SAVE",
	"ACL SETUSER mallory on >x +@all", "CLIENT KILL TYPE normal", "SORT ro:l STORE ro:d", "SORT_RO ro:l",
	"BITOP NOT ro:d ro:s", "PFADD ro:p a", "GEOADD ro:g 1 1 m", "PUBLISH c m", "KEYS *",
	"SCAN 0 COUNT 100000", "GET\x00 ro:s", "\"GET \" ro:s", "MEMORY PURGE", "OBJECT FREQ ro:s",
}

func TestReadOnlyProbesLive(t *testing.T) {
	snapshot := seedMisc(t)
	before := snapshot()
	ctx := context.Background()
	rw := openTest(t, dbMisc, false)
	ro := openTest(t, dbMisc, true)
	for _, text := range liveProbes {
		for _, tc := range []struct {
			name string
			c    *conn
			q    driver.Query
		}{
			{"conn read-only", ro, driver.Query{Text: text}},
			{"query read-only", rw, driver.Query{Text: text, ReadOnly: true}},
		} {
			_, err := tc.c.Query(ctx, tc.q)
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%s %q: want ErrReadOnly, got %v", tc.name, text, err)
			}
		}
	}
	if after := snapshot(); !maps.Equal(before, after) {
		t.Errorf("the probes changed the server:\nbefore %v\nafter  %v", before, after)
	}
	// The allow-listed reads still pass both layers.
	for _, text := range []string{
		"GET ro:s", "HGETALL ro:h", "LRANGE ro:l 0 -1", "SMEMBERS ro:set", "ZRANGE ro:z 0 -1 WITHSCORES",
		"XRANGE ro:x - +", "XINFO STREAM ro:x", "TYPE ro:s", "TTL ro:s", "SCAN 0 COUNT 10", "DBSIZE", "INFO keyspace",
		"JSON.GET ro:j", "JSON.TYPE ro:j", "MEMORY USAGE ro:s", "OBJECT ENCODING ro:s", "FT._LIST",
	} {
		if _, err := ro.Query(ctx, driver.Query{Text: text}); err != nil {
			t.Errorf("read-only %q: %v", text, err)
		}
	}
}

// TestReadOnlyRenamedCommand runs a server whose rename-command gave DEL
// the allow-listed name TTL. The allow-list passes "TTL", and the second
// layer (COMMAND INFO flags) must refuse it.
func TestReadOnlyRenamedCommand(t *testing.T) {
	ctx := context.Background()
	ctr, addr, err := startRedis(ctx, "--rename-command", "TTL", "\"\"", "--rename-command", "DEL", "TTL")
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	if err != nil {
		t.Fatal(err)
	}
	cl := goredis.NewClient(&goredis.Options{Addr: addr, Protocol: 2, DisableIdentity: true})
	t.Cleanup(func() { _ = cl.Close() })
	if err := cl.Set(ctx, "victim", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	c, err := New().Open(ctx, driver.ConnConfig{DSN: "redis://" + addr + "/0", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Query(ctx, driver.Query{Text: "TTL victim"}); !errors.Is(err, driver.ErrReadOnly) ||
		!strings.Contains(err.Error(), "server does not report") {
		t.Errorf("renamed DEL as TTL: want ErrReadOnly from the COMMAND INFO layer, got %v", err)
	}
	if n, err := cl.Exists(ctx, "victim").Result(); err != nil || n != 1 {
		t.Errorf("victim deleted (exists=%d, %v)", n, err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: "GET victim"}); err != nil {
		t.Errorf("GET on the renamed server: %v", err)
	}

	// The same server has keyspace notifications off: Watch refuses
	// without touching the configuration.
	w := c.(driver.Watcher)
	_, err = w.Watch(ctx, driver.WatchRequest{Channel: "*"})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Watch with notifications off: want ErrUnsupported, got %v", err)
	}
	if cfg, err := cl.ConfigGet(ctx, "notify-keyspace-events").Result(); err != nil || cfg["notify-keyspace-events"] != "" {
		t.Errorf("notify-keyspace-events changed: %v %v", cfg, err)
	}
}
