package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// Allow-listed reads that return a whole value are checked before they
// run, in every mode: the size command (HLEN, SCARD, LLEN, ZCARD, XLEN or
// STRLEN) must report at most maxReplyElements elements or maxReplyBytes
// bytes for the range asked. Otherwise the reply would be read whole into
// memory before Query.Limit could cut it.
const (
	maxReplyElements = 100000
	maxReplyBytes    = 16 << 20
)

const tooLargeHint = "too large; use HSCAN/SSCAN/ZSCAN or a ranged command"

// sizeCommand names the command that sizes the reply of args, or "" when
// args needs no check (a bounded read, or one with COUNT or LIMIT).
func sizeCommand(args []string) string {
	name, _ := canonicalName(args)
	if len(args) < 2 {
		return ""
	}
	switch name {
	case "HGETALL", "HKEYS", "HVALS":
		return "HLEN"
	case "SMEMBERS":
		return "SCARD"
	case "LRANGE":
		return "LLEN"
	case "ZRANGE", "ZRANGEBYSCORE", "ZREVRANGE":
		if hasArg(args, "LIMIT") {
			return ""
		}
		return "ZCARD"
	case "XRANGE", "XREVRANGE":
		if hasArg(args, "COUNT") {
			return ""
		}
		return "XLEN"
	case "GET", "GETRANGE", "MGET":
		return "STRLEN"
	}
	return ""
}

// withinBounds reports whether a reply of args from a value of the given
// size (elements, or bytes for STRLEN; the sum over keys for MGET) stays
// within the limits. Index ranges are measured as Redis normalises them;
// unparsable arguments pass, since the server rejects them.
func withinBounds(args []string, size int64) bool {
	name, _ := canonicalName(args)
	switch name {
	case "GET", "MGET":
		return size <= maxReplyBytes
	case "GETRANGE":
		return rangeLen(args, size) <= maxReplyBytes
	case "LRANGE", "ZREVRANGE":
		return rangeLen(args, size) <= maxReplyElements
	case "ZRANGE":
		if hasArg(args, "BYSCORE") || hasArg(args, "BYLEX") {
			return size <= maxReplyElements
		}
		return rangeLen(args, size) <= maxReplyElements
	}
	return size <= maxReplyElements
}

// rangeLen is the number of elements args[2]..args[3] selects from n.
func rangeLen(args []string, n int64) int64 {
	if len(args) < 4 {
		return 0
	}
	start, err1 := strconv.ParseInt(args[2], 10, 64)
	end, err2 := strconv.ParseInt(args[3], 10, 64)
	if err1 != nil || err2 != nil {
		return 0
	}
	if start < 0 {
		start = max(start+n, 0)
	}
	if end < 0 {
		end += n
	}
	end = min(end, n-1)
	if start > end {
		return 0
	}
	return end - start + 1
}

// checkReplySize runs the size command for args and refuses a reply that
// would be too large with ErrLimit. A failing size command (a wrong
// type, a missing key) lets the real command run and report its own error.
func checkReplySize(ctx context.Context, cl *goredis.Client, args []string) error {
	cmd := sizeCommand(args)
	if cmd == "" {
		return nil
	}
	keys := args[1:2]
	if name, _ := canonicalName(args); name == "MGET" {
		keys = args[1:]
	}
	cmds := make([]*goredis.Cmd, len(keys))
	_, err := cl.Pipelined(ctx, func(p goredis.Pipeliner) error {
		for i, k := range keys {
			cmds[i] = p.Do(ctx, cmd, k)
		}
		return nil
	})
	if ctx.Err() != nil {
		return mapError(ctx, "query", ctx.Err())
	}
	if err != nil {
		return nil
	}
	var size int64
	for _, c := range cmds {
		n, _ := c.Int64()
		size += n
	}
	if !withinBounds(args, size) {
		return replyTooLarge(args, cmd, size)
	}
	return nil
}

// replyTooLarge is the ErrLimit of a reply over the bounds: size elements,
// or bytes for STRLEN, as cmd measured them.
func replyTooLarge(args []string, cmd string, size int64) error {
	unit := "elements"
	if cmd == "STRLEN" {
		unit = "bytes"
	}
	return driver.Wrap(driver.ErrLimit, Name, "query",
		fmt.Errorf("%s of %d %s is %s", strings.ToUpper(args[0]), size, unit, tooLargeHint))
}
