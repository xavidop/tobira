// Package mcphttp is the network policy of `tobira mcp --http`: which
// addresses it may bind, and the guard every request passes before it
// reaches the MCP server (DNS-rebinding defence, bearer token, body limit).
//
// It has no MCP dependency, so the CLI can refuse a bind before anything
// is served, and the server applies the same rules.
package mcphttp

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxBody is the largest request body the guard lets through: 4 MiB.
const MaxBody = 4 << 20

// ErrInsecureBind refuses a non-loopback address without --insecure-bind.
var ErrInsecureBind = errors.New("--http: a non-loopback address needs --insecure-bind (anyone who can reach it could use the exposed connections)")

// Bind is an address to listen on.
type Bind struct {
	Host string // never empty; no brackets around an IPv6 literal
	Port string // decimal, 0 to 65535 (0 picks a free port)
}

// ParseBind parses host:port. An empty host means 127.0.0.1, never every
// interface. Errors never quote the input.
func ParseBind(addr string) (Bind, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return Bind{}, errors.New("--http: want host:port, such as 127.0.0.1:8765 or :8765")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 || strconv.Itoa(n) != port {
		return Bind{}, errors.New("--http: the port must be a number from 0 to 65535")
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if !validHost(host) {
		return Bind{}, errors.New("--http: the host must be an IP address or a host name")
	}
	return Bind{Host: host, Port: port}, nil
}

// String is the address as host:port, an IPv6 literal in brackets.
func (b Bind) String() string { return net.JoinHostPort(b.Host, b.Port) }

// ListenAddress is the address to listen on: String, except that
// localhost is 127.0.0.1, so the system resolver never decides what a
// "loopback" bind listens on.
func (b Bind) ListenAddress() string {
	if strings.EqualFold(b.Host, "localhost") {
		return net.JoinHostPort("127.0.0.1", b.Port)
	}
	return b.String()
}

// LoopbackAddr reports whether a listener's address is a loopback IP: the
// check, after listening, that a loopback bind really is one.
func LoopbackAddr(a net.Addr) bool {
	t, ok := a.(*net.TCPAddr)
	return ok && t.IP.IsLoopback()
}

// DefaultBodyTimeout bounds reading one request body.
const DefaultBodyTimeout = 30 * time.Second

// Loopback reports whether the host is a loopback address or localhost.
// Other names are never resolved, so they count as non-loopback.
func (b Bind) Loopback() bool { return loopbackHost(b.Host) }

// CheckBind refuses a non-loopback bind unless insecure (--insecure-bind).
func CheckBind(b Bind, insecure bool) error {
	if !b.Loopback() && !insecure {
		return ErrInsecureBind
	}
	return nil
}

// ParseAllowedHost checks one --allowed-host value, a host name or IP
// address, optionally with a port, and returns it lowercased. Errors never
// quote the input.
func ParseAllowedHost(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	host, port := s, ""
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || strings.HasPrefix(p, "+") {
			return "", errors.New("--allowed-host: the port must be a number from 1 to 65535")
		}
		host, port = h, strconv.Itoa(n) // "080" is port 80
	}
	if host == "" || !validHost(host) {
		return "", errors.New("--allowed-host: want a host name or IP address, optionally with :port")
	}
	if port != "" {
		return net.JoinHostPort(host, port), nil
	}
	return host, nil
}

// validHost accepts an IP address or a DNS-style name.
func validHost(h string) bool {
	if net.ParseIP(h) != nil {
		return true
	}
	for _, r := range h {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_'
		if !ok {
			return false
		}
	}
	return h != ""
}

// loopbackHost reports whether h (without brackets) is localhost or a
// loopback IP address.
func loopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// GuardConfig configures Guard.
type GuardConfig struct {
	// Bind is the address as configured, with the real port when 0 was
	// asked for.
	Bind Bind
	// Listen is the address the listener reports.
	Listen string
	// AllowedHosts are extra Host values (ParseAllowedHost): a name alone
	// matches on the bound port, a name with a port matches exactly.
	AllowedHosts []string
	// BodyTimeout bounds reading a request body; 0 is DefaultBodyTimeout.
	BodyTimeout time.Duration
	// Token is the bearer token. Empty refuses every request, since an
	// empty bearer token is never accepted.
	Token []byte
}

// Guard admits a request only when, in order:
//  1. its Host is the bound address, a loopback name on the bound port when
//     bound to loopback, or an allowed host (else 403): a page that points
//     its own name at 127.0.0.1 (DNS rebinding) is refused;
//  2. any Origin it carries is loopback (else 403);
//  3. it carries Authorization: Bearer <token> (else 401 with
//     WWW-Authenticate: Bearer), compared in constant time.
//
// The body of an admitted request is limited to MaxBody.
func Guard(next http.Handler, c GuardConfig) http.Handler {
	hosts := map[string]bool{strings.ToLower(c.Bind.String()): true, strings.ToLower(c.Listen): true}
	if c.Bind.Loopback() {
		for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
			hosts[net.JoinHostPort(h, c.Bind.Port)] = true
		}
	}
	for _, h := range c.AllowedHosts {
		if _, _, err := net.SplitHostPort(h); err != nil {
			h = net.JoinHostPort(h, c.Bind.Port)
		}
		hosts[strings.ToLower(h)] = true
	}
	if c.Bind.Port == "80" {
		// Port 80 is http's default, so a client may leave it out.
		for h := range maps.Clone(hosts) {
			if bare, port, err := net.SplitHostPort(h); err == nil && port == "80" {
				if strings.Contains(bare, ":") {
					bare = "[" + bare + "]"
				}
				hosts[bare] = true
			}
		}
	}
	bodyTimeout := c.BodyTimeout
	if bodyTimeout <= 0 {
		bodyTimeout = DefaultBodyTimeout
	}
	want := sha256.Sum256(c.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[strings.ToLower(r.Host)] {
			http.Error(w, "forbidden: this Host is not served here", http.StatusForbidden)
			return
		}
		for _, o := range r.Header.Values("Origin") {
			if !loopbackOrigin(o) {
				http.Error(w, "forbidden: cross-origin requests are not served", http.StatusForbidden)
				return
			}
		}
		got, ok := bearer(r.Header.Get("Authorization"))
		sum := sha256.Sum256([]byte(got))
		// Both sides are hashed, so the comparison is on equal lengths and
		// time does not depend on the token's length or content.
		if subtle.ConstantTimeCompare(sum[:], want[:]) != 1 || !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized: pass Authorization: Bearer <token>", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// A body must arrive within bodyTimeout. net/http lifts the
			// deadline itself once the body is read to its end (its
			// background read starts with none), so a long call is not cut
			// off; a GET (an SSE stream) never gets one. Without a real
			// connection there is no deadline to set.
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyTimeout))
		}
		next.ServeHTTP(w, r)
	})
}

// bearer extracts the token of an Authorization: Bearer header.
func bearer(h string) (string, bool) {
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

// loopbackOrigin reports whether an Origin header names a loopback host
// over http or https.
func loopbackOrigin(o string) bool {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return loopbackHost(u.Hostname())
}
