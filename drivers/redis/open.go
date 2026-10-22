package redis

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	goredis "github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/xavidop/tobira/driver"
)

// Open parses the DSN and pings once, so a bad address or bad credentials
// fail here rather than on the first query.
func (redisDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	opts, err := buildOptions(cfg)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	c := newConn(opts, cfg.ReadOnly)
	cl, _, release, err := c.client("open", "")
	if err == nil {
		err = cl.Ping(ctx).Err()
		if err == nil {
			c.dbCount = databaseCount(ctx, cl)
		}
		release()
	}
	if err != nil {
		_ = c.Close()
		return nil, scrubError(mapError(ctx, "open", err), cfg)
	}
	return c, nil
}

// databaseCount is CONFIG GET databases, or 0 when the server refuses it.
func databaseCount(ctx context.Context, cl *goredis.Client) int {
	cfg, err := cl.ConfigGet(ctx, "databases").Result()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(cfg["databases"])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// buildOptions turns cfg into go-redis options without connecting. It
// accepts redis://, rediss:// (TLS) and unix:// URLs with go-redis's query
// parameters, then enforces what tobira relies on:
//
//   - RESP2 (protocol=2), the reply shapes the result renderer reads;
//   - no CLIENT SETINFO and no maintenance-notification handshake, so the
//     driver sends nothing the user did not ask for beyond PING;
//   - on a read-only connection, every new connection sends READONLY when
//     the server is a cluster replica (see readOnlyOnConnect);
//   - a non-empty ConnConfig.Password replaces the DSN's password.
func buildOptions(cfg driver.ConnConfig) (*goredis.Options, error) {
	fail := func(kind error, err error) (*goredis.Options, error) {
		return nil, driver.Wrap(kind, Name, "open", err)
	}
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return fail(driver.ErrUnsupported, fmt.Errorf("unsupported option %q: the redis driver takes its settings from DSN parameters", k))
	}
	dsn := cfg.DSN
	scheme := driver.Scheme(dsn)
	switch scheme {
	case "redis", "rediss", "unix":
		dsn = scheme + dsn[len(scheme):] // go-redis wants a lower-case scheme
	default:
		return fail(driver.ErrSyntax, fmt.Errorf("unsupported DSN scheme %q: want redis://, rediss:// or unix://", scheme))
	}
	opts, err := goredis.ParseURL(dsn)
	if err != nil {
		return fail(driver.ErrSyntax, fmt.Errorf("invalid DSN: %w", err))
	}
	opts.Protocol = 2
	opts.DisableIdentity = true
	opts.MaintNotificationsConfig = &maintnotifications.Config{Mode: maintnotifications.ModeDisabled}
	opts.ContextTimeoutEnabled = true
	if !cfg.Password.IsZero() {
		opts.Password = cfg.Password.Reveal()
	}
	if cfg.ReadOnly {
		opts.OnConnect = readOnlyOnConnect
	}
	return opts, nil
}

// readOnlyOnConnect sends READONLY on a new connection when the server is a
// cluster replica, so reads are served there and writes are refused by the
// server as well as by the guard.
func readOnlyOnConnect(ctx context.Context, cn *goredis.Conn) error {
	info, err := cn.Info(ctx, "cluster", "replication").Result()
	if err != nil {
		return nil // INFO may be refused by ACL; the allow-list still holds
	}
	if isClusterReplica(info) {
		return cn.ReadOnly(ctx).Err()
	}
	return nil
}

// isClusterReplica reports whether an INFO reply says cluster mode is on and
// this node is a replica.
func isClusterReplica(info string) bool {
	cluster, replica := false, false
	for line := range strings.SplitSeq(info, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), ":")
		switch k {
		case "cluster_enabled":
			cluster = v == "1"
		case "role":
			replica = v == "slave" || v == "replica"
		}
	}
	return cluster && replica
}

// scrubError masks every password of cfg in the message of a *driver.Error,
// keeping its kind, code and the original error (through Unwrap).
func scrubError(err error, cfg driver.ConnConfig) error {
	var de *driver.Error
	if !errors.As(err, &de) || de.Err == nil {
		return err
	}
	msg := de.Err.Error()
	if clean := driver.RedactMessage(msg, cfg); clean != msg {
		de.Err = maskedError{msg: clean, err: de.Err}
	}
	return err
}

// maskedError shows a masked message but keeps the original error reachable.
type maskedError struct {
	msg string
	err error
}

func (m maskedError) Error() string { return m.msg }
func (m maskedError) Unwrap() error { return m.err }
