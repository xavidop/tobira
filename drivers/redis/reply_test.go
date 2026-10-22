package redis

import (
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func colNames(cols []driver.Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

func checkRows(t *testing.T, what string, got, want []driver.Row) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d rows %v, want %d %v", what, len(got), got, len(want), want)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("%s: row %d = %v, want %v", what, i, got[i], want[i])
		}
		for j := range want[i] {
			if !drivertest.EqualValue(want[i][j], got[i][j]) || want[i][j].Type() != got[i][j].Type() {
				t.Errorf("%s: row %d col %d = %s (%s), want %s (%s)", what, i, j, got[i][j], got[i][j].Type(), want[i][j], want[i][j].Type())
			}
		}
	}
}

func TestRenderReply(t *testing.T) {
	s := driver.String
	tests := []struct {
		name  string
		args  []string
		reply any
		cols  []string
		rows  []driver.Row
	}{
		{"scalar string", []string{"GET", "k"}, "v", []string{"value"}, []driver.Row{{s("v")}}},
		{"nil", []string{"GET", "k"}, nil, []string{"value"}, []driver.Row{{driver.Null()}}},
		{"int", []string{"STRLEN", "k"}, int64(3), []string{"value"}, []driver.Row{{driver.Int(3)}}},
		{"binary", []string{"GET", "k"}, "\xff", []string{"value"}, []driver.Row{{driver.Bytes([]byte{0xff})}}},
		{"hgetall", []string{"HGETALL", "h"}, []any{"a", "1", "b", "2"}, []string{"field", "value"},
			[]driver.Row{{s("a"), s("1")}, {s("b"), s("2")}}},
		{"mget", []string{"MGET", "a", "b"}, []any{"1", nil}, []string{"key", "value"},
			[]driver.Row{{s("a"), s("1")}, {s("b"), driver.Null()}}},
		{"hmget", []string{"hmget", "h", "f", "g"}, []any{"1", nil}, []string{"field", "value"},
			[]driver.Row{{s("f"), s("1")}, {s("g"), driver.Null()}}},
		{"lrange", []string{"LRANGE", "l", "0", "-1"}, []any{"x", "y"}, []string{"value"},
			[]driver.Row{{s("x")}, {s("y")}}},
		{"zrange withscores", []string{"ZRANGE", "z", "0", "-1", "WITHSCORES"}, []any{"m", "1.5", "n", "2"},
			[]string{"member", "score"}, []driver.Row{{s("m"), driver.Float(1.5)}, {s("n"), driver.Float(2)}}},
		{"zrange plain", []string{"ZRANGE", "z", "0", "-1"}, []any{"m"}, []string{"value"}, []driver.Row{{s("m")}}},
		{"scan", []string{"SCAN", "0"}, []any{"17", []any{"a", "b"}}, []string{"key"}, []driver.Row{{s("a")}, {s("b")}}},
		{"hscan", []string{"HSCAN", "h", "0"}, []any{"0", []any{"f", "v"}}, []string{"field", "value"}, []driver.Row{{s("f"), s("v")}}},
		{"zscan", []string{"ZSCAN", "z", "0"}, []any{"0", []any{"m", "3"}}, []string{"member", "score"}, []driver.Row{{s("m"), driver.Float(3)}}},
		{"sscan", []string{"SSCAN", "s", "0"}, []any{"0", []any{"m"}}, []string{"member"}, []driver.Row{{s("m")}}},
		{"xrange", []string{"XRANGE", "x", "-", "+"}, []any{[]any{"1-0", []any{"f", "v"}}}, []string{"id", "fields"},
			[]driver.Row{{s("1-0"), driver.ObjectValue(driver.Member{Key: "f", Value: s("v")})}}},
		{"info", []string{"INFO", "keyspace"}, "# Keyspace\r\ndb0:keys=1,expires=0\r\n", []string{"section", "key", "value"},
			[]driver.Row{{s("Keyspace"), s("db0"), s("keys=1,expires=0")}}},
		{"nested", []string{"XINFO", "STREAM", "x"}, []any{"length", int64(1), "groups", []any{}}, []string{"value"},
			[]driver.Row{{s("length")}, {driver.Int(1)}, {s("groups")}, {driver.List()}}},
		{"ft.search", []string{"FT.SEARCH", "i", "*"},
			[]any{int64(2), "p:1", []any{"id", "1", "age", "30"}, "p:2", []any{"id", "2", "name", "x"}},
			[]string{"_key", "id", "age", "name"},
			[]driver.Row{{s("p:1"), driver.Int(1), driver.Int(30), driver.Null()}, {s("p:2"), driver.Int(2), driver.Null(), s("x")}}},
		{"ft.search nocontent", []string{"FT.SEARCH", "i", "*", "NOCONTENT"}, []any{int64(1), "p:1"},
			[]string{"_key"}, []driver.Row{{s("p:1")}}},
		{"ft.search withscores", []string{"FT.SEARCH", "i", "*", "WITHSCORES"}, []any{int64(1), "p:1", "0.5", []any{"id", "1"}},
			[]string{"_key", "_score", "id"}, []driver.Row{{s("p:1"), driver.Float(0.5), driver.Int(1)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cols, rows, _ := renderReply(tt.args, tt.reply)
			if got := colNames(cols); !slices.Equal(got, tt.cols) {
				t.Fatalf("columns = %q, want %q", got, tt.cols)
			}
			checkRows(t, tt.name, rows, tt.rows)
		})
	}
}

func TestRenderReplyScanCursorNotice(t *testing.T) {
	_, _, notices := renderReply([]string{"SCAN", "0"}, []any{"17", []any{"a"}})
	if !slices.Contains(notices, "next cursor: 17") {
		t.Errorf("notices = %q, want the next cursor", notices)
	}
}

// TestRenderReplyRedacts: secrets in CONFIG GET and ACL replies are masked.
func TestRenderReplyRedacts(t *testing.T) {
	_, rows, _ := renderReply([]string{"CONFIG", "GET", "*"},
		[]any{"requirepass", "hunter2", "masterauth", "m4ster", "tls-key-file-pass", "kp", "my-secret-x", "sx", "maxmemory", "0"})
	got := map[string]string{}
	for _, r := range rows {
		k, _ := r[0].AsText()
		v, _ := r[1].AsText()
		got[k] = v
	}
	for _, k := range []string{"requirepass", "masterauth", "tls-key-file-pass", "my-secret-x"} {
		if got[k] != "xxxxx" {
			t.Errorf("CONFIG GET %s = %q, want masked", k, got[k])
		}
	}
	if got["maxmemory"] != "0" {
		t.Errorf("maxmemory = %q", got["maxmemory"])
	}
	_, rows, _ = renderReply([]string{"acl", "list"}, []any{"user default on #5e88489 >plain <old ~* &* +@all"})
	if s, _ := rows[0][0].AsText(); s != "user default on #xxxxx >xxxxx <xxxxx ~* &* +@all" {
		t.Errorf("ACL LIST = %q", s)
	}
	_, rows, _ = renderReply([]string{"ACL", "GETUSER", "u"},
		[]any{"flags", []any{"on"}, "passwords", []any{"5e88489", "abc"}, "commands", "+@all", "selectors", []any{}})
	flat := ""
	for _, r := range rows {
		flat += r[0].String() + " "
	}
	if strings.Contains(flat, "5e88489") || strings.Contains(flat, "abc") {
		t.Errorf("ACL GETUSER leaks password hashes: %s", flat)
	}
}

func TestSizeProbe(t *testing.T) {
	for _, tt := range []struct {
		args []string
		cmd  string // size command, "" = no check
	}{
		{[]string{"HGETALL", "h"}, "HLEN"}, {[]string{"hkeys", "h"}, "HLEN"}, {[]string{"HVALS", "h"}, "HLEN"},
		{[]string{"SMEMBERS", "s"}, "SCARD"}, {[]string{"LRANGE", "l", "0", "-1"}, "LLEN"},
		{[]string{"ZRANGE", "z", "0", "-1"}, "ZCARD"}, {[]string{"ZRANGEBYSCORE", "z", "-inf", "+inf"}, "ZCARD"},
		{[]string{"ZREVRANGE", "z", "0", "-1"}, "ZCARD"}, {[]string{"XRANGE", "x", "-", "+"}, "XLEN"},
		{[]string{"XREVRANGE", "x", "+", "-"}, "XLEN"}, {[]string{"GET", "k"}, "STRLEN"}, {[]string{"GETRANGE", "k", "0", "-1"}, "STRLEN"},
		{[]string{"MGET", "a", "b"}, "STRLEN"},
		{[]string{"XRANGE", "x", "-", "+", "COUNT", "10"}, ""}, {[]string{"ZRANGEBYSCORE", "z", "0", "1", "LIMIT", "0", "10"}, ""},
		{[]string{"HGET", "h", "f"}, ""}, {[]string{"SCARD", "s"}, ""},
	} {
		if got := sizeCommand(tt.args); got != tt.cmd {
			t.Errorf("sizeCommand(%q) = %q, want %q", tt.args, got, tt.cmd)
		}
	}
	for _, tt := range []struct {
		args []string
		size int64
		ok   bool
	}{
		{[]string{"LRANGE", "l", "0", "-1"}, maxReplyElements + 1, false},
		{[]string{"LRANGE", "l", "0", "99"}, maxReplyElements + 1, true},
		{[]string{"LRANGE", "l", "-100", "-1"}, maxReplyElements + 1, true},
		{[]string{"LRANGE", "l", "0", "-1"}, maxReplyElements, true},
		{[]string{"ZRANGE", "z", "0", "-1", "BYSCORE"}, maxReplyElements + 1, false},
		{[]string{"HGETALL", "h"}, maxReplyElements + 1, false},
		{[]string{"GET", "k"}, maxReplyBytes + 1, false},
		{[]string{"GET", "k"}, maxReplyBytes, true},
		{[]string{"GETRANGE", "k", "0", "1023"}, maxReplyBytes + 1, true},
	} {
		if got := withinBounds(tt.args, tt.size); got != tt.ok {
			t.Errorf("withinBounds(%q, %d) = %v, want %v", tt.args, tt.size, got, tt.ok)
		}
	}
}

// The generic value column carries the natural type of its values:
// GET is a string, INCR an int, LRANGE strings, a missing key's nil a
// string; only a reply that mixes types stays unknown.
func TestRenderReplyValueColumnType(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		reply any
		want  driver.Type
	}{
		{"get", []string{"GET", "k"}, "v", driver.TypeString},
		{"get missing", []string{"GET", "k"}, nil, driver.TypeString},
		{"get binary", []string{"GET", "k"}, "\xff", driver.TypeBytes},
		{"incr", []string{"INCR", "n"}, int64(4), driver.TypeInt},
		{"strlen", []string{"STRLEN", "k"}, int64(3), driver.TypeInt},
		{"lrange", []string{"LRANGE", "l", "0", "-1"}, []any{"x", nil, "y"}, driver.TypeString},
		{"sintercard", []string{"SINTERCARD", "1", "s"}, int64(0), driver.TypeInt},
		{"error reply", []string{"GET", "k"}, errString("WRONGTYPE"), driver.TypeString},
		{"mixed", []string{"XINFO", "STREAM", "x"}, []any{"length", int64(1)}, driver.TypeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cols, _, _ := renderReply(tt.args, tt.reply)
			if len(cols) != 1 || cols[0].Type != tt.want {
				t.Fatalf("columns = %+v, want one value column of type %s", cols, tt.want)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }
