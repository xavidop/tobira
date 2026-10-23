package sshtunnel_test

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/sshtunnel"
	"github.com/xavidop/tobira/internal/sshtunnel/sshtest"
)

// fixture is a server that trusts one client key, a known_hosts file that
// trusts the server, and an echo service behind it.
type fixture struct {
	srv        *sshtest.Server
	keyPath    string
	knownHosts string
	echo       string
}

func newFixture(t *testing.T, passphrase string) fixture {
	t.Helper()
	dir := t.TempDir()
	pub, keyPath, err := sshtest.ClientKey(dir, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sshtest.Start(sshtest.Options{Authorized: pub, Password: "letmein"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	kh := filepath.Join(dir, "known_hosts")
	if err := srv.WriteKnownHosts(kh); err != nil {
		t.Fatal(err)
	}
	echo, stop, err := sshtest.Echo("hello\n")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return fixture{srv: srv, keyPath: keyPath, knownHosts: kh, echo: echo}
}

func (f fixture) config() sshtunnel.Config {
	return sshtunnel.Config{Host: f.srv.Addr, User: "deploy", KeyFile: f.keyPath, KnownHosts: f.knownHosts,
		Getenv: func(string) string { return "" }, Home: "/nonexistent"}
}

// roundTrip connects to addr, reads the banner and echoes one line.
func roundTrip(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	if line, _ := r.ReadString('\n'); line != "hello\n" {
		t.Fatalf("banner = %q", line)
	}
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if line, _ := r.ReadString('\n'); line != "ping\n" {
		t.Fatalf("echo = %q", line)
	}
}

func TestForwardsThroughTheServer(t *testing.T) {
	f := newFixture(t, "")
	tun, err := sshtunnel.Open(context.Background(), f.config(), f.echo, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tun.Close() }()
	if !strings.HasPrefix(tun.Addr(), "127.0.0.1:") || tun.Addr() == f.echo {
		t.Fatalf("Addr = %s, want a fresh loopback port", tun.Addr())
	}
	roundTrip(t, tun.Addr())
	roundTrip(t, tun.Addr())
	if got := f.srv.Targets(); len(got) != 2 || got[0] != f.echo {
		t.Errorf("server forwarded to %v, want %s twice", got, f.echo)
	}
	if got := f.srv.Users(); len(got) == 0 || got[0] != "deploy" {
		t.Errorf("users = %v, want deploy", got)
	}
	if err := tun.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if err := tun.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	if _, err := net.Dial("tcp", tun.Addr()); err == nil {
		t.Error("the loopback port still accepts after Close")
	}
}

func TestEncryptedKeyNeedsThePassphrase(t *testing.T) {
	f := newFixture(t, "s3cret")
	cfg := f.config()
	_, err := sshtunnel.Open(context.Background(), cfg, f.echo, nil)
	if err == nil || !strings.Contains(err.Error(), "is encrypted; set ssh.passphrase") {
		t.Fatalf("err = %v, want the passphrase hint", err)
	}
	cfg.Passphrase = driver.NewSecret("wrong")
	if _, err := sshtunnel.Open(context.Background(), cfg, f.echo, nil); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("err = %v, want wrong passphrase", err)
	}
	if strings.Contains(err.Error(), "wrong") && strings.Contains(err.Error(), "s3cret") {
		t.Error("the error carries the passphrase")
	}
	cfg.Passphrase = driver.NewSecret("s3cret")
	tun, err := sshtunnel.Open(context.Background(), cfg, f.echo, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tun.Close() }()
	roundTrip(t, tun.Addr())
}

func TestPasswordAuth(t *testing.T) {
	f := newFixture(t, "")
	cfg := f.config()
	cfg.KeyFile = ""
	if _, err := sshtunnel.Open(context.Background(), cfg, f.echo, nil); err == nil || !strings.Contains(err.Error(), "no way to authenticate") {
		t.Fatalf("err = %v, want no way to authenticate", err)
	}
	cfg.Password = driver.NewSecret("nope")
	_, err := sshtunnel.Open(context.Background(), cfg, f.echo, nil)
	if err == nil || !strings.Contains(err.Error(), "authentication failed") || strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want authentication failed without the password", err)
	}
	cfg.Password = driver.NewSecret("letmein")
	tun, err := sshtunnel.Open(context.Background(), cfg, f.echo, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tun.Close() }()
	roundTrip(t, tun.Addr())
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHostKeyIsChecked(t *testing.T) {
	f := newFixture(t, "")
	cfg := f.config()
	cfg.KnownHosts = filepath.Join(t.TempDir(), "missing")
	if _, err := sshtunnel.Open(context.Background(), cfg, f.echo, nil); err == nil || !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "ssh-keyscan") {
		t.Fatalf("missing file: err = %v", err)
	}
	// A file that knows another server: this one is unknown.
	other, err := sshtest.Start(sshtest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := other.WriteKnownHosts(kh); err != nil {
		t.Fatal(err)
	}
	cfg.KnownHosts = kh
	if _, err := sshtunnel.Open(context.Background(), cfg, f.echo, nil); err == nil || !strings.Contains(err.Error(), "not in known_hosts") {
		t.Fatalf("unknown host: err = %v", err)
	}
	// A file that holds a different key for this address: a mismatch.
	line, _ := strings.CutSuffix(readFile(t, kh), "\n")
	// Replace the address of the other server's line with ours.
	line = f.srv.Addr + line[strings.Index(line, " "):]
	writeFile(t, kh, line+"\n")
	if _, err := sshtunnel.Open(context.Background(), cfg, f.echo, nil); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatch: err = %v", err)
	}
}

func TestOpenConnRewritesTheDSN(t *testing.T) {
	f := newFixture(t, "")
	reg := driver.NewRegistry()
	d := &dialingDriver{}
	if err := reg.Add(d); err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(f.echo)
	cfg := driver.ConnConfig{Name: "c", DSN: "echo://user:pw@" + net.JoinHostPort(host, port) + "/db?x=1"}
	tc := f.config()
	conn, tun, err := sshtunnel.OpenConn(context.Background(), reg, cfg, &tc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tun.Close() }()
	defer func() { _ = conn.Close() }()
	if d.dsn != "echo://user:pw@"+tun.Addr()+"/db?x=1" {
		t.Errorf("driver saw %q, want the tunnel address in place of the host", d.dsn)
	}
	if d.banner != "hello\n" {
		t.Errorf("driver read %q through the tunnel, want the banner", d.banner)
	}
	if cfg.DSN != "echo://user:pw@"+f.echo+"/db?x=1" {
		t.Error("OpenConn changed the caller's config")
	}

	// Without a tunnel config the driver opens directly.
	conn2, tun2, err := sshtunnel.OpenConn(context.Background(), reg, cfg, nil)
	if err != nil || tun2 != nil {
		t.Fatalf("direct: tun=%v err=%v", tun2, err)
	}
	_ = conn2.Close()
	if d.dsn != cfg.DSN {
		t.Errorf("direct open saw %q", d.dsn)
	}

	// A DSN without a usable host fails before any SSH session.
	bad := cfg
	bad.DSN = "sqlite:./x.db"
	if _, _, err := sshtunnel.OpenConn(context.Background(), reg, bad, &tc); err == nil || !strings.Contains(err.Error(), "scheme://host:port") {
		t.Errorf("sqlite DSN: err = %v", err)
	}
}

// dialingDriver connects to the DSN's host and reads one line, like a real
// driver would speak to its server.
type dialingDriver struct {
	dsn    string
	banner string
}

func (d *dialingDriver) Info() driver.Info {
	return driver.Info{Name: "echo", Schemes: []string{"echo"}}
}

func (d *dialingDriver) Open(_ context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d.dsn = cfg.DSN
	_, hostport, _, _ := splitDSN(cfg.DSN)
	c, err := net.Dial("tcp", hostport)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	d.banner, _ = bufio.NewReader(c).ReadString('\n')
	return nopConn{}, nil
}

func splitDSN(dsn string) (string, string, string, bool) {
	i := strings.Index(dsn, "://")
	rest := dsn[i+3:]
	end := strings.IndexAny(rest, "/?")
	auth := rest[:end]
	at := strings.LastIndex(auth, "@")
	return dsn[:i], auth[at+1:], rest[end:], true
}

type nopConn struct{}

func (nopConn) Query(context.Context, driver.Query) (driver.ResultSet, error) {
	return nil, driver.ErrUnsupported
}
func (nopConn) Ping(context.Context) error { return nil }
func (nopConn) Close() error               { return nil }
func (nopConn) Namespaces(context.Context) ([]driver.Namespace, error) {
	return nil, driver.ErrUnsupported
}
func (nopConn) Collections(context.Context, driver.NamespaceRef) ([]driver.Collection, error) {
	return nil, driver.ErrUnsupported
}
func (nopConn) Describe(context.Context, driver.CollectionRef) (*driver.CollectionSchema, error) {
	return nil, driver.ErrUnsupported
}
