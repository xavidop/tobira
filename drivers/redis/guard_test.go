package redis

import (
	"errors"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"GET a", []string{"GET", "a"}},
		{"  get   a  ", []string{"get", "a"}},
		{"\tHGET\th f\n", []string{"HGET", "h", "f"}},
		{`SET "a b" 'c d'`, []string{"SET", "a b", "c d"}},
		{`GET "q\"x\\y"`, []string{"GET", `q"x\y`}},
		{`GET "\x41\n\t"`, []string{"GET", "A\n\t"}},
		{`GET 'it''s'`, []string{"GET", "its"}},
		{`GET 'a\'b'`, []string{"GET", "a'b"}},
		{`GET a"b c"`, []string{"GET", "ab c"}},
		{`GET ""`, []string{"GET", ""}},
		{"", nil},
		{"   ", nil},
	}
	for _, tt := range tests {
		got, err := splitCommand(tt.in)
		if err != nil {
			t.Errorf("splitCommand(%q): %v", tt.in, err)
			continue
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("splitCommand(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{`GET "unterminated`, `GET 'open`, `GET "\x4"`, `GET "a"b"`} {
		if _, err := splitCommand(bad); err == nil {
			t.Errorf("splitCommand(%q): want an error", bad)
		}
	}
}

// TestReadOnlyAllowed lists commands the read-only allow-list accepts.
func TestReadOnlyAllowed(t *testing.T) {
	for _, args := range [][]string{
		{"GET", "k"}, {"get", "k"}, {"MGET", "a", "b"}, {"STRLEN", "k"}, {"GETRANGE", "k", "0", "9"},
		{"HGET", "h", "f"}, {"HMGET", "h", "f"}, {"HGETALL", "h"}, {"HKEYS", "h"}, {"HVALS", "h"}, {"HLEN", "h"},
		{"HSCAN", "h", "0"}, {"LRANGE", "l", "0", "-1"}, {"LLEN", "l"}, {"LINDEX", "l", "0"},
		{"SMEMBERS", "s"}, {"SCARD", "s"}, {"SISMEMBER", "s", "m"}, {"SSCAN", "s", "0"},
		{"ZRANGE", "z", "0", "-1"}, {"ZRANGEBYSCORE", "z", "-inf", "+inf"}, {"ZREVRANGE", "z", "0", "1"},
		{"ZCARD", "z"}, {"ZSCORE", "z", "m"}, {"ZSCAN", "z", "0"},
		{"XRANGE", "x", "-", "+"}, {"XREVRANGE", "x", "+", "-"}, {"XLEN", "x"},
		{"XINFO", "STREAM", "x"}, {"xinfo", "groups", "x"}, {"XINFO", "CONSUMERS", "x", "g"}, {"XINFO", "HELP"},
		{"TYPE", "k"}, {"TTL", "k"}, {"PTTL", "k"}, {"EXISTS", "k"}, {"SCAN", "0"}, {"SCAN", "0", "COUNT", "10000"},
		{"DBSIZE"}, {"INFO"}, {"INFO", "keyspace"}, {"JSON.GET", "j"}, {"json.type", "j"},
		{"MEMORY", "USAGE", "k"}, {"memory", "usage", "k"}, {"OBJECT", "ENCODING", "k"},
		{"FT.SEARCH", "idx", "*"}, {"FT.INFO", "idx"}, {"FT._LIST"},
	} {
		if err := checkReadOnly(args); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want allowed", args, err)
		}
	}
}

// TestReadOnlyRefused probes every write family, the script, transaction,
// module and admin commands, and command-renaming tricks. Each must fail
// closed with ErrReadOnly.
func TestReadOnlyRefused(t *testing.T) {
	for _, args := range [][]string{
		// scripting and functions
		{"EVAL", "redis.call('del','k')", "0"}, {"EVALSHA", "abc", "0"}, {"EVAL_RO", "return 1", "0"},
		{"EVALSHA_RO", "abc", "0"}, {"FCALL", "f", "0"}, {"FCALL_RO", "f", "0"},
		{"FUNCTION", "LOAD", "#!lua name=x\n"}, {"FUNCTION", "LIST"}, {"FUNCTION", "FLUSH"},
		{"SCRIPT", "LOAD", "return 1"}, {"SCRIPT", "FLUSH"},
		// transactions and session state
		{"MULTI"}, {"EXEC"}, {"DISCARD"}, {"WATCH", "k"}, {"SELECT", "1"}, {"SWAPDB", "0", "1"},
		{"RESET"}, {"HELLO", "3"}, {"AUTH", "x"}, {"CLIENT", "KILL", "ID", "1"}, {"CLIENT", "LIST"},
		{"READWRITE"}, {"QUIT"},
		// configuration, modules, admin
		{"CONFIG", "SET", "notify-keyspace-events", "KEA"}, {"CONFIG", "GET", "*"}, {"CONFIG", "REWRITE"},
		{"MODULE", "LOAD", "/x.so"}, {"MODULE", "LIST"}, {"DEBUG", "SLEEP", "1"}, {"DEBUG", "OBJECT", "k"},
		{"FLUSHALL"}, {"FLUSHDB"}, {"flushall", "ASYNC"}, {"SHUTDOWN"}, {"SAVE"}, {"BGSAVE"}, {"BGREWRITEAOF"},
		{"REPLICAOF", "h", "1"}, {"SLAVEOF", "NO", "ONE"}, {"ACL", "SETUSER", "u"}, {"ACL", "LIST"},
		{"LATENCY", "RESET"}, {"SLOWLOG", "RESET"}, {"CLUSTER", "RESET"}, {"FAILOVER"}, {"MONITOR"},
		{"SYNC"}, {"PSYNC", "?", "-1"}, {"COMMAND"}, {"KEYS", "*"}, {"WAIT", "1", "0"},
		// strings and keys
		{"SET", "k", "v"}, {"SETNX", "k", "v"}, {"SETEX", "k", "1", "v"}, {"PSETEX", "k", "1", "v"},
		{"GETSET", "k", "v"}, {"GETDEL", "k"}, {"GETEX", "k", "PERSIST"}, {"MSET", "k", "v"}, {"MSETNX", "k", "v"},
		{"APPEND", "k", "v"}, {"SETRANGE", "k", "0", "v"}, {"INCR", "k"}, {"INCRBY", "k", "1"},
		{"INCRBYFLOAT", "k", "1"}, {"DECR", "k"}, {"DECRBY", "k", "1"}, {"SETBIT", "k", "0", "1"},
		{"BITOP", "AND", "d", "a"}, {"BITFIELD", "k", "SET", "u8", "0", "1"}, {"BITFIELD_RO", "k", "GET", "u8", "0"},
		{"DEL", "k"}, {"UNLINK", "k"}, {"EXPIRE", "k", "1"}, {"PEXPIRE", "k", "1"}, {"EXPIREAT", "k", "1"},
		{"PERSIST", "k"}, {"RENAME", "a", "b"}, {"RENAMENX", "a", "b"}, {"COPY", "a", "b"}, {"MOVE", "k", "1"},
		{"MIGRATE", "h", "1", "k", "0", "1"}, {"RESTORE", "k", "0", "x"}, {"DUMP", "k"}, {"TOUCH", "k"},
		{"OBJECT", "FREQ", "k"}, {"OBJECT", "HELP"}, {"OBJECT"}, {"SORT", "k", "STORE", "d"}, {"SORT_RO", "k"},
		{"LCS", "a", "b"},
		// hashes
		{"HSET", "h", "f", "v"}, {"HSETNX", "h", "f", "v"}, {"HMSET", "h", "f", "v"}, {"HDEL", "h", "f"},
		{"HINCRBY", "h", "f", "1"}, {"HINCRBYFLOAT", "h", "f", "1"}, {"HEXPIRE", "h", "1", "FIELDS", "1", "f"},
		{"HPERSIST", "h", "FIELDS", "1", "f"}, {"HGETDEL", "h", "FIELDS", "1", "f"}, {"HGETEX", "h", "FIELDS", "1", "f"},
		{"HSETEX", "h", "FIELDS", "1", "f", "v"},
		// lists
		{"LPUSH", "l", "v"}, {"RPUSH", "l", "v"}, {"LPUSHX", "l", "v"}, {"RPUSHX", "l", "v"}, {"LPOP", "l"},
		{"RPOP", "l"}, {"LSET", "l", "0", "v"}, {"LREM", "l", "0", "v"}, {"LTRIM", "l", "0", "1"},
		{"LINSERT", "l", "BEFORE", "a", "b"}, {"LMOVE", "a", "b", "LEFT", "RIGHT"}, {"RPOPLPUSH", "a", "b"},
		{"BLPOP", "l", "0"}, {"BRPOP", "l", "0"}, {"BLMOVE", "a", "b", "LEFT", "LEFT", "0"}, {"LMPOP", "1", "l", "LEFT"},
		// sets
		{"SADD", "s", "m"}, {"SREM", "s", "m"}, {"SPOP", "s"}, {"SMOVE", "a", "b", "m"},
		{"SINTERSTORE", "d", "a"}, {"SUNIONSTORE", "d", "a"}, {"SDIFFSTORE", "d", "a"},
		// sorted sets
		{"ZADD", "z", "1", "m"}, {"ZINCRBY", "z", "1", "m"}, {"ZREM", "z", "m"}, {"ZPOPMIN", "z"},
		{"ZPOPMAX", "z"}, {"BZPOPMIN", "z", "0"}, {"ZREMRANGEBYSCORE", "z", "0", "1"},
		{"ZREMRANGEBYRANK", "z", "0", "1"}, {"ZREMRANGEBYLEX", "z", "-", "+"}, {"ZUNIONSTORE", "d", "1", "z"},
		{"ZINTERSTORE", "d", "1", "z"}, {"ZDIFFSTORE", "d", "1", "z"}, {"ZRANGESTORE", "d", "z", "0", "1"},
		{"ZMPOP", "1", "z", "MIN"},
		// streams
		{"XADD", "x", "*", "f", "v"}, {"XDEL", "x", "1-1"}, {"XTRIM", "x", "MAXLEN", "0"},
		{"XGROUP", "CREATE", "x", "g", "$"}, {"XREADGROUP", "GROUP", "g", "c", "STREAMS", "x", ">"},
		{"XACK", "x", "g", "1-1"}, {"XCLAIM", "x", "g", "c", "0", "1-1"}, {"XAUTOCLAIM", "x", "g", "c", "0", "0"},
		{"XSETID", "x", "1-1"}, {"XREAD", "STREAMS", "x", "0"}, {"XINFO", "BOGUS", "x"}, {"XINFO"},
		// geo, hyperloglog, pub/sub
		{"GEOADD", "g", "1", "1", "m"}, {"GEORADIUS", "g", "1", "1", "1", "km", "STORE", "d"},
		{"GEOSEARCHSTORE", "d", "g", "FROMLONLAT", "1", "1", "BYRADIUS", "1", "km"},
		{"PFADD", "p", "a"}, {"PFMERGE", "d", "p"}, {"PFCOUNT", "p"},
		{"PUBLISH", "c", "m"}, {"SUBSCRIBE", "c"}, {"PSUBSCRIBE", "*"}, {"SPUBLISH", "c", "m"},
		// module write commands
		{"JSON.SET", "j", "$", "1"}, {"JSON.DEL", "j"}, {"JSON.FORGET", "j"}, {"JSON.MERGE", "j", "$", "{}"},
		{"JSON.NUMINCRBY", "j", "$", "1"}, {"JSON.ARRAPPEND", "j", "$", "1"}, {"JSON.CLEAR", "j"},
		{"FT.CREATE", "i", "SCHEMA", "f", "TEXT"}, {"FT.DROPINDEX", "i"}, {"FT.ALTER", "i"},
		{"FT.ALIASADD", "a", "i"}, {"FT.SUGADD", "k", "s", "1"}, {"FT.DICTADD", "d", "t"},
		{"FT.SYNUPDATE", "i", "g", "t"}, {"FT.AGGREGATE", "i", "*"}, {"FT.CURSOR", "DEL", "i", "1"},
		{"TS.ADD", "t", "*", "1"}, {"TS.CREATE", "t"}, {"BF.ADD", "b", "x"}, {"VADD", "v", "VALUES", "1", "1", "e"},
		// renaming and spelling tricks
		{"GET\x00", "k"}, {" GET", "k"}, {"GET ", "k"}, {"GET DEL", "k"}, {"G\u0415T", "k"},
		{"\uff27\uff25\uff34", "k"}, {"GET\u200b", "k"}, {"GE\tT", "k"}, {"JSON.GETX", "j"}, {"JSON.GET.", "j"},
		{"FT.SEARCHX", "i", "*"}, {"JSON", "GET", "j"}, {"MEMORY", "PURGE"}, {"MEMORY", "DOCTOR"}, {"MEMORY"},
		{"MEMORY", "USAGEX", "k"}, {"OBJECT", "ENCODINGX", "k"}, {"INFO\r\nFLUSHALL"}, {"DEL\r\n"},
		{"SCAN", "0", "COUNT", "10001"}, {"HSCAN", "h", "0", "COUNT", "999999999"}, {"SCAN", "0", "count", "-1"},
		{"SCAN", "0", "COUNT", "x"},
	} {
		err := checkReadOnly(args)
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkReadOnly(%q) = %v, want ErrReadOnly", args, err)
			continue
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("checkReadOnly(%q): want a *driver.Error from %q, got %#v", args, Name, err)
		}
	}
}

func TestCheckSession(t *testing.T) {
	for _, args := range [][]string{
		{"SELECT", "1"}, {"select", "1"}, {"MULTI"}, {"EXEC"}, {"DISCARD"}, {"WATCH", "k"}, {"UNWATCH"},
		{"SUBSCRIBE", "c"}, {"PSUBSCRIBE", "*"}, {"SSUBSCRIBE", "c"}, {"UNSUBSCRIBE"}, {"PUNSUBSCRIBE"},
		{"SUNSUBSCRIBE"}, {"MONITOR"}, {"SYNC"}, {"PSYNC", "?", "-1"}, {"QUIT"}, {"RESET"}, {"HELLO", "3"},
		{"AUTH", "x"}, {"READONLY"}, {"READWRITE"}, {"CLIENT", "REPLY", "OFF"}, {"CLIENT", "TRACKING", "ON"},
		{"CLIENT", "SETNAME", "x"}, {"CLIENT", "NO-EVICT", "ON"}, {"BLPOP", "l", "0"}, {"BRPOP", "l", "0"},
		{"BRPOPLPUSH", "a", "b", "0"}, {"BLMOVE", "a", "b", "LEFT", "LEFT", "0"}, {"BLMPOP", "0", "1", "l", "LEFT"},
		{"BZPOPMIN", "z", "0"}, {"BZPOPMAX", "z", "0"}, {"BZMPOP", "0", "1", "z", "MIN"}, {"WAIT", "1", "0"},
		{"WAITAOF", "0", "0", "0"}, {"XREAD", "BLOCK", "0", "STREAMS", "x", "$"},
		{"XREADGROUP", "GROUP", "g", "c", "block", "0", "STREAMS", "x", ">"},
	} {
		if err := checkSession(args); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("checkSession(%q) = %v, want ErrUnsupported", args, err)
		}
	}
	for _, args := range [][]string{
		{"GET", "k"}, {"SET", "k", "v"}, {"CLIENT", "LIST"}, {"CLIENT", "INFO"}, {"XREAD", "STREAMS", "x", "0"},
		{"DEL", "k"}, {"EVAL", "return 1", "0"},
	} {
		if err := checkSession(args); err != nil {
			t.Errorf("checkSession(%q) = %v, want allowed", args, err)
		}
	}
}

func TestCommandInfoName(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"get", "k"}, "get"},
		{[]string{"MEMORY", "usage", "k"}, "memory|usage"},
		{[]string{"object", "ENCODING", "k"}, "object|encoding"},
		{[]string{"XINFO", "STREAM", "x"}, "xinfo|stream"},
		{[]string{"JSON.GET", "j"}, "json.get"},
		{[]string{"FT.SEARCH", "i", "*"}, "ft.search"},
		{[]string{"INFO", "server"}, "info"},
	} {
		if got := commandInfoName(tt.args); got != tt.want {
			t.Errorf("commandInfoName(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestFlagsRefused(t *testing.T) {
	for _, tt := range []struct {
		flags []string
		want  bool
	}{
		{[]string{"readonly", "fast"}, false},
		{[]string{"loading", "stale"}, false}, // INFO
		{[]string{"readonly", "module"}, false},
		{[]string{"write", "denyoom"}, true},
		{[]string{"WRITE"}, true},
		{[]string{"admin", "noscript"}, true},
		{[]string{"readonly", "may_replicate"}, true},
		{[]string{"pubsub"}, true},
		{[]string{"blocking", "readonly"}, true},
	} {
		if got := flagsRefused(tt.flags); got != tt.want {
			t.Errorf("flagsRefused(%q) = %v, want %v", tt.flags, got, tt.want)
		}
	}
}

// TestCheckSessionNonASCII: a name that is not plain ASCII fails closed in
// every mode, since Redis's strcasecmp lookup stops at NUL.
func TestCheckSessionNonASCII(t *testing.T) {
	for _, args := range [][]string{{"SELECT\x00x", "1"}, {"S\u0415LECT", "1"}, {" SELECT", "1"}, {"GET\r\n", "k"}, {"", "k"}} {
		if err := checkSession(args); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("checkSession(%q) = %v, want ErrSyntax", args, err)
		}
	}
}
