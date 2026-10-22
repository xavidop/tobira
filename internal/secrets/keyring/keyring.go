// Package keyring resolves keyring:<name> password references from the OS
// keyring (macOS Keychain, Windows Credential Manager, the Secret Service on
// Linux). Entries live under service "tobira" with the connection name as the
// account. The store sits behind an interface so tests never touch a real
// keychain.
package keyring

import (
	"context"
	"errors"
	"fmt"

	gokeyring "github.com/zalando/go-keyring"

	"github.com/xavidop/mamori"
	"github.com/xavidop/tobira/internal/config"
)

// Service is the keyring service name every tobira entry is stored under.
const Service = "tobira"

// Scheme is the secret reference scheme this package provides.
const Scheme = "keyring"

// Store is the part of a keyring tobira uses.
type Store interface {
	Get(service, account string) (string, error)
	Set(service, account, secret string) error
	Delete(service, account string) error
}

// ErrNotFound is returned by the OS store when an entry does not exist.
var ErrNotFound = gokeyring.ErrNotFound

type osStore struct{}

// OS returns the Store backed by the operating system keyring.
func OS() Store { return osStore{} }

func (osStore) Get(service, account string) (string, error) {
	return gokeyring.Get(service, account)
}

func (osStore) Set(service, account, secret string) error {
	return gokeyring.Set(service, account, secret)
}

func (osStore) Delete(service, account string) error {
	return gokeyring.Delete(service, account)
}

// Ref returns the password reference for the named connection.
func Ref(name string) string { return Scheme + ":" + name }

// ValidName reports whether name can be a keyring account: the same rule as
// a connection name.
func ValidName(name string) error { return config.ValidateName(name) }

// NewProvider returns the mamori provider for keyring: references, reading
// (Service, ref.Path) from s.
func NewProvider(s Store) mamori.Provider { return provider{s: s} }

type provider struct{ s Store }

func (provider) Scheme() string { return Scheme }

func (p provider) Resolve(_ context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ValidName(ref.Path); err != nil {
		return mamori.Value{}, errors.New("keyring: invalid account name")
	}
	pw, err := p.s.Get(Service, ref.Path)
	if err != nil {
		if errors.Is(err, gokeyring.ErrNotFound) {
			return mamori.Value{}, fmt.Errorf("keyring: no entry for %q: %w", ref.Path, mamori.ErrNotFound)
		}
		// The backend's text is dropped: it is not ours to vouch for.
		return mamori.Value{}, fmt.Errorf("keyring: read %q failed", ref.Path)
	}
	return mamori.Value{Bytes: []byte(pw), Sensitive: true}, nil
}

// CleanupOnRemove deletes the keyring entry of the connection called name,
// which cfg (the config before the removal) still holds, after it is removed.
// removedRef is the removed entry's password reference. The entry goes only
// when that reference is exactly keyring:<name> and no other entry, broken
// ones included, uses the same reference. It is best effort: it returns a
// one-line warning (never the backend's text) when the delete fails, and ""
// otherwise. A nil store does nothing.
func CleanupOnRemove(s Store, cfg *config.Config, name, removedRef string) string {
	ref := Ref(name)
	if s == nil || name == "" || removedRef != ref {
		return ""
	}
	uses := 0
	for _, c := range cfg.Connections {
		if c.Password == ref {
			uses++
		}
	}
	for _, b := range cfg.Broken {
		if b.Partial.Password == ref {
			uses++
		}
	}
	if uses > 1 {
		return "" // another entry still uses it
	}
	if err := s.Delete(Service, name); err != nil && !errors.Is(err, gokeyring.ErrNotFound) {
		return "could not delete the keyring entry for " + name
	}
	return ""
}

// SharedWith returns the name of an entry of cfg other than the one called
// name, broken ones included, whose password reference is keyring:<name>, and
// whether there is one. An unnamed broken entry is named by its line.
func SharedWith(cfg *config.Config, name string) (string, bool) {
	if cfg == nil {
		return "", false
	}
	ref := Ref(name)
	for _, c := range cfg.Connections {
		if c.Name != name && c.Password == ref {
			return c.Name, true
		}
	}
	for _, b := range cfg.Broken {
		if b.Name != name && b.Partial.Password == ref {
			if b.Name == "" {
				return fmt.Sprintf("at line %d", b.Line), true
			}
			return b.Name, true
		}
	}
	return "", false
}

// SetError describes a failed Set in terms safe to show: it names the account
// and, for the failures worth explaining, the cause. The backend's own text is
// dropped since it is not ours to vouch for.
func SetError(name string, err error) error {
	switch {
	case errors.Is(err, gokeyring.ErrSetDataTooBig):
		return fmt.Errorf("could not store the password for %s in the OS keyring: it is too long for this keyring", name)
	case errors.Is(err, gokeyring.ErrUnsupportedPlatform):
		return fmt.Errorf("could not store the password for %s: the OS keyring is not supported on this platform", name)
	}
	return fmt.Errorf("could not store the password for %s in the OS keyring (is a keyring service running?)", name)
}
