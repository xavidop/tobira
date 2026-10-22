package redis

import (
	"context"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// Query runs one native command line, such as `HGETALL user:1` or
// `FT.SEARCH idx "@age:[30 40]"`. Arguments split as in redis-cli (see
// splitCommand). Commands that would change or hold the shared connection
// (SELECT, MULTI, SUBSCRIBE, blocking pops...) are refused in every mode.
// A read-only call (Query.ReadOnly or a read-only connection) must pass
// both layers of the read-only guard (see guard.go). The reply is shown as
// a table (see renderReply); Query.Limit truncates it.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	args, err := splitCommand(q.Text)
	if err != nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "query", err)
	}
	if len(args) == 0 {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "query", errors.New("empty command"))
	}
	if len(q.Params) > 0 {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("redis commands take no parameters: put the values in the command line"))
	}
	readOnly := q.ReadOnly || c.readOnly
	if readOnly { // first, so a refused write is always ErrReadOnly
		if err := checkReadOnly(args); err != nil {
			return nil, err
		}
	}
	if err := checkSession(args); err != nil {
		return nil, err
	}
	cl, _, release, err := c.client("query", q.Namespace)
	if err != nil {
		return nil, err
	}
	defer release()
	if readOnly {
		if err := c.verifyCommand(ctx, cl, args); err != nil {
			return nil, err
		}
	}
	if err := checkReplySize(ctx, cl, args); err != nil {
		return nil, err
	}
	cmdArgs := make([]any, len(args))
	for i, a := range args {
		cmdArgs[i] = a
	}
	reply, err := cl.Do(ctx, cmdArgs...).Result()
	if errors.Is(err, goredis.Nil) {
		reply, err = nil, nil
	}
	if err != nil {
		return nil, mapError(ctx, "query", err)
	}
	cols, rows, notices := renderReply(args, reply)
	truncated := false
	if q.Limit > 0 && len(rows) > q.Limit {
		rows, truncated = rows[:q.Limit], true
	}
	return driver.NewSliceResultSet(cols, rows, driver.Stats{
		RowsAffected: -1, Duration: time.Since(start), Truncated: truncated, Notices: notices,
	}), nil
}
