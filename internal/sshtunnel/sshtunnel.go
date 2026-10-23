package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/xavidop/tobira/driver"
)

// Config says how to reach the SSH server. Secrets are driver.Secret so no
// formatting shows them.
type Config struct {
	// Host is the SSH server, optionally with ":port".
	Host string
	// Port overrides a port in Host; 0 means Host's, else 22.
	Port int
	// User is the login; "" means $USER, else the process owner.
	User string
	// KeyFile is a private key file; "" means the agent, then the default
	// keys under ~/.ssh. "~" and $VAR are expanded.
	KeyFile string
	// Passphrase decrypts KeyFile when it is encrypted.
	Passphrase driver.Secret
	// Password enables password authentication, tried after keys.
	Password driver.Secret
	// KnownHosts is the file host keys are checked against; "" means
	// ~/.ssh/known_hosts.
	KnownHosts string
	// Home is the home directory "~" expands to; "" means the process's.
	Home string
	// Getenv reads the environment; nil means os.Getenv.
	Getenv func(string) string
}

// Addr is the SSH server address, "host:port".
func (c Config) Addr() string {
	host, port := c.Host, ""
	if h, p, err := net.SplitHostPort(c.Host); err == nil {
		host, port = h, p
	}
	if c.Port != 0 {
		port = strconv.Itoa(c.Port)
	}
	if port == "" {
		port = "22"
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), port)
}

func (c Config) getenv(name string) string {
	if c.Getenv != nil {
		return c.Getenv(name)
	}
	return os.Getenv(name)
}

func (c Config) home() string {
	if c.Home != "" {
		return c.Home
	}
	h, _ := os.UserHomeDir()
	return h
}

// expand replaces $VAR, ${VAR} and a leading "~" in p.
func (c Config) expand(p string) string {
	p = os.Expand(p, c.getenv)
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(c.home(), p[min(len(p), 2):])
	}
	return p
}

func (c Config) user() string {
	if c.User != "" {
		return c.User
	}
	if u := c.getenv("USER"); u != "" {
		return u
	}
	return c.getenv("USERNAME")
}

// connectTimeout bounds the TCP connect and the SSH handshake.
const connectTimeout = 30 * time.Second

// defaultKeys are tried, in order, when no key file is set, as ssh does.
var defaultKeys = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

// Tunnel is an open SSH session with a loopback listener in front of it.
// Close it after the driver connection that uses it.
type Tunnel struct {
	client *ssh.Client
	ln     net.Listener
	target string
	log    *slog.Logger
	once   sync.Once
	closed chan struct{}
}

// Addr is the loopback "127.0.0.1:port" to connect to.
func (t *Tunnel) Addr() string { return t.ln.Addr().String() }

// Target is the "host:port" the tunnel forwards to.
func (t *Tunnel) Target() string { return t.target }

// Open connects to cfg's SSH server and starts forwarding a loopback port
// to target ("host:port" as seen from that server). Errors name the server
// and the reason, never a secret.
func Open(ctx context.Context, cfg Config, target string, log *slog.Logger) (*Tunnel, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		return nil, fmt.Errorf("ssh tunnel: target must be host:port: %w", err)
	}
	addr := cfg.Addr()
	hostKey, err := cfg.hostKeyCallback()
	if err != nil {
		return nil, err
	}
	auth, err := cfg.authMethods(log)
	if err != nil {
		return nil, err
	}
	user := cfg.user()
	if user == "" {
		return nil, errors.New("ssh tunnel: no user: set ssh.user")
	}
	clientCfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: hostKey,
		Timeout:         connectTimeout,
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh tunnel: connect to %s: %w", addr, err)
	}
	// The handshake has no context: close the socket if ctx ends first.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-done:
		}
	}()
	sshConn, chans, reqs, err := ssh.NewClientConn(raw, addr, clientCfg)
	close(done)
	if err != nil {
		_ = raw.Close()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ssh tunnel: connect to %s: %w", addr, ctx.Err())
		}
		return nil, fmt.Errorf("ssh tunnel: %s: %w", addr, describe(err))
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ssh tunnel: listen: %w", err)
	}
	t := &Tunnel{client: client, ln: ln, target: target, log: log, closed: make(chan struct{})}
	go t.accept()
	log.Debug("ssh tunnel open", "server", addr, "user", user, "target", target, "local", t.Addr())
	return t, nil
}

// describe rewrites the library's handshake errors into one line that
// says what to do.
func describe(err error) error {
	var ke *knownhosts.KeyError
	if errors.As(err, &ke) {
		if len(ke.Want) == 0 {
			return errors.New("host key is not in known_hosts; add it with: ssh-keyscan -H <host> >> ~/.ssh/known_hosts")
		}
		return errors.New("host key does not match known_hosts (a changed server, or someone in the middle); fix known_hosts before connecting")
	}
	msg := err.Error()
	if strings.Contains(msg, "unable to authenticate") {
		return errors.New("authentication failed: no key or password the server accepts (set ssh.key, ssh.passphrase or ssh.password)")
	}
	return errors.New(strings.TrimPrefix(msg, "ssh: "))
}

// KeyPath is the expanded KeyFile, or "" when none is set.
func (c Config) KeyPath() string {
	if c.KeyFile == "" {
		return ""
	}
	return c.expand(c.KeyFile)
}

// KnownHostsPath is the expanded KnownHosts, or ~/.ssh/known_hosts. It is
// "" when neither is known (no home directory).
func (c Config) KnownHostsPath() string {
	if c.KnownHosts != "" {
		return c.expand(c.KnownHosts)
	}
	if home := c.home(); home != "" {
		return filepath.Join(home, ".ssh", "known_hosts")
	}
	return ""
}

func (c Config) hostKeyCallback() (ssh.HostKeyCallback, error) {
	path := c.KnownHostsPath()
	if path == "" {
		return nil, errors.New("ssh tunnel: cannot locate ~/.ssh/known_hosts without a home directory; set ssh.known_hosts")
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("ssh tunnel: known_hosts file %s does not exist; add the server's key with: ssh-keyscan -H <host> >> %s", path, path)
		}
		return nil, fmt.Errorf("ssh tunnel: read known_hosts: %w", err)
	}
	return cb, nil
}

// authMethods is the key, agent, default-key and password methods cfg
// allows, in that order. At least one must apply.
func (c Config) authMethods(log *slog.Logger) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if c.KeyFile != "" {
		signer, err := c.loadKey(c.KeyPath())
		if err != nil {
			return nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if sock := c.getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		} else {
			log.Debug("ssh agent unavailable", "error", err.Error())
		}
	}
	if c.KeyFile == "" {
		if home := c.home(); home != "" {
			for _, name := range defaultKeys {
				path := filepath.Join(home, ".ssh", name)
				if _, err := os.Stat(path); err != nil {
					continue
				}
				signer, err := c.loadKey(path)
				if err != nil {
					log.Debug("ssh default key skipped", "key", name, "error", err.Error())
					continue
				}
				methods = append(methods, ssh.PublicKeys(signer))
			}
		}
	}
	if !c.Password.IsZero() {
		pw := c.Password
		methods = append(methods, ssh.PasswordCallback(func() (string, error) { return pw.Reveal(), nil }))
	}
	if len(methods) == 0 {
		return nil, errors.New("ssh tunnel: no way to authenticate: set ssh.key or ssh.password, or run an SSH agent")
	}
	return methods, nil
}

// loadKey parses the private key at path, with the passphrase when the key
// is encrypted. Errors name the file, never its content or the passphrase.
func (c Config) loadKey(path string) (ssh.Signer, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ssh tunnel: read key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err == nil {
		return signer, nil
	}
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return nil, fmt.Errorf("ssh tunnel: key %s: not a private key tobira can read", path)
	}
	if c.Passphrase.IsZero() {
		return nil, fmt.Errorf("ssh tunnel: key %s is encrypted; set ssh.passphrase", path)
	}
	signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(c.Passphrase.Reveal()))
	if err != nil {
		return nil, fmt.Errorf("ssh tunnel: key %s: wrong passphrase", path)
	}
	return signer, nil
}

// accept forwards each local connection to the target until Close.
func (t *Tunnel) accept() {
	for {
		local, err := t.ln.Accept()
		if err != nil {
			return
		}
		go t.forward(local)
	}
}

func (t *Tunnel) forward(local net.Conn) {
	defer func() { _ = local.Close() }()
	remote, err := t.client.Dial("tcp", t.target)
	if err != nil {
		t.log.Debug("ssh tunnel forward failed", "target", t.target, "error", err.Error())
		return
	}
	defer func() { _ = remote.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
	select {
	case <-done:
	case <-t.closed:
	}
}

// Close stops the listener and the SSH session. Forwarded connections end
// with it.
func (t *Tunnel) Close() error {
	if t == nil {
		return nil
	}
	var err error
	t.once.Do(func() {
		close(t.closed)
		_ = t.ln.Close()
		err = t.client.Close()
	})
	return err
}

// OpenConn opens cfg's driver connection through a tunnel described by t:
// the DSN's host and port are replaced by the tunnel's loopback address
// before the driver sees them. A nil t opens the connection directly and
// returns a nil tunnel. Close the tunnel after the connection. Errors are
// not redacted; redact them with the original cfg.
func OpenConn(ctx context.Context, reg *driver.Registry, cfg driver.ConnConfig, t *Config) (driver.Conn, *Tunnel, error) {
	if reg == nil {
		reg = driver.Default
	}
	if t == nil {
		conn, err := reg.Open(ctx, cfg)
		return conn, nil, err
	}
	target, err := Target(cfg.DSN)
	if err != nil {
		return nil, nil, err
	}
	tun, err := Open(ctx, *t, target, cfg.Logger)
	if err != nil {
		return nil, nil, err
	}
	dsn, err := Rewrite(cfg.DSN, tun.Addr())
	if err != nil {
		_ = tun.Close()
		return nil, nil, err
	}
	cfg.DSN = dsn
	conn, err := reg.Open(ctx, cfg)
	if err != nil {
		_ = tun.Close()
		return nil, nil, err
	}
	return conn, tun, nil
}
