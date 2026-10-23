package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SSH is a connection's ssh: block: the database is reached through an SSH
// tunnel to Host, and the DSN's host and port name where the database is as
// seen from that machine.
type SSH struct {
	// Host is the SSH server, with an optional ":port" (default 22).
	Host string `yaml:"host"`
	// Port overrides the port in Host; 0 means Host's or 22.
	Port int `yaml:"port"`
	// User is the SSH login; empty means the current user.
	User string `yaml:"user"`
	// Key is a private key file; empty means the SSH agent, then the
	// default keys in ~/.ssh.
	Key string `yaml:"key"`
	// Passphrase is a secret ref for Key's passphrase, never the value.
	Passphrase string `yaml:"passphrase"`
	// Password is a secret ref for password authentication, never the value.
	Password string `yaml:"password"`
	// KnownHosts is the file the server's host key is checked against;
	// empty means ~/.ssh/known_hosts.
	KnownHosts string `yaml:"known_hosts"`
}

var sshKeyList = []string{"host", "port", "user", "key", "passphrase", "password", "known_hosts"}

// checkSSHKeys refuses unknown keys under ssh:, naming the closest known
// one when there is one. It never quotes a key.
func checkSSHKeys(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("ssh must be a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		known := false
		for _, want := range sshKeyList {
			known = known || k == want
		}
		if known {
			continue
		}
		if g := guessKeyIn(k, sshKeyList); g != "" {
			return fmt.Errorf("unknown ssh key (did you mean %q?)", g)
		}
		return errors.New("unknown ssh key")
	}
	return nil
}

// validateSSH checks a decoded ssh: block. Errors never quote a value.
func validateSSH(s *SSH) error {
	if s == nil {
		return nil
	}
	if strings.TrimSpace(s.Host) == "" {
		return errors.New("ssh.host is required")
	}
	for _, v := range []string{s.Host, s.User, s.Key, s.KnownHosts} {
		if hasControl(v) {
			return errors.New("ssh values must not contain control characters")
		}
	}
	if s.Port < 0 || s.Port > 65535 {
		return errors.New("ssh.port must be from 1 to 65535")
	}
	for name, p := range map[string]string{"ssh.key": s.Key, "ssh.known_hosts": s.KnownHosts} {
		if p != "" && !filepath.IsAbs(p) && !strings.HasPrefix(p, "~") && !strings.HasPrefix(p, "$") {
			return errors.New(name + " must be absolute, or start with ~ or an environment variable")
		}
	}
	for name, ref := range map[string]string{"ssh.passphrase": s.Passphrase, "ssh.password": s.Password} {
		if ref != "" && ValidateLoadedPasswordRef(ref) != nil {
			return errors.New(name + " must be a secret reference (env:NAME, file:///path, dotenv://path#KEY, keyring:NAME)")
		}
	}
	return nil
}

// safeSSH keeps the fields of s that are safe to show and re-save, or nil
// when s is nil or its host is unusable.
func safeSSH(s *SSH) *SSH {
	if s == nil || hasControl(s.Host) || strings.TrimSpace(s.Host) == "" {
		return nil
	}
	p := &SSH{Host: s.Host, Port: s.Port}
	if !hasControl(s.User) {
		p.User = s.User
	}
	if !hasControl(s.Key) {
		p.Key = s.Key
	}
	if !hasControl(s.KnownHosts) {
		p.KnownHosts = s.KnownHosts
	}
	if ValidateLoadedPasswordRef(s.Passphrase) == nil {
		p.Passphrase = s.Passphrase
	}
	if ValidateLoadedPasswordRef(s.Password) == nil {
		p.Password = s.Password
	}
	return p
}

// Clone returns a copy of s, or nil.
func (s *SSH) Clone() *SSH {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}
