// Package netpolicy holds the transport rules every network driver applies
// before it sends a credential: what counts as this machine, which hosts
// are a TLS-only cloud service, when plain (unencrypted) transport is
// allowed, and how redirects are refused. It uses the standard library
// only, so any driver module may import it.
//
// The plaintext rule: plain transport to a loopback host is always
// allowed; to any other host it needs the driver's own switch plus
// allow_plaintext=1; to a host in one of the driver's cloud zones it is
// refused even then.
package netpolicy

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
)

// IsLoopback reports whether host is this machine: "localhost" (any case)
// or a loopback IP address, with or without the brackets of an IPv6
// literal. Lookalikes such as localhost.example.com or 127.0.0.1.nip.io,
// and the short or numeric forms net.ParseIP refuses (127.1, 2130706433),
// are not loopback, so they fall on the safe side.
func IsLoopback(host string) bool {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") && len(host) > 2 {
		ip := net.ParseIP(host[1 : len(host)-1])
		return ip != nil && ip.To4() == nil && ip.IsLoopback()
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// InCloudZone reports whether host is one of zones or a subdomain of one,
// compared case-insensitively and without a trailing dot. An empty zone
// matches nothing.
func InCloudZone(host string, zones ...string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "" {
		return false
	}
	for _, z := range zones {
		z = strings.TrimSuffix(strings.ToLower(z), ".")
		if z != "" && (h == z || strings.HasSuffix(h, "."+z)) {
			return true
		}
	}
	return false
}

// Verdict is the outcome of CheckPlaintext.
type Verdict int

const (
	// Allowed: plain transport to the host may go ahead.
	Allowed Verdict = iota
	// RefusedCloud: the host is in a TLS-only cloud zone; plain transport
	// is refused whatever the DSN says.
	RefusedCloud
	// NeedsOptIn: the host is not this machine, and allow_plaintext=1 was
	// not given.
	NeedsOptIn
)

// String names the verdict.
func (v Verdict) String() string {
	switch v {
	case Allowed:
		return "allowed"
	case RefusedCloud:
		return "refused (cloud)"
	case NeedsOptIn:
		return "needs allow_plaintext=1"
	}
	return "unknown"
}

// CheckPlaintext applies the plaintext rule to a connection that will not
// use TLS: a host in one of zones is refused, a loopback host is allowed,
// and any other host is allowed only when allowPlaintext is set. Each
// driver words its own error for RefusedCloud and NeedsOptIn.
func CheckPlaintext(host string, allowPlaintext bool, zones ...string) Verdict {
	switch {
	case InCloudZone(host, zones...):
		return RefusedCloud
	case IsLoopback(host), allowPlaintext:
		return Allowed
	}
	return NeedsOptIn
}

// RefuseRedirects is an http.Client CheckRedirect that never follows a
// redirect: the client hands the 3xx back as the reply. Go keeps
// Authorization on a redirect to the same host name (another port, or
// https to http), forwards custom credential headers to any host, and
// replays a 307 or 308 body anywhere, so a client that carries a
// credential must not follow one.
func RefuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// DropRedirectLocation wraps the transport of an http.Client the driver
// does not own (an SDK builds it and follows redirects): it removes
// Location from every 3xx reply, and Go never follows a 3xx without one,
// so the client hands the redirect back as the reply. CloseIdleConnections
// is forwarded to rt.
func DropRedirectLocation(rt http.RoundTripper) http.RoundTripper { return dropLocation{rt} }

type dropLocation struct{ rt http.RoundTripper }

func (d dropLocation) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := d.rt.RoundTrip(r)
	if err == nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Header.Del("Location")
	}
	return resp, err
}

// CloseIdleConnections forwards to the wrapped transport.
func (d dropLocation) CloseIdleConnections() {
	if c, ok := d.rt.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// tlsMismatch are the messages Go's TLS client, net/http and gRPC give
// when a TLS handshake meets a server that answers in clear.
var tlsMismatch = []string{
	"server gave HTTP response to HTTPS client",
	"first record does not look like a TLS handshake",
	"tls: unknown record type",
	"tls: oversized record received",
}

// IsTLSMismatch reports whether err looks like a TLS handshake against a
// server that speaks plain text, so a driver can hint at
// tls=false&allow_plaintext=1. It matches on the message because gRPC
// flattens the cause into a status.
func IsTLSMismatch(err error) bool {
	if err == nil {
		return false
	}
	var rh tls.RecordHeaderError
	if errors.As(err, &rh) {
		return true
	}
	msg := err.Error()
	for _, m := range tlsMismatch {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
