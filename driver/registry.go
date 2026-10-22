package driver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Registry maps driver names and DSN schemes to drivers. It is a value type so
// embedders and tests can avoid global state. Names are case-sensitive;
// schemes are matched case-insensitively. A Registry is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	byName   map[string]Driver
	byScheme map[string]Driver
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Driver{}, byScheme: map[string]Driver{}}
}

// Add registers d under its Info().Name and Info().Schemes. It fails on a nil
// driver, an empty or duplicate name, or an empty or duplicate scheme; on
// failure nothing is registered.
func (r *Registry) Add(d Driver) error {
	if d == nil {
		return errors.New("driver: Add: nil driver")
	}
	info := d.Info()
	if info.Name == "" {
		return errors.New("driver: Add: empty driver name")
	}
	schemes := make([]string, 0, len(info.Schemes))
	for _, s := range info.Schemes {
		s = strings.ToLower(s)
		if s == "" {
			return fmt.Errorf("driver: Add: driver %q has an empty scheme", info.Name)
		}
		if !slices.Contains(schemes, s) {
			schemes = append(schemes, s)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[info.Name]; dup {
		return fmt.Errorf("driver: Add: driver %q already registered", info.Name)
	}
	for _, s := range schemes {
		if other, dup := r.byScheme[s]; dup {
			return fmt.Errorf("driver: Add: scheme %q of driver %q already registered by %q",
				s, info.Name, other.Info().Name)
		}
	}
	r.byName[info.Name] = d
	for _, s := range schemes {
		r.byScheme[s] = d
	}
	return nil
}

// Lookup returns the driver registered under name.
func (r *Registry) Lookup(name string) (Driver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byName[name]
	return d, ok
}

// ForScheme returns the driver registered for a DSN scheme, ignoring case.
func (r *Registry) ForScheme(scheme string) (Driver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byScheme[strings.ToLower(scheme)]
	return d, ok
}

// Drivers lists the registered drivers, sorted by name.
// Info is called with no lock held, so a slow Info cannot stall the
// registry.
func (r *Registry) Drivers() []Info {
	r.mu.RLock()
	ds := make([]Driver, 0, len(r.byName))
	for _, d := range r.byName {
		ds = append(ds, d)
	}
	r.mu.RUnlock()
	infos := make([]Info, 0, len(ds))
	for _, d := range ds {
		infos = append(infos, d.Info())
	}
	slices.SortFunc(infos, func(a, b Info) int { return strings.Compare(a.Name, b.Name) })
	return infos
}

// Open opens a connection with the driver named by cfg.Driver or, when that is
// empty, the driver registered for the scheme of cfg.DSN. cfg is passed to the
// driver unchanged. It returns ErrUnknownDriver when no driver matches.
func (r *Registry) Open(ctx context.Context, cfg ConnConfig) (Conn, error) {
	var d Driver
	if cfg.Driver != "" {
		var ok bool
		if d, ok = r.Lookup(cfg.Driver); !ok {
			return nil, Wrap(ErrUnknownDriver, "", "open", fmt.Errorf("no driver named %q", cfg.Driver))
		}
	} else {
		sc := Scheme(cfg.DSN)
		if sc == "" {
			return nil, Wrap(ErrUnknownDriver, "", "open",
				fmt.Errorf("no driver specified and DSN %q has no scheme", RedactDSN(cfg.DSN)))
		}
		var ok bool
		if d, ok = r.ForScheme(sc); !ok {
			return nil, Wrap(ErrUnknownDriver, "", "open", fmt.Errorf("no driver for scheme %q", sc))
		}
	}
	return d.Open(ctx, cfg)
}

// Default is the registry used by Register, Open, OpenConfig and Drivers.
var Default = NewRegistry()

// Register adds d to Default. It panics on error, so call it only from init,
// like database/sql.Register.
func Register(d Driver) {
	if err := Default.Add(d); err != nil {
		panic(err)
	}
}

// Open opens dsn with the driver registered for its scheme in Default.
func Open(ctx context.Context, dsn string) (Conn, error) {
	return Default.Open(ctx, ConnConfig{DSN: dsn})
}

// OpenConfig opens cfg using Default.
func OpenConfig(ctx context.Context, cfg ConnConfig) (Conn, error) {
	return Default.Open(ctx, cfg)
}

// Drivers lists the drivers registered in Default, sorted by name.
func Drivers() []Info { return Default.Drivers() }
