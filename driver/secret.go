package driver

import (
	"fmt"
	"log/slog"
)

const redacted = "[redacted]"

// Secret holds a resolved credential. Its String, GoString, Format, LogValue
// and MarshalJSON all print "[redacted]"; only Reveal returns the value.
//
// The value sits behind a pointer, so fmt prints an address, never the
// credential, even when a Secret is held in an unexported struct field (where
// fmt bypasses Formatter and Stringer). The guarantee is against accidental
// printing; code that reflects with unsafe access can still read the value.
// The zero Secret holds no value. Because of the pointer, Secrets compare by
// identity, not content: NewSecret("a") != NewSecret("a"). Compare Reveal()
// results when content equality is needed.
type Secret struct {
	v *string
}

// NewSecret wraps s as a Secret.
func NewSecret(s string) Secret { return Secret{v: &s} }

// Reveal returns the credential. Call it only where the value is handed to a
// database client, never to a printer or logger.
func (s Secret) Reveal() string {
	if s.v == nil {
		return ""
	}
	return *s.v
}

// IsZero reports whether the Secret holds no value.
func (s Secret) IsZero() bool { return s.v == nil || *s.v == "" }

// String returns "[redacted]".
func (s Secret) String() string { return redacted }

// GoString returns "[redacted]" for %#v.
func (s Secret) GoString() string { return redacted }

// Format prints "[redacted]" for every verb.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// LogValue returns "[redacted]" to log/slog.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON returns the JSON string "[redacted]".
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
