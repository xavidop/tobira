// Package connio moves connections in and out of tobira: it prepares the
// connections `tobira connections export` prints, with every password
// removed, and reads the connection lists of other tools (DBeaver, pgpass,
// TablePlus) and of export itself into an import plan.
//
// Nothing here prints, writes the config or touches the keyring: the CLI
// does that. A password an import source holds travels only in
// Candidate.Password and Entry.Password, which nothing renders.
package connio

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/xavidop/tobira/internal/config"
)

// Export returns conns ready to be printed: a password that is not a secret
// reference is dropped, a DSN password is removed, and options whose key
// looks like a secret are dropped. Refs are kept: they are not secrets. A
// connection whose DSN password cannot be removed is left out. notes say
// what was dropped, naming the connection and never the value.
func Export(conns []config.Connection) (out []config.Connection, notes []string) {
	out = []config.Connection{}
	for _, c := range conns {
		if c.Password != "" && config.ValidateLoadedPasswordRef(c.Password) != nil {
			c.Password = ""
			notes = append(notes, c.Name+": dropped a password that is not a secret reference")
		}
		if dsn, had := StripDSNPassword(c.DSN); had {
			if dsn == "" {
				notes = append(notes, c.Name+": left out, its DSN password could not be removed")
				continue
			}
			c.DSN = dsn
			notes = append(notes, c.Name+": removed the password from the DSN")
		}
		if s := c.SSH; s != nil {
			s = s.Clone()
			if s.Passphrase != "" && config.ValidateLoadedPasswordRef(s.Passphrase) != nil {
				s.Passphrase = ""
				notes = append(notes, c.Name+": dropped an ssh passphrase that is not a secret reference")
			}
			if s.Password != "" && config.ValidateLoadedPasswordRef(s.Password) != nil {
				s.Password = ""
				notes = append(notes, c.Name+": dropped an ssh password that is not a secret reference")
			}
			c.SSH = s
		}
		if len(c.Options) > 0 {
			keys := make([]string, 0, len(c.Options))
			for k := range c.Options {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			opts := map[string]string{}
			for _, k := range keys {
				if config.IsSecretOptionKey(k) {
					notes = append(notes, fmt.Sprintf("%s: dropped option %q, which looks like a secret", c.Name, k))
					continue
				}
				opts[k] = c.Options[k]
			}
			c.Options = opts
			if len(opts) == 0 {
				c.Options = nil
			}
		}
		out = append(out, c)
	}
	return out, notes
}

// exported is one connection in the JSON export: the config's keys, empty
// ones left out.
type exported struct {
	Name     string            `json:"name"`
	Driver   string            `json:"driver,omitempty"`
	DSN      string            `json:"dsn"`
	Password string            `json:"password,omitempty"`
	Color    string            `json:"color,omitempty"`
	ReadOnly bool              `json:"read_only,omitempty"`
	Tags     []string          `json:"tags,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
	SSH      *exportedSSH      `json:"ssh,omitempty"`
}

// exportedSSH is a connection's ssh block in the JSON export, empty keys
// left out.
type exportedSSH struct {
	Host       string `json:"host"`
	Port       int    `json:"port,omitempty"`
	User       string `json:"user,omitempty"`
	Key        string `json:"key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	Password   string `json:"password,omitempty"`
	KnownHosts string `json:"known_hosts,omitempty"`
}

// MarshalJSON renders conns as {"connections": [...]}, the JSON form of
// config.MarshalConnections. ParseFile reads it back.
func MarshalJSON(conns []config.Connection) ([]byte, error) {
	doc := struct {
		Connections []exported `json:"connections"`
	}{Connections: []exported{}}
	for _, c := range conns {
		e := exported{
			Name: c.Name, Driver: c.Driver, DSN: c.DSN, Password: c.Password,
			Color: c.Color, ReadOnly: c.ReadOnly, Tags: c.Tags, Options: c.Options,
		}
		if s := c.SSH; s != nil {
			e.SSH = &exportedSSH{Host: s.Host, Port: s.Port, User: s.User, Key: s.Key,
				Passphrase: s.Passphrase, Password: s.Password, KnownHosts: s.KnownHosts}
		}
		doc.Connections = append(doc.Connections, e)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
