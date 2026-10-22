package milvus

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

const (
	defaultPort = 19530
	defaultDB   = "default"
	// dialTimeout bounds Open when the caller's ctx has no deadline: the
	// first call would otherwise wait for an unreachable host as long as
	// gRPC keeps retrying.
	dialTimeout = 20 * time.Second
	// maxRecvMsg caps one gRPC reply (the SDK allows 2 GiB) at the 64 MiB
	// response budget the other drivers use; a reply over it is ErrLimit.
	// A reply holds at most maxK (1000) entities, so it passes 64 MiB only
	// with vectors of more than about 16 000 dimensions or very large text
	// or JSON fields returned whole; fewer outputFields or a smaller limit
	// get past it.
	maxRecvMsg = 64 << 20
)

// dsnConfig is a parsed milvus:// DSN.
type dsnConfig struct {
	host  string
	port  int
	db    string
	token string
	tls   bool
	// plaintextToken: a token goes to a remote host without TLS because
	// tls=false was given explicitly. Open logs a warning.
	plaintextToken bool
}

// address is host:port, with an IPv6 host in brackets.
func (d dsnConfig) address() string { return net.JoinHostPort(d.host, strconv.Itoa(d.port)) }

// dbName is the alphabet of a Milvus database name.
var dbName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Open dials the server and lists the database's collections once, so a
// bad address, token or database fails here rather than on the first
// query. Errors never contain the token.
func (milvusDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	d, err := parseDSN(cfg)
	if err != nil {
		return nil, redact(err, cfg, "")
	}
	if d.plaintextToken {
		// The host's --debug log (ConnConfig.Logger; discarded when nil).
		// The token itself is never logged.
		cfg.Log().WarnContext(ctx, "milvus: the token is sent without TLS because tls=false is set", "connection", cfg.Name, "host", d.host)
	}
	dctx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	creds := insecure.NewCredentials()
	if d.tls {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: d.host})
	}
	cc, err := grpc.NewClient("passthrough:///"+d.address(),
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecvMsg)),
		grpc.WithUnaryInterceptor(authInterceptor(d.token, d.db)),
	)
	if err != nil {
		return nil, redact(driver.Wrap(driver.ErrSyntax, Name, "open", err), cfg, d.token)
	}
	c := newConn(milvuspb.NewMilvusServiceClient(cc), cc.Close, d.db, cfg)
	if err := c.Ping(dctx); err != nil {
		_ = c.Close()
		if ctx.Err() == nil && dctx.Err() != nil {
			// Our own dial timeout, not the caller's.
			err = driver.Wrap(driver.ErrConnection, Name, "open", fmt.Errorf("no answer from %s within %s", d.address(), dialTimeout))
		}
		return nil, redact(err, cfg, d.token)
	}
	return c, nil
}

// authInterceptor adds the headers the Milvus SDK sends: the token,
// base64-encoded, and the database: the one the request names in its body,
// else the connection's, so a privilege check that reads the header sees
// the same database as the one that reads the body.
func authInterceptor(token, db string) grpc.UnaryClientInterceptor {
	auth := base64.StdEncoding.EncodeToString([]byte(token))
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", auth)
		}
		name := db
		if r, ok := req.(interface{ GetDbName() string }); ok && r.GetDbName() != "" {
			name = r.GetDbName() // the database the request names
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "dbname", name)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// parseDSN reads milvus://[user[:password]@]host[:port][/database][?tls=…&allow_plaintext=…].
// The token is "user:password" for a Milvus user, or an API key (Zilliz
// Cloud) given as the password alone. A non-empty cfg.Password replaces the
// DSN password. ConnConfig.Options may set "tls" and "allow_plaintext".
// TLS is on for any host that is not loopback; plaintext to such a host
// needs both tls=false and allow_plaintext=1 (the rule the chroma driver
// set), and plaintext toward Zilliz Cloud is refused outright.
func parseDSN(cfg driver.ConnConfig) (dsnConfig, error) {
	fail := func(format string, args ...any) (dsnConfig, error) {
		return dsnConfig{}, driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	if driver.Scheme(cfg.DSN) != "milvus" {
		return fail("DSN must start with milvus://")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return fail("invalid DSN") // url errors quote the DSN, token included
	}
	d := dsnConfig{host: u.Hostname(), port: defaultPort, db: defaultDB}
	if d.host == "" {
		d.host = "localhost"
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fail("invalid port %q", p)
		}
		d.port = n
	}
	if path := strings.Trim(u.Path, "/"); path != "" {
		if !dbName.MatchString(path) {
			return fail("the DSN path is the database name: letters, digits and underscores")
		}
		d.db = path
	}
	user := u.User.Username()
	password, _ := u.User.Password()
	if !cfg.Password.IsZero() {
		password = cfg.Password.Reveal()
	}
	switch {
	case user != "" && password != "":
		d.token = user + ":" + password
	case password != "":
		d.token = password
	case user != "":
		return fail("a user needs a password (milvus://user:PASSWORD@host or the password field)")
	}
	tlsSet, allowPlain := false, false
	set := func(k, v string) error {
		switch k {
		case "tls":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("tls must be true or false")
			}
			d.tls, tlsSet = b, true
		case "allow_plaintext":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return errors.New("allow_plaintext must be true or false")
			}
			allowPlain = b
		default:
			return fmt.Errorf("%q is not accepted (only tls and allow_plaintext)", k)
		}
		return nil
	}
	for k, vs := range u.Query() {
		switch strings.ToLower(k) {
		case "token", "api_key", "api-key", "apikey", "key", "password", "user", "username":
			return fail("put the credentials in the password position (milvus://user:PASSWORD@host) or the password field, not in parameter %q", k)
		}
		if err := set(strings.ToLower(k), vs[len(vs)-1]); err != nil {
			return fail("DSN parameter %v", err)
		}
	}
	for k, v := range cfg.Options {
		if err := set(k, v); err != nil {
			return fail("option %v", err)
		}
	}
	loopback := netpolicy.IsLoopback(d.host)
	if !tlsSet {
		d.tls = !loopback
	}
	if !d.tls {
		switch netpolicy.CheckPlaintext(d.host, allowPlain, cloudZones...) {
		case netpolicy.RefusedCloud:
			return fail("Zilliz Cloud is only reachable over TLS: remove tls=false")
		case netpolicy.NeedsOptIn:
			return fail("plaintext to %s (not loopback) needs allow_plaintext=1 as well as tls=false", d.host)
		}
	}
	d.plaintextToken = d.token != "" && !d.tls && !loopback
	return d, nil
}

// cloudZones is Zilliz Cloud (global and China), which only speaks TLS.
var cloudZones = []string{"zillizcloud.com", "zilliz.com", "zillizcloud.com.cn"}

// redact masks the DSN, the password and the token (raw, URL-escaped and
// as the base64 authorization value) in err's message, keeping its kind
// and driver attribution.
func redact(err error, cfg driver.ConnConfig, token string) error {
	if err == nil {
		return nil
	}
	msg := maskToken(driver.RedactMessage(err.Error(), cfg), token)
	out := &driver.Error{Kind: driver.KindOf(err), Driver: Name, Op: "open", Err: errors.New(msg)}
	var de *driver.Error
	if errors.As(err, &de) {
		out.Code = de.Code
		if de.Op != "" {
			out.Op = de.Op
		}
		// Keep context identity (errors.Is(err, context.Canceled)).
		if ctxErr := contextCause(de.Err); ctxErr != nil {
			out.Err = errors.Join(out.Err, ctxErr)
		}
	}
	return out
}

// maskToken replaces every spelling of token in msg.
func maskToken(msg, token string) string {
	if token == "" {
		return msg
	}
	forms := []string{token, url.QueryEscape(token), url.PathEscape(token), base64.StdEncoding.EncodeToString([]byte(token))}
	if _, pw, ok := strings.Cut(token, ":"); ok && pw != "" {
		forms = append(forms, pw, url.QueryEscape(pw), url.PathEscape(pw))
	}
	for _, f := range forms {
		msg = strings.ReplaceAll(msg, f, "xxxxx")
	}
	return msg
}

// contextCause returns context.Canceled or context.DeadlineExceeded when err
// wraps one.
func contextCause(err error) error {
	for _, c := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, c) {
			return c
		}
	}
	return nil
}
