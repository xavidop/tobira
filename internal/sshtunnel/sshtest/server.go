// Package sshtest is an in-process SSH server for tests: it accepts one
// public key and an optional password, and serves direct-tcpip channels,
// which is all a tunnel needs. Nothing here is for production.
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Server is a running SSH server.
type Server struct {
	// Addr is "127.0.0.1:port".
	Addr string
	// HostKey is the server's public host key.
	HostKey ssh.PublicKey

	ln      net.Listener
	cfg     *ssh.ServerConfig
	mu      sync.Mutex
	targets []string
	users   []string
	wg      sync.WaitGroup
	closed  chan struct{}
}

// Options configure the server.
type Options struct {
	// Authorized is the one public key accepted; nil accepts none.
	Authorized ssh.PublicKey
	// Password, when not empty, is accepted for any user.
	Password string
}

// Start listens on a loopback port and serves until Close.
func Start(opts Options) (*Server, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	hostSigner, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	s := &Server{HostKey: hostSigner.PublicKey(), closed: make(chan struct{})}
	s.cfg = &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if opts.Authorized != nil && string(key.Marshal()) == string(opts.Authorized.Marshal()) {
				s.sawUser(conn.User())
				return nil, nil
			}
			return nil, errors.New("key not authorized")
		},
	}
	if opts.Password != "" {
		s.cfg.PasswordCallback = func(conn ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == opts.Password {
				s.sawUser(conn.User())
				return nil, nil
			}
			return nil, errors.New("wrong password")
		}
	}
	s.cfg.AddHostKey(hostSigner)
	s.ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.Addr = s.ln.Addr().String()
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

func (s *Server) sawUser(u string) {
	s.mu.Lock()
	s.users = append(s.users, u)
	s.mu.Unlock()
}

// Targets lists the "host:port" addresses clients asked to be forwarded to.
func (s *Server) Targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.targets...)
}

// Users lists the logins that authenticated.
func (s *Server) Users() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.users...)
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer s.wg.Done()
	conn, chans, reqs, err := ssh.NewServerConn(c, s.cfg)
	if err != nil {
		_ = c.Close()
		return
	}
	defer func() { _ = conn.Close() }()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "direct-tcpip" {
			_ = nc.Reject(ssh.UnknownChannelType, "only direct-tcpip")
			continue
		}
		host, port, ok := parseDirect(nc.ExtraData())
		if !ok {
			_ = nc.Reject(ssh.ConnectionFailed, "bad request")
			continue
		}
		target := net.JoinHostPort(host, port)
		s.mu.Lock()
		s.targets = append(s.targets, target)
		s.mu.Unlock()
		remote, err := net.Dial("tcp", target)
		if err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, "dial failed")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			_ = remote.Close()
			continue
		}
		go ssh.DiscardRequests(chReqs)
		go pipe(ch, remote)
	}
}

func pipe(ch ssh.Channel, remote net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, ch); done <- struct{}{} }()
	go func() { _, _ = io.Copy(ch, remote); done <- struct{}{} }()
	<-done
	_ = ch.Close()
	_ = remote.Close()
}

// parseDirect decodes the direct-tcpip payload: host string, port uint32,
// origin host string, origin port uint32.
func parseDirect(b []byte) (host, port string, ok bool) {
	if len(b) < 4 {
		return "", "", false
	}
	n := binary.BigEndian.Uint32(b)
	if uint32(len(b)) < 4+n+4 {
		return "", "", false
	}
	host = string(b[4 : 4+n])
	p := binary.BigEndian.Uint32(b[4+n:])
	return host, itoa(p), true
}

func itoa(n uint32) string {
	if n == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Close stops the server and ends its connections.
func (s *Server) Close() {
	_ = s.ln.Close()
	close(s.closed)
	s.wg.Wait()
}

// WriteKnownHosts writes a known_hosts file at path that trusts s.
func (s *Server) WriteKnownHosts(path string) error {
	line := knownhosts.Line([]string{s.Addr}, s.HostKey) + "\n"
	return os.WriteFile(path, []byte(line), 0o600)
}

// ClientKey generates an ed25519 key, writes it to dir/id (PEM, encrypted
// with passphrase when not empty) and returns its public key and path.
func ClientKey(dir, passphrase string) (ssh.PublicKey, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "id")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	return sshPub, path, nil
}

// Echo starts a TCP server on a loopback port that writes banner and then
// echoes what it reads, for checking that bytes flow through a tunnel.
func Echo(banner string) (addr string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.WriteString(c, banner)
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }, nil
}
