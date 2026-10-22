package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
)

// openTimeout bounds one shared open, which runs detached from its waiters.
const openTimeout = 30 * time.Second

// Status is the connection lifecycle state.
type Status int

const (
	// Idle is a connection that is not open (never opened, or closed).
	Idle Status = iota
	// Connecting is a connection with an open in flight.
	Connecting
	// Ready is an open connection.
	Ready
	// Failed is a connection whose last open failed; Open retries it.
	Failed
)

func (s Status) String() string {
	switch s {
	case Idle:
		return "idle"
	case Connecting:
		return "connecting"
	case Ready:
		return "ready"
	case Failed:
		return "failed"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// State is a snapshot of one connection.
type State struct {
	Name   string
	Status Status
	Err    string // redacted; set only when Status is Failed
	// Broken marks a config entry that failed validation: it cannot be
	// opened, and Err says what is wrong with it.
	Broken   bool
	ReadOnly bool
	Info     driver.Info // zero when no driver matches
}

// redactedError carries redacted text while keeping the original's kind
// reachable through errors.Is and errors.As. It deliberately has no Unwrap,
// so the raw message cannot be recovered with errors.Unwrap.
type redactedError struct {
	msg  string
	orig error
	cfg  driver.ConnConfig
}

func (e *redactedError) Error() string { return e.msg }

// Is matches sentinels (driver.ErrAuth, context.Canceled, ...) of the original.
func (e *redactedError) Is(target error) bool { return errors.Is(e.orig, target) }

// As supports *driver.Error targets, returning a copy whose message is redacted.
func (e *redactedError) As(target any) bool {
	t, ok := target.(**driver.Error)
	if !ok {
		return false
	}
	var de *driver.Error
	if !errors.As(e.orig, &de) {
		return false
	}
	cp := *de
	cp.Err = nil
	if de.Err != nil {
		cp.Err = errors.New(driver.RedactMessage(de.Err.Error(), e.cfg))
	}
	*t = &cp
	return true
}

// flight is one in-flight open shared by all concurrent Open calls.
type flight struct {
	done chan struct{} // closed when conn and err are set
	conn driver.Conn
	err  error
}

type entry struct {
	cfg    config.Connection
	status Status
	err    string
	conn   driver.Conn
	flight *flight
	adhoc  bool
	// broken is set for a config entry that failed validation. Such an
	// entry is always Failed with the validation error and never opens.
	broken *config.BrokenConnection
	// redactCfg is the ConnConfig the last open used (secrets included). It
	// is used only by Redact and survives Close and Reload, so a late error
	// from a call that was in flight is still masked; it goes away with the
	// entry when the connection is removed from the config. Nil until a
	// resolution succeeded.
	redactCfg *driver.ConnConfig
	gen       int // bumped by Close so a stale open discards its result
	// roOverride is the runtime read-only switch (SetReadOnly). It never
	// changes how the connection is opened: it only adds the per-call guard.
	roOverride bool
	// cfgGen identifies the configuration the entry holds (ConfigGeneration).
	cfgGen uint64
}

// Manager holds the lazily opened connections of one run. It is safe for
// concurrent use.
type Manager struct {
	reg           *driver.Registry
	res           secrets.Resolver
	forceReadOnly bool
	embedder      driver.Embedder // set into every ConnConfig; nil = none
	logger        *slog.Logger    // set into every ConnConfig; never nil

	mu      sync.Mutex
	order   []string
	entries map[string]*entry
	lastGen uint64 // the last configuration generation handed out
}

// Option configures New.
type Option func(*Manager)

// WithEmbedder passes e as ConnConfig.Embedder to every connection the
// Manager opens, for text similarity search. A nil e means none.
func WithEmbedder(e driver.Embedder) Option { return func(m *Manager) { m.embedder = e } }

// WithLogger passes l, the host's --debug log, as ConnConfig.Logger to every
// connection the Manager opens, so driver warnings reach it. Without it (or
// with a nil l) drivers get a logger that discards.
func WithLogger(l *slog.Logger) Option {
	return func(m *Manager) {
		if l != nil {
			m.logger = l
		}
	}
}

// New returns a Manager for the connections in cfg, plus any added later.
// With forceReadOnly every connection is opened read-only.
func New(cfg *config.Config, reg *driver.Registry, res secrets.Resolver, forceReadOnly bool, opts ...Option) *Manager {
	m := &Manager{reg: reg, res: res, forceReadOnly: forceReadOnly, entries: map[string]*entry{}, logger: slog.New(slog.DiscardHandler)}
	for _, o := range opts {
		o(m)
	}
	if cfg != nil {
		for _, c := range cfg.Connections {
			m.add(c)
		}
		for _, b := range cfg.Broken {
			m.addBroken(b)
		}
	}
	return m
}

// brokenKey is the unique name a broken entry is listed under: its own name
// (redacted, since a mistyped DSN can land in the name field) when it has one
// that is free, else one that says where it is. Keying by name keeps the
// sidebar selection on the entry when lines above it change. It never
// returns a taken key, so no broken entry is dropped.
func brokenKey(b config.BrokenConnection, taken func(string) bool) string {
	key := driver.RedactMessage(b.Name, driver.ConnConfig{})
	if key == "" || taken(key) {
		key = fmt.Sprintf("(entry at line %d)", b.Line)
	}
	base := key
	for i := 2; taken(key); i++ {
		key = fmt.Sprintf("%s #%d", base, i)
	}
	return key
}

// addBroken lists b after the connections that loaded.
func (m *Manager) addBroken(b config.BrokenConnection) {
	key := brokenKey(b, func(n string) bool { _, ok := m.entries[n]; return ok })
	m.entries[key] = &entry{broken: &b, status: Failed, err: b.Err, cfgGen: m.nextGen()}
	m.order = append(m.order, key)
}

// BrokenEntry returns the broken config entry listed under key.
func (m *Manager) BrokenEntry(key string) (config.BrokenConnection, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[key]; ok && e.broken != nil {
		return *e.broken, true
	}
	return config.BrokenConnection{}, false
}

// Broken lists the config entries that failed validation, in list order.
// Their Name is the entry's own, which is empty for an unnamed one.
func (m *Manager) Broken() []config.BrokenConnection {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []config.BrokenConnection
	for _, name := range m.order {
		if e := m.entries[name]; e.broken != nil {
			out = append(out, *e.broken)
		}
	}
	return out
}

// add registers c unless the name is taken. The caller holds no lock only
// during construction; AddDSN locks around it.
func (m *Manager) add(c config.Connection) {
	if _, ok := m.entries[c.Name]; ok {
		return
	}
	m.entries[c.Name] = &entry{cfg: c, cfgGen: m.nextGen()}
	m.order = append(m.order, c.Name)
}

// nextGen hands out a configuration generation. The caller holds m.mu, or
// is the constructor.
func (m *Manager) nextGen() uint64 {
	m.lastGen++
	return m.lastGen
}

// ConfigGeneration identifies the configuration name currently holds. It
// changes whenever a Reload adds or changes the entry, and is never reused
// within the Manager, so a name removed and added again gets a new one.
// Opening and closing do not change it. An unknown name yields 0. Callers
// that remember per-connection decisions (the assistant's row consent) key
// them by name and generation, so a repointed connection asks again.
func (m *Manager) ConfigGeneration(name string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[name]; ok {
		return e.cfgGen
	}
	return 0
}

// Names lists the connections: config order, then ad-hoc ones in the order
// they were added.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

// AddDSN registers an ad-hoc connection and returns its name, the redacted
// DSN. A DSN that redacts to the name of an existing ad-hoc connection reuses
// that entry; one that collides with a configured connection name is an error.
func (m *Manager) AddDSN(dsn string) (string, error) {
	name := driver.RedactDSN(dsn)
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[name]; ok {
		if e.adhoc {
			return name, nil
		}
		return "", fmt.Errorf("dsn name %q collides with a configured connection", name)
	}
	m.add(config.Connection{Name: name, DSN: dsn})
	m.entries[name].adhoc = true
	return name, nil
}

// Reload makes the configured connections match cfg without a restart, and
// reports the names it added, changed (any field differs) and removed.
// Changed and removed connections are closed, and a changed one reopens
// lazily with its new settings; unchanged ones keep their open connection.
// Ad-hoc DSN connections are left alone. Names are ordered as in cfg, then
// the ad-hoc ones. Errors from closing are dropped: the entries are gone or
// reset, so there is nobody left to act on them. A nil cfg removes every
// configured connection.
func (m *Manager) Reload(cfg *config.Config) (added, changed, removed []string) {
	var conns []config.Connection
	var broken []config.BrokenConnection
	if cfg != nil {
		conns, broken = cfg.Connections, cfg.Broken
	}
	var stale []driver.Conn
	m.mu.Lock()
	want := make(map[string]bool, len(conns)+len(broken))
	order := make([]string, 0, len(m.order)+len(conns)+len(broken))
	apply := func(name string, c config.Connection, b *config.BrokenConnection) {
		want[name] = true
		e, ok := m.entries[name]
		switch {
		case ok && e.adhoc:
			return // an ad-hoc connection owns the name
		case !ok:
			e = &entry{}
			m.entries[name] = e
			e.set(c, b)
			e.cfgGen = m.nextGen()
			added = append(added, name)
		case !reflect.DeepEqual(e.cfg, c) || !reflect.DeepEqual(e.broken, b):
			stale = append(stale, m.reset(e))
			e.set(c, b)
			e.cfgGen = m.nextGen()
			changed = append(changed, name)
		}
		order = append(order, name)
	}
	for _, c := range conns {
		apply(c.Name, c, nil)
	}
	// Broken keys are chosen against the names already taken in this
	// reload, so a broken duplicate never displaces the entry it duplicates.
	for _, b := range broken {
		apply(brokenKey(b, func(n string) bool {
			e, ok := m.entries[n]
			return want[n] || (ok && e.adhoc)
		}), config.Connection{}, &b)
	}
	for _, name := range m.order {
		e := m.entries[name]
		switch {
		case e.adhoc:
			order = append(order, name)
		case !want[name]:
			stale = append(stale, m.reset(e))
			delete(m.entries, name)
			removed = append(removed, name)
		}
	}
	m.order = order
	m.mu.Unlock()
	for _, c := range stale {
		_ = closeConn(c)
	}
	return added, changed, removed
}

// set points e at a configured connection, or at a broken entry (c is then
// the zero Connection) in which case it is Failed with the entry's error.
func (e *entry) set(c config.Connection, b *config.BrokenConnection) {
	e.cfg, e.broken, e.status, e.err = c, nil, Idle, ""
	if b != nil {
		bc := *b
		e.broken, e.status, e.err = &bc, Failed, b.Err
	}
}

// Drivers lists the drivers the manager can open connections with.
func (m *Manager) Drivers() []driver.Info { return m.reg.Drivers() }

// Color returns the configured color of name, or "" if it has none.
func (m *Manager) Color(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[name]; ok {
		return e.cfg.Color
	}
	return ""
}

// State returns a snapshot of name. An unknown name yields an Idle State.
// The driver's Info is read after the lock is released, so a slow Info
// (a plugin's) never stalls the Manager.
func (m *Manager) State(name string) State {
	m.mu.Lock()
	e, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return State{Name: name}
	}
	s := State{Name: name, Status: e.status, Err: e.err, Broken: e.broken != nil, ReadOnly: e.cfg.ReadOnly || m.forceReadOnly || e.roOverride}
	d, found := m.lookup(e.cfg.Driver, e.cfg.DSN)
	m.mu.Unlock()
	if found {
		s.Info = d.Info()
	}
	return s
}

// ErrReadOnlyEnforced refuses SetReadOnly(name, false) when the config entry
// or the --read-only flag makes the connection read-only.
var ErrReadOnlyEnforced = errors.New("read-only is enforced by config/flag")

// SetReadOnly turns the runtime read-only override of name on or off. State
// reports config OR flag OR override. The override never reopens the
// connection: a connection opened writable stays so, and callers pass
// State.ReadOnly as the per-call ReadOnly flag. Turning it off is refused
// with ErrReadOnlyEnforced when the config entry or the flag enforces
// read-only.
func (m *Manager) SetReadOnly(name string, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[name]
	if !ok {
		return fmt.Errorf("unknown connection %q", name)
	}
	if !on && (e.cfg.ReadOnly || m.forceReadOnly) {
		return ErrReadOnlyEnforced
	}
	e.roOverride = on
	return nil
}

// ReadOnlyOverride reports whether name's runtime read-only switch is on
// (SetReadOnly), apart from config and flag. A rename carries it over.
func (m *Manager) ReadOnlyOverride(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[name]
	return ok && e.roOverride
}

func (m *Manager) lookup(name, dsn string) (driver.Driver, bool) {
	if name != "" {
		return m.reg.Lookup(name)
	}
	return m.reg.ForScheme(driver.Scheme(dsn))
}

// Conn returns the open connection for name.
func (m *Manager) Conn(name string) (driver.Conn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[name]; ok && e.status == Ready {
		return e.conn, true
	}
	return nil, false
}

// Open returns the open connection for name, opening it first when needed.
// Concurrent calls share one open, and a Ready connection is returned at
// once. The open outlives a cancelled ctx (bounded by a 30s timeout) so other
// waiters still get it; a cancelled caller just returns ctx.Err().
func (m *Manager) Open(ctx context.Context, name string) (driver.Conn, error) {
	m.mu.Lock()
	e, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("unknown connection %q", name)
	}
	if e.broken != nil {
		msg := e.broken.Err
		m.mu.Unlock()
		return nil, errors.New(msg)
	}
	if e.status == Ready {
		c := e.conn
		m.mu.Unlock()
		return c, nil
	}
	f := e.flight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		e.flight, e.status, e.err = f, Connecting, ""
		go m.run(context.WithoutCancel(ctx), e, f, e.gen)
	}
	m.mu.Unlock()

	select {
	case <-f.done:
		return f.conn, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run performs the shared open and publishes its result.
func (m *Manager) run(ctx context.Context, e *entry, f *flight, gen int) {
	ctx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()

	m.mu.Lock()
	c := e.cfg
	m.mu.Unlock()

	cfg, err := m.res.ConnConfig(ctx, c)
	var conn driver.Conn
	resolved := err == nil
	if err != nil {
		cfg = unresolvedRedaction(c)
	} else {
		cfg.ReadOnly = cfg.ReadOnly || m.forceReadOnly
		cfg.Embedder = m.embedder
		cfg.Logger = m.logger
		conn, err = m.reg.Open(ctx, cfg)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if e.flight == f {
		e.flight = nil
	}
	if resolved && e.gen == gen {
		rc := cfg
		e.redactCfg = &rc
	}
	switch {
	case err != nil:
		msg := driver.RedactMessage(err.Error(), cfg)
		f.err = &redactedError{msg: msg, orig: err, cfg: cfg}
		if e.gen == gen {
			e.status, e.err = Failed, msg
		}
	case e.gen != gen:
		// Closed while connecting: discard the connection. Its Close error
		// is dropped on purpose: nobody is left to act on it.
		_ = conn.Close()
		f.err = fmt.Errorf("%w: closed while opening", driver.ErrConnection)
	default:
		f.conn = conn
		e.status, e.conn, e.err = Ready, conn, ""
	}
	close(f.done)
}

// Test checks that c can connect without touching any managed connection: it
// resolves the password, opens, pings and closes. The returned error is
// redacted. Cancelling ctx abandons the attempt.
func (m *Manager) Test(ctx context.Context, c config.Connection) error {
	ctx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	cfg, err := m.res.ConnConfig(ctx, c)
	if err != nil {
		return redact(err, unresolvedRedaction(c))
	}
	cfg.ReadOnly = cfg.ReadOnly || m.forceReadOnly
	cfg.Embedder = m.embedder
	cfg.Logger = m.logger
	conn, err := m.reg.Open(ctx, cfg)
	if err != nil {
		return redact(err, cfg)
	}
	err = conn.Ping(ctx)
	// The close error is dropped on purpose: the connection was only a probe.
	_ = conn.Close()
	if err != nil {
		return redact(err, cfg)
	}
	return nil
}

// unresolvedRedaction is the ConnConfig to redact with when c's password
// could not be resolved. The resolved value is unknown on failure, so the
// configured setting text (a literal password, or a ref whose text errors may
// echo) is masked instead, along with the DSN.
func unresolvedRedaction(c config.Connection) driver.ConnConfig {
	return driver.ConnConfig{DSN: c.DSN, Password: driver.NewSecret(c.Password)}
}

func redact(err error, cfg driver.ConnConfig) error {
	return &redactedError{msg: driver.RedactMessage(err.Error(), cfg), orig: err, cfg: cfg}
}

// Close closes name if it is open, or abandons an open in flight. The
// connection returns to Idle and a later Open reopens it.
func (m *Manager) Close(name string) error {
	m.mu.Lock()
	e, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	conn := m.reset(e)
	m.mu.Unlock()
	return closeConn(conn)
}

// CloseAll closes every open connection, returning the joined close errors.
// It is idempotent.
func (m *Manager) CloseAll() error {
	m.mu.Lock()
	var conns []driver.Conn
	for _, name := range m.order {
		if c := m.reset(m.entries[name]); c != nil {
			conns = append(conns, c)
		}
	}
	m.mu.Unlock()
	var errs []error
	for _, c := range conns {
		errs = append(errs, closeConn(c))
	}
	return errors.Join(errs...)
}

// reset returns e to Idle and hands back the connection to close, if any.
// The caller holds m.mu.
func (m *Manager) reset(e *entry) driver.Conn {
	conn := e.conn
	e.conn, e.err, e.status, e.flight = nil, "", Idle, nil
	e.gen++
	if e.broken != nil {
		e.status, e.err = Failed, e.broken.Err
	}
	return conn
}

func closeConn(c driver.Conn) error {
	if c == nil {
		return nil
	}
	return c.Close()
}

// Redact masks secrets belonging to connection name in msg, for errors that
// come from calls on an open connection (the session itself already redacts
// the errors Open returns). An unknown name still gets generic redaction.
func (m *Manager) Redact(name, msg string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[name]
	if !ok {
		return driver.RedactMessage(msg, driver.ConnConfig{})
	}
	// The DSN in the config may have changed since the last open (a Reload),
	// while a late error can still carry the old resolved settings: mask with
	// both.
	if e.redactCfg != nil {
		msg = driver.RedactMessage(msg, *e.redactCfg)
	}
	return driver.RedactMessage(msg, driver.ConnConfig{DSN: e.cfg.DSN})
}
