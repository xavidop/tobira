package sshtunnel

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// defaultPorts fills in the port when a URL-style DSN leaves it out, by
// scheme. A scheme missing here needs the port in the DSN.
var defaultPorts = map[string]string{
	"postgres": "5432", "postgresql": "5432", "pgvector": "5432", "cockroachdb": "26257", "redshift": "5439", "greenplum": "5432",
	"mysql": "3306", "mariadb": "3306", "singlestore": "3306",
	"sqlserver": "1433", "mssql": "1433",
	"oracle":     "1521",
	"clickhouse": "9000",
	"mongodb":    "27017",
	"redis":      "6379", "rediss": "6379",
	"cassandra": "9042",
	"neo4j":     "7687", "bolt": "7687", "neo4j+s": "7687", "bolt+s": "7687",
	"qdrant": "6334",
	"http":   "80", "https": "443",
}

// authority splits a URL-style DSN around its host part: before is the text
// up to the host (scheme, "://" and any userinfo), hostport the host with
// its optional port, and after the rest. ok is false for a DSN that is not
// of the form scheme://[user[:password]@]host[:port]...
func authority(dsn string) (before, hostport, after string, ok bool) {
	i := strings.Index(dsn, "://")
	if i <= 0 {
		return "", "", "", false
	}
	rest := dsn[i+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	auth := rest[:end]
	if at := strings.LastIndex(auth, "@"); at >= 0 {
		return dsn[:i+3+at+1], auth[at+1:], rest[end:], true
	}
	return dsn[:i+3], auth, rest[end:], true
}

// Target is the "host:port" a DSN names, with the scheme's default port
// when the DSN has none. It refuses a DSN that is not URL-style, names
// several hosts, or leaves out a port with no known default: the tunnel
// forwards to exactly one address.
func Target(dsn string) (string, error) {
	before, hostport, _, ok := authority(dsn)
	if !ok {
		return "", errors.New("ssh tunnel: the DSN must be of the form scheme://host:port/... so the host to forward to is known")
	}
	if strings.Contains(hostport, ",") {
		return "", errors.New("ssh tunnel: the DSN names several hosts; a tunnel forwards to one")
	}
	scheme := strings.ToLower(before[:strings.Index(before, "://")])
	if strings.HasSuffix(scheme, "+srv") {
		return "", errors.New("ssh tunnel: a +srv DSN resolves its hosts by DNS; name the host and port instead")
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		// No port (or a bare IPv6 literal in brackets).
		host = strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
		port = defaultPorts[scheme]
		if port == "" {
			return "", fmt.Errorf("ssh tunnel: the DSN has no port and %q has no default; add the port", scheme)
		}
	}
	if host == "" {
		return "", errors.New("ssh tunnel: the DSN has no host")
	}
	return net.JoinHostPort(host, port), nil
}

// Rewrite returns dsn with its host and port replaced by addr, the
// tunnel's loopback address.
func Rewrite(dsn, addr string) (string, error) {
	before, _, after, ok := authority(dsn)
	if !ok {
		return "", errors.New("ssh tunnel: the DSN must be of the form scheme://host:port/path")
	}
	return before + addr + after, nil
}
