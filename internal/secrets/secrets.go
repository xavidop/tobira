package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/xavidop/mamori"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/sshtunnel"
)

// Resolver resolves connection passwords. Zero-value fields default to
// os.Getenv and mamori.Resolve.
type Resolver struct {
	Getenv  func(string) string
	Resolve func(ctx context.Context, ref string) ([]byte, error)
}

// EnvName returns the environment variable that overrides the password of
// the named connection: "local-pg" becomes TOBIRA_CONN_LOCAL_PG_PASSWORD.
// The name is upper-cased and every non-alphanumeric byte becomes "_".
func EnvName(conn string) string {
	var b strings.Builder
	b.WriteString("TOBIRA_CONN_")
	for _, r := range strings.ToUpper(conn) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	b.WriteString("_PASSWORD")
	return b.String()
}

// ConnConfig builds the driver.ConnConfig for c. The password is taken from
// the TOBIRA_CONN_<NAME>_PASSWORD variable if non-empty, else from the
// connection's password: ref, else left zero so the driver uses the password
// embedded in the DSN (which is passed through unchanged).
//
// A secret resolved from a ref has a single trailing "\n" or "\r\n" removed,
// because file:// secrets usually end in a newline. A ref that resolves to
// an empty value is an error. Errors name the connection, the ref scheme
// (the first one of a chain "a,b") and a coarse failure class, never the
// ref's path or key and never the secret. A scheme that is not a registered
// mamori provider is printed as "password", since the value may be a literal
// password that merely looks like "word:rest". The original error
// stays reachable through errors.Is/As but is not part of the message, since
// mamori provider errors carry the ref's path.
func (r Resolver) ConnConfig(ctx context.Context, c config.Connection) (driver.ConnConfig, error) {
	cfg := driver.ConnConfig{
		Name:     c.Name,
		Driver:   c.Driver,
		DSN:      c.DSN,
		ReadOnly: c.ReadOnly,
	}
	if c.Options != nil {
		cfg.Options = make(map[string]string, len(c.Options))
		for k, v := range c.Options {
			cfg.Options[k] = v
		}
	}

	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := getenv(EnvName(c.Name)); v != "" {
		cfg.Password = driver.NewSecret(v)
		return cfg, nil
	}
	if c.Password == "" {
		return cfg, nil
	}

	raw, scheme, err := r.resolve(ctx, c.Password)
	if err != nil {
		return driver.ConnConfig{}, &resolveError{what: fmt.Sprintf("connection %q: resolve password", c.Name), scheme: scheme, err: err}
	}
	if len(raw) == 0 {
		return driver.ConnConfig{}, fmt.Errorf("connection %q: resolve password (%s ref): %w", c.Name, scheme, errEmptyValue)
	}
	cfg.Password = driver.NewSecret(string(raw))
	return cfg, nil
}

// Secret resolves a secret ref that is not a connection password, such as
// ai.api_key; what names it in errors. As in ConnConfig, one trailing
// newline is removed, an empty value is an error, and errors name only the
// scheme and a coarse failure class, never the ref's path or the secret.
func (r Resolver) Secret(ctx context.Context, what, ref string) (driver.Secret, error) {
	raw, scheme, err := r.resolve(ctx, ref)
	if err != nil {
		return driver.Secret{}, &resolveError{what: what + ": resolve", scheme: scheme, err: err}
	}
	if len(raw) == 0 {
		return driver.Secret{}, fmt.Errorf("%s: resolve (%s ref): %w", what, scheme, errEmptyValue)
	}
	return driver.NewSecret(string(raw)), nil
}

// Cached returns a copy of r that resolves each distinct ref once and
// serves that result, value or error, to every later call, concurrent ones
// included (they wait for the first). It is meant for one short-lived run,
// such as tobira doctor, where the same ref would otherwise be resolved by
// several checks (and a keyring ref would prompt each time). The first
// call's ctx governs the one resolution.
func (r Resolver) Cached() Resolver {
	resolve := r.Resolve
	if resolve == nil {
		resolve = mamoriResolve
	}
	type entry struct {
		once sync.Once
		raw  []byte
		err  error
	}
	var mu sync.Mutex
	cache := map[string]*entry{}
	r.Resolve = func(ctx context.Context, ref string) ([]byte, error) {
		mu.Lock()
		e := cache[ref]
		if e == nil {
			e = &entry{}
			cache[ref] = e
		}
		mu.Unlock()
		e.once.Do(func() { e.raw, e.err = resolve(ctx, ref) })
		return bytes.Clone(e.raw), e.err
	}
	return r
}

// resolve resolves ref and strips one trailing newline. scheme is the first
// scheme of the ref, or "password" when it is not a registered provider.
func (r Resolver) resolve(ctx context.Context, ref string) (raw []byte, scheme string, err error) {
	resolve := r.Resolve
	if resolve == nil {
		resolve = mamoriResolve
	}
	scheme = Scheme(ref)
	raw, err = resolve(ctx, ref)
	if err != nil {
		return nil, scheme, err
	}
	if bytes.HasSuffix(raw, []byte("\r\n")) {
		raw = raw[:len(raw)-2]
	} else {
		raw = bytes.TrimSuffix(raw, []byte("\n"))
	}
	return raw, scheme, nil
}

// errEmptyValue is the failure of a ref that resolves to nothing.
var errEmptyValue = errors.New("resolved to an empty value")

// Scheme is the scheme errors name for ref: the first scheme of a chain
// "a,b" when it is a registered mamori provider, else "password", since
// the value may be a literal password that merely looks like "word:rest".
// It never returns any other part of ref.
func Scheme(ref string) string {
	if refs, err := mamori.ParseRefs(ref); err == nil && len(refs) > 0 &&
		slices.Contains(mamori.RegisteredSchemes(), refs[0].Scheme) {
		return refs[0].Scheme
	}
	return "password"
}

// localSchemes are the ref schemes resolved on this machine: the
// environment, a file, a dotenv file and the OS keyring.
var localSchemes = []string{"env", "file", "dotenv", "keyring"}

// IsLocal reports whether every ref of a chain "a,b" is resolved on this
// machine (env, file, dotenv, keyring), so resolving it reaches no remote
// store. A ref that does not parse is not local.
func IsLocal(ref string) bool {
	refs, err := mamori.ParseRefs(ref)
	if err != nil || len(refs) == 0 {
		return false
	}
	for _, r := range refs {
		if !slices.Contains(localSchemes, r.Scheme) {
			return false
		}
	}
	return true
}

// KnownSchemes reports whether every ref of a chain "a,b" parses and uses a
// scheme a provider compiled into this build handles. A false result means
// the ref can never resolve here, whatever the store holds.
func KnownSchemes(ref string) bool {
	refs, err := mamori.ParseRefs(ref)
	if err != nil || len(refs) == 0 {
		return false
	}
	registered := mamori.RegisteredSchemes()
	for _, r := range refs {
		if !slices.Contains(registered, r.Scheme) {
			return false
		}
	}
	return true
}

// FailureClass is a coarse reason for an error from Secret or ConnConfig
// ("not found", "permission denied", "resolved to an empty value", ...),
// never the ref's path, key or value. It is "unknown error" for an error
// that did not come from resolving a ref.
func FailureClass(err error) string {
	var re *resolveError
	switch {
	case errors.As(err, &re):
		return errorClass(re.err)
	case errors.Is(err, errEmptyValue):
		return errEmptyValue.Error()
	}
	return "unknown error"
}

func mamoriResolve(ctx context.Context, ref string) ([]byte, error) {
	v, err := mamori.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	return v.Bytes, nil
}

// resolveError reports a failed password resolution without the original
// error's text, which may contain the ref's path or key.
type resolveError struct {
	what, scheme string
	err          error
}

func (e *resolveError) Error() string {
	return fmt.Sprintf("%s (%s ref): %s", e.what, e.scheme, errorClass(e.err))
}

func (e *resolveError) Unwrap() error { return e.err }

func errorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	}
	switch mamori.ErrorKind(err) {
	case mamori.KindNotFound:
		return "not found"
	case mamori.KindPermissionDenied:
		return "permission denied"
	case mamori.KindUnauthenticated:
		return "unauthenticated"
	case mamori.KindUnavailable:
		return "unavailable"
	case mamori.KindRateLimited:
		return "rate limited"
	case mamori.KindInvalid:
		return "invalid ref"
	}
	return "unknown error"
}

// SSH builds the tunnel config for c's ssh: block, resolving its
// passphrase and password refs as ConnConfig resolves the password. It is
// nil, nil for a direct connection. Errors name the connection and the
// field, the ref scheme and a coarse failure class, never a value.
func (r Resolver) SSH(ctx context.Context, c config.Connection) (*sshtunnel.Config, error) {
	s := c.SSH
	if s == nil {
		return nil, nil
	}
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	out := &sshtunnel.Config{Host: s.Host, Port: s.Port, User: s.User, KeyFile: s.Key, KnownHosts: s.KnownHosts, Getenv: getenv}
	for _, f := range []struct {
		name, ref string
		dst       *driver.Secret
	}{{"ssh.passphrase", s.Passphrase, &out.Passphrase}, {"ssh.password", s.Password, &out.Password}} {
		if f.ref == "" {
			continue
		}
		sec, err := r.Secret(ctx, fmt.Sprintf("connection %q: %s", c.Name, f.name), f.ref)
		if err != nil {
			return nil, err
		}
		*f.dst = sec
	}
	return out, nil
}
