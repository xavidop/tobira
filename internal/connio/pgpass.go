package connio

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
)

// CheckPgpassMode refuses a pgpass file that is not a regular file or that
// the group or others may access, as libpq does (it ignores such a file
// with a warning; tobira refuses it so that the import is not silently
// empty).
func CheckPgpassMode(mode fs.FileMode) error {
	if !mode.IsRegular() {
		return errors.New("not a regular file")
	}
	if mode.Perm()&0o077 != 0 {
		return fmt.Errorf("it has group or world access (%#o); libpq ignores such a file and so does tobira: chmod 0600 it", mode.Perm())
	}
	return nil
}

var pgpassFields = [4]string{"hostname", "port", "database", "username"}

// ParsePgpass reads a libpq password file: one
// hostname:port:database:username:password entry per line, where '\'
// escapes the next character (\: and \\), a line starting with '#' is a
// comment and blank lines are ignored. Each entry becomes a postgres
// connection named <hostname>-<database> whose password is offered for the
// keyring. An entry with a * wildcard matches many servers, so it is
// skipped, as is a line with fewer than five fields.
func ParsePgpass(data []byte) ([]Candidate, error) {
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return nil, errors.New("not a text file")
	}
	var out []Candidate
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		c := Candidate{Label: fmt.Sprintf("line %d", i+1)}
		f, wild := splitPgpass(line)
		if len(f) < 5 {
			c.Skip = "malformed line: want hostname:port:database:username:password"
			out = append(out, c)
			continue
		}
		var wilds []string
		for j, name := range pgpassFields {
			if wild[j] {
				wilds = append(wilds, name)
			}
		}
		if len(wilds) > 0 {
			c.Skip = "wildcard (*) in " + strings.Join(wilds, " and ") + ": it matches many servers, not one connection"
			out = append(out, c)
			continue
		}
		host, port, db, user := f[0], f[1], f[2], f[3]
		dsn, err := buildDSN("postgres", host, port, db, user)
		if err != nil {
			c.Skip = err.Error()
			out = append(out, c)
			continue
		}
		name := host
		if db != "" {
			name += "-" + db
		}
		c.Conn = config.Connection{Name: name, Driver: "postgres", DSN: dsn}
		if f[4] != "" {
			c.Password = driver.NewSecret(f[4])
		}
		out = append(out, c)
	}
	return out, nil
}

// splitPgpass splits a pgpass line at unescaped colons and decodes the
// escapes. wild[i] reports whether field i is an unescaped lone "*".
func splitPgpass(line string) (fields []string, wild []bool) {
	var b strings.Builder
	raw := true // the field so far holds no escape
	flush := func() {
		fields = append(fields, b.String())
		wild = append(wild, raw && b.String() == "*")
		b.Reset()
		raw = true
	}
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && i+1 < len(line):
			i++
			b.WriteByte(line[i])
			raw = false
		case c == ':':
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return fields, wild
}
