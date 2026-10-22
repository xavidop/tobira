package postgres

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlcomplete"
)

const (
	defaultApplicationName = "tobira"
	readOnlyParam          = "default_transaction_read_only"
	redactedMark           = "xxxxx"
	// resetTimeout bounds DISCARD ALL when a connection returns to the pool.
	resetTimeout = 2 * time.Second
	// cancelDeadlineDelay is how long a cancelled statement may take to
	// answer the cancel request before the socket deadline closes it.
	cancelDeadlineDelay = 2 * time.Second
	// closeGrace is how long Close waits for the pool to drain on its own: a
	// release that hits a hung server ends after resetTimeout plus
	// cancelDeadlineDelay. After that Close drops the remaining sockets, so it
	// never waits for pgx's own 15 s cleanup deadlines.
	closeGrace = resetTimeout + cancelDeadlineDelay
)

// allowedOptions are the ConnConfig.Options keys that become session runtime
// parameters. Everything else is rejected so options cannot weaken the
// read-only guard.
var allowedOptions = map[string]bool{
	"application_name":  true,
	"search_path":       true,
	"statement_timeout": true,
}

// Open builds a pgxpool from cfg and pings it once, so a bad DSN or bad
// credentials fail here rather than on the first query.
func (d *postgresDriver) Open(ctx context.Context, cfg driver.ConnConfig) (_ driver.Conn, err error) {
	name := d.Info().Name
	defer func() { err = (&conn{name: name}).named(err) }()
	pcfg, fl, err := buildConfig(cfg, d.flavor)
	if err != nil {
		return nil, err
	}
	socks := trackSockets(pcfg)
	pool, err := d.connect(ctx, pcfg)
	if err != nil {
		socks.closeAll()
		return nil, scrubError(mapError(ctx, "open", err), cfg, pcfg.ConnConfig.Password)
	}
	c := &conn{pool: pool, socks: socks, readOnly: cfg.ReadOnly, embedder: cfg.Embedder, flavor: fl.get(), name: name}
	if c.flavor == "" {
		c.flavor = flavorPostgres
		if d.detect != nil {
			f, err := d.detect(ctx, c)
			if err != nil {
				pool.Close()
				socks.closeAll()
				return nil, scrubError(mapError(ctx, "open", err), cfg, pcfg.ConnConfig.Password)
			}
			c.flavor = f
		}
		fl.set(c.flavor)
	}
	if d.probeVector != nil && c.flavor.has(driver.CapVectorSearcher) {
		c.vectorSchema = d.probeVector(ctx, pool)
	}
	c.catalog = sqlcomplete.NewCache(catalogReader{c}, sqlcomplete.DefaultTTL)
	return c, nil
}

// flavorBox holds a pool's flavor for its AfterRelease hook, which is built
// before the server could be asked. Until Open sets it, it reads "".
type flavorBox struct{ v atomic.Value }

func (b *flavorBox) get() flavor {
	f, _ := b.v.Load().(flavor)
	return f
}

func (b *flavorBox) set(f flavor) { b.v.Store(f) }

// connectPool creates the pool (pgxpool connects lazily) and pings it.
func connectPool(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// buildConfig turns cfg into a pool config without connecting. forced is
// the driver's flavor ("" for none); the DSN may name one too (a flavored
// scheme or flavor=…). The returned box holds the chosen flavor, or "" when
// Open must ask the server.
func buildConfig(cfg driver.ConnConfig, forced flavor) (*pgxpool.Config, *flavorBox, error) {
	fail := func(kind error, err error) (*pgxpool.Config, *flavorBox, error) {
		return nil, nil, driver.Wrap(kind, Name, "open", err)
	}
	for _, k := range slices.Sorted(maps.Keys(cfg.Options)) {
		if !allowedOptions[k] {
			return fail(driver.ErrUnsupported, fmt.Errorf("unsupported option %q (allowed: application_name, search_path, statement_timeout)", k))
		}
	}
	dsn, f, err := splitFlavor(cfg.DSN, forced)
	if err != nil {
		return nil, nil, scrubError(err, cfg, "")
	}
	box := &flavorBox{}
	box.set(f)
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fail(driver.ErrSyntax, errors.New("invalid DSN: "+scrub(scrub(err.Error(), driver.ConnConfig{DSN: dsn}, ""), cfg, "")))
	}
	if err := forceExtendedProtocol(pcfg); err != nil {
		return nil, nil, err
	}
	pcfg.AfterRelease = func(c *pgx.Conn) bool { return resetSession(c, box.get()) }
	// Cancelling a statement sends a cancel request and keeps the connection,
	// instead of pgx's default of closing the socket.
	pcfg.ConnConfig.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, DeadlineDelay: cancelDeadlineDelay}
	}
	rp := pcfg.ConnConfig.RuntimeParams
	if rp == nil {
		rp = map[string]string{}
		pcfg.ConnConfig.RuntimeParams = rp
	}
	if !cfg.Password.IsZero() {
		pcfg.ConnConfig.Password = cfg.Password.Reveal()
	}
	maps.Copy(rp, cfg.Options)
	if rp["application_name"] == "" {
		rp["application_name"] = defaultApplicationName
	}
	if cfg.ReadOnly {
		// Postgres matches GUC names case-insensitively, so drop every
		// spelling before setting the one we mean.
		for k := range rp {
			if strings.EqualFold(k, readOnlyParam) {
				delete(rp, k)
			}
		}
		for k, v := range rp {
			norm := strings.ReplaceAll(strings.ToLower(v), "-", "_")
			if strings.EqualFold(k, "options") && strings.Contains(norm, "read_only") {
				return fail(driver.ErrUnsupported, errors.New("DSN parameter \"options\" must not set read-only GUCs on a read-only connection"))
			}
		}
		if f.startupReadOnly() {
			rp[readOnlyParam] = "on"
		}
	}
	return pcfg, box, nil
}

// resetSession runs DISCARD ALL on every connection returned to the pool, so
// each call starts from a clean session: advisory locks, prepared statements,
// LISTEN, temp tables and any SET/RESET or SET ROLE done by earlier query text
// are gone. DISCARD ALL restores the startup-packet values, so
// default_transaction_read_only=on survives on read-only connections. If the
// reset fails the pool destroys the connection.
//
// Redshift has no DISCARD, so its connections get RESET ALL, which restores
// the settings but not the rest (see the Redshift docs page).
func resetSession(conn *pgx.Conn, f flavor) bool {
	ctx, cancel := context.WithTimeout(context.Background(), resetTimeout)
	defer cancel()
	for _, s := range f.resetStatements() {
		if _, err := conn.Exec(ctx, s); err != nil {
			return false
		}
	}
	return true
}

// forceExtendedProtocol pins the pool to an extended-protocol query mode. The
// server rejects a multi-command string sent through Parse, which is what
// makes "one statement per query" hold. Simple-protocol modes would accept
// several statements, so a DSN asking for them is refused.
func forceExtendedProtocol(pcfg *pgxpool.Config) error {
	cc := pcfg.ConnConfig
	switch cc.DefaultQueryExecMode {
	case pgx.QueryExecModeSimpleProtocol, pgx.QueryExecModeExec:
		return driver.Wrap(driver.ErrUnsupported, Name, "open",
			fmt.Errorf("default_query_exec_mode=%s is not supported: tobira needs the extended protocol to enforce one statement per query", cc.DefaultQueryExecMode))
	case pgx.QueryExecModeCacheStatement:
		// Named prepared statements go stale across schema changes and break
		// behind transaction poolers; a cached description does neither.
		cc.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	}
	if cc.DefaultQueryExecMode == pgx.QueryExecModeCacheDescribe && cc.DescriptionCacheCapacity <= 0 {
		cc.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	}
	return nil
}

var keywordPassword = regexp.MustCompile(`(?i)password\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`)

// dsnSecrets returns every substring of the DSN and password that must never
// be printed: the URL userinfo password, keyword/value passwords, their
// URL-decoded forms and the explicit password.
func dsnSecrets(cfg driver.ConnConfig, extra string) []string {
	var out []string
	add := func(s string) {
		if s = strings.Trim(s, `'`); s != "" {
			out = append(out, s)
			if u, err := url.PathUnescape(s); err == nil && u != s {
				out = append(out, u)
			}
		}
	}
	add(cfg.Password.Reveal())
	add(extra)
	dsn := cfg.DSN
	if sc := driver.Scheme(dsn); sc != "" && strings.HasPrefix(dsn[len(sc):], "://") {
		rest := dsn[len(sc)+3:]
		if at := strings.LastIndexByte(rest, '@'); at >= 0 {
			if colon := strings.IndexByte(rest, ':'); colon >= 0 && colon < at {
				add(rest[colon+1 : at])
			}
		}
	}
	for _, m := range keywordPassword.FindAllStringSubmatch(dsn, -1) {
		add(m[1])
	}
	// Longest first so a secret that contains another is masked whole.
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// scrub masks the DSN password and any explicit password in msg.
func scrub(msg string, cfg driver.ConnConfig, extra string) string {
	if cfg.DSN != "" {
		msg = strings.ReplaceAll(msg, cfg.DSN, driver.RedactDSN(cfg.DSN))
	}
	for _, s := range dsnSecrets(cfg, extra) {
		msg = strings.ReplaceAll(msg, s, redactedMark)
	}
	return msg
}

// scrubError replaces the underlying error of a *driver.Error with a masked
// copy when its text would reveal a password. The kind and code are kept.
func scrubError(err error, cfg driver.ConnConfig, password string) error {
	var de *driver.Error
	if !errors.As(err, &de) || de.Err == nil {
		return err
	}
	msg := de.Err.Error()
	if clean := scrub(msg, cfg, password); clean != msg {
		de.Err = maskedError{msg: clean, err: de.Err}
	}
	return err
}

// maskedError shows a masked message but keeps the original error reachable,
// so errors.As and errors.Is still see the PgError or context error.
type maskedError struct {
	msg string
	err error
}

func (m maskedError) Error() string { return m.msg }
func (m maskedError) Unwrap() error { return m.err }
