package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// The read-only guard is fail-closed and has two layers:
//
//  1. checkReadOnly, before anything is sent: the command (with its
//     subcommand for MEMORY, OBJECT and XINFO) must be on readOnlyCommands,
//     spelled in plain ASCII. Everything else is refused with ErrReadOnly:
//     scripts (EVAL, FCALL and their _RO forms), FUNCTION, MULTI, CONFIG,
//     SELECT, MODULE, DEBUG, every write family and every module write.
//  2. verifyCommand, on the server: COMMAND INFO for the command must exist
//     and must not carry the write, admin, may_replicate, pubsub or blocking
//     flags. This catches a server whose rename-command gave a write
//     command an allow-listed name. When COMMAND INFO itself fails (an ACL
//     without +command), the call is refused.

// readOnlyCommands is the allow-list, in canonical form: the upper-case
// command, plus " SUBCOMMAND" for container commands.
var readOnlyCommands = map[string]bool{
	"GET": true, "MGET": true, "STRLEN": true, "GETRANGE": true,
	"HGET": true, "HMGET": true, "HGETALL": true, "HKEYS": true, "HVALS": true, "HLEN": true, "HSCAN": true,
	"LRANGE": true, "LLEN": true, "LINDEX": true,
	"SMEMBERS": true, "SCARD": true, "SISMEMBER": true, "SSCAN": true,
	"ZRANGE": true, "ZRANGEBYSCORE": true, "ZREVRANGE": true, "ZCARD": true, "ZSCORE": true, "ZSCAN": true,
	"XRANGE": true, "XREVRANGE": true, "XLEN": true,
	"XINFO STREAM": true, "XINFO GROUPS": true, "XINFO CONSUMERS": true, "XINFO HELP": true,
	"TYPE": true, "TTL": true, "PTTL": true, "EXISTS": true, "SCAN": true, "DBSIZE": true, "INFO": true,
	"JSON.GET": true, "JSON.TYPE": true, "MEMORY USAGE": true, "OBJECT ENCODING": true,
	// RediSearch reads: FT.SEARCH is how hash records are queried as rows.
	"FT.SEARCH": true, "FT.INFO": true, "FT._LIST": true,
}

// containerCommands take a subcommand that is part of the command's name.
var containerCommands = map[string]bool{
	"MEMORY": true, "OBJECT": true, "XINFO": true, "CONFIG": true, "CLIENT": true, "FUNCTION": true,
	"SCRIPT": true, "MODULE": true, "ACL": true, "CLUSTER": true, "COMMAND": true, "LATENCY": true,
	"SLOWLOG": true, "DEBUG": true, "XGROUP": true, "PUBSUB": true,
}

// maxScanCount caps the COUNT of a read-only SCAN, HSCAN, SSCAN or ZSCAN,
// so one call cannot turn into a KEYS.
const maxScanCount = 10000

// asciiUpper upper-cases s when it is printable ASCII without spaces, and
// reports false otherwise, so look-alike letters, control bytes and padded
// names never match an allow-list entry.
func asciiUpper(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	b := []byte(s)
	for i, c := range b {
		if c <= ' ' || c >= 0x7f {
			return "", false
		}
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b), true
}

// canonicalName returns the command's canonical name (see readOnlyCommands)
// and false when it is not plain ASCII.
func canonicalName(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	name, ok := asciiUpper(args[0])
	if !ok {
		return "", false
	}
	if containerCommands[name] {
		if len(args) < 2 {
			return name, true
		}
		sub, ok := asciiUpper(args[1])
		if !ok {
			return "", false
		}
		return name + " " + sub, true
	}
	return name, true
}

// commandInfoName is the name COMMAND INFO takes: lower case, with
// "container|sub" for container commands.
func commandInfoName(args []string) string {
	name, _ := canonicalName(args)
	return strings.ReplaceAll(strings.ToLower(name), " ", "|")
}

func readOnlyError(format string, a ...any) error {
	return driver.Wrap(driver.ErrReadOnly, Name, "query", fmt.Errorf(format, a...))
}

// checkReadOnly is the guard's first layer: nil when args is an
// allow-listed read, else ErrReadOnly.
func checkReadOnly(args []string) error {
	name, ok := canonicalName(args)
	if !ok {
		if len(args) == 0 {
			return readOnlyError("empty command")
		}
		return readOnlyError("command %q is not on the read-only allow-list", args[0])
	}
	if !readOnlyCommands[name] {
		return readOnlyError("%s is not on the read-only allow-list", name)
	}
	switch name {
	case "SCAN", "HSCAN", "SSCAN", "ZSCAN":
		if err := checkScanCount(args); err != nil {
			return err
		}
	}
	return nil
}

// checkScanCount refuses a COUNT above maxScanCount (or not a positive
// number) on a read-only scan.
func checkScanCount(args []string) error {
	for i := 1; i < len(args); i++ {
		if !strings.EqualFold(args[i], "COUNT") || i+1 >= len(args) {
			continue
		}
		n, err := strconv.Atoi(args[i+1])
		if err != nil || n <= 0 || n > maxScanCount {
			return readOnlyError("a read-only %s takes COUNT 1 to %d", strings.ToUpper(args[0]), maxScanCount)
		}
	}
	return nil
}

// sessionCommands change the state of the pooled connection they run on,
// block it, or switch it into a push mode. They are refused in every mode:
// the namespace selects the database, and Watch subscribes.
var sessionCommands = map[string]bool{
	"SELECT": true, "MULTI": true, "EXEC": true, "DISCARD": true, "WATCH": true, "UNWATCH": true,
	"SUBSCRIBE": true, "PSUBSCRIBE": true, "SSUBSCRIBE": true, "UNSUBSCRIBE": true, "PUNSUBSCRIBE": true,
	"SUNSUBSCRIBE": true, "MONITOR": true, "SYNC": true, "PSYNC": true, "QUIT": true, "RESET": true,
	"HELLO": true, "AUTH": true, "READONLY": true, "READWRITE": true,
	"CLIENT REPLY": true, "CLIENT TRACKING": true, "CLIENT SETNAME": true, "CLIENT NO-EVICT": true,
	"CLIENT NO-TOUCH": true, "CLIENT SETINFO": true, "CLIENT CACHING": true,
	"BLPOP": true, "BRPOP": true, "BRPOPLPUSH": true, "BLMOVE": true, "BLMPOP": true,
	"BZPOPMIN": true, "BZPOPMAX": true, "BZMPOP": true, "WAIT": true, "WAITAOF": true,
}

// checkSession refuses, with ErrUnsupported, commands that would change or
// hold the pooled connection (see sessionCommands), including XREAD and
// XREADGROUP with BLOCK. A name that is not plain printable ASCII is
// ErrSyntax: Redis looks commands up with strcasecmp, which stops at NUL,
// so such a name could still reach SELECT.
func checkSession(args []string) error {
	name, ok := canonicalName(args)
	if !ok {
		return driver.Wrap(driver.ErrSyntax, Name, "query", errors.New("the command name must be plain printable ASCII"))
	}
	refuse := func(why string) error {
		return driver.Wrap(driver.ErrUnsupported, Name, "query", fmt.Errorf("%s is not supported: %s", name, why))
	}
	if sessionCommands[name] {
		if name == "SELECT" {
			return refuse("choose the database with the namespace")
		}
		return refuse("it would change, block or take over the shared connection")
	}
	if name == "XREAD" || name == "XREADGROUP" {
		for _, a := range args[1:] {
			if strings.EqualFold(a, "BLOCK") {
				return refuse("BLOCK would hold the shared connection")
			}
			if strings.EqualFold(a, "STREAMS") {
				break
			}
		}
	}
	return nil
}

// refusedFlags are COMMAND INFO flags that disqualify a command from a
// read-only call.
var refusedFlags = map[string]bool{"write": true, "admin": true, "may_replicate": true, "pubsub": true, "blocking": true}

func flagsRefused(flags []string) bool {
	for _, f := range flags {
		if refusedFlags[strings.ToLower(f)] {
			return true
		}
	}
	return false
}

// verifyCommand is the guard's second layer: it asks the server, once per
// command name and connection, for the command's flags.
func (c *conn) verifyCommand(ctx context.Context, cl *goredis.Client, args []string) error {
	name := commandInfoName(args)
	c.flagsMu.Lock()
	refused, cached := c.flags[name]
	c.flagsMu.Unlock()
	if !cached {
		flags, err := commandFlags(ctx, cl, name)
		if err != nil {
			if ctx.Err() != nil {
				return mapError(ctx, "query", ctx.Err())
			}
			return readOnlyError("cannot verify %s with COMMAND INFO: %v", strings.ToUpper(name), err)
		}
		refused = flags == nil || flagsRefused(flags)
		c.flagsMu.Lock()
		c.flags[name] = refused
		c.flagsMu.Unlock()
	}
	if refused {
		return readOnlyError("the server does not report %s as a read-only command", strings.ToUpper(name))
	}
	return nil
}

// commandFlags returns the flags COMMAND INFO reports for name, or nil when
// the server does not know the command.
func commandFlags(ctx context.Context, cl *goredis.Client, name string) ([]string, error) {
	reply, err := cl.Do(ctx, "COMMAND", "INFO", name).Slice()
	if err != nil {
		return nil, err
	}
	if len(reply) != 1 || reply[0] == nil {
		return nil, nil
	}
	entry, ok := reply[0].([]any)
	if !ok || len(entry) < 3 {
		return nil, errors.New("unexpected COMMAND INFO reply")
	}
	raw, ok := entry[2].([]any)
	if !ok {
		return nil, errors.New("unexpected COMMAND INFO flags")
	}
	flags := make([]string, 0, len(raw))
	for _, f := range raw {
		s, ok := f.(string)
		if !ok {
			return nil, errors.New("unexpected COMMAND INFO flag")
		}
		flags = append(flags, s)
	}
	return flags, nil
}
