package mongodb

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"

	"github.com/xavidop/tobira/driver"
)

// fallbackDB is the database used when the DSN names none, as in mongosh.
const fallbackDB = "test"

// defaultServerSelection bounds how long Open waits for a server when the
// DSN does not set serverSelectionTimeoutMS.
const defaultServerSelection = 10 * time.Second

// Open builds a client from cfg and pings once, so a bad DSN, an unreachable
// server or bad credentials fail here rather than on the first query.
func (mongoDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	opts, db, err := buildOptions(cfg)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, scrubError(mapError(ctx, "open", err), cfg)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, scrubError(mapError(ctx, "open", err), cfg)
	}
	c := &conn{client: client, defaultDB: db, namedDB: hasPath(cfg.DSN), readOnly: cfg.ReadOnly}
	c.catalog = newCatalogCache(c, catalogTTL)
	return c, nil
}

// buildOptions turns cfg into client options without connecting, and
// returns the default database. It accepts mongodb:// and mongodb+srv://
// URIs. A non-empty ConnConfig.Password replaces the URI's password (the
// URI must then name a user). Reads use read concern "local". The client
// never logs.
func buildOptions(cfg driver.ConnConfig) (*options.ClientOptions, string, error) {
	fail := func(kind error, err error) (*options.ClientOptions, string, error) {
		return nil, "", driver.Wrap(kind, Name, "open", err)
	}
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return fail(driver.ErrUnsupported, fmt.Errorf("unsupported option %q: the mongodb driver takes its settings from the URI", k))
	}
	scheme := strings.ToLower(driver.Scheme(cfg.DSN))
	if scheme != "mongodb" && scheme != "mongodb+srv" {
		return fail(driver.ErrSyntax, errors.New("the DSN must be a mongodb:// or mongodb+srv:// URI"))
	}
	cs, err := connstring.Parse(cfg.DSN)
	if err != nil {
		return fail(driver.ErrSyntax, fmt.Errorf("invalid DSN: %w", err))
	}
	opts := options.Client().ApplyURI(cfg.DSN).SetAppName("tobira").SetReadConcern(readconcern.Local()).
		SetLoggerOptions(options.Logger().SetSink(discardSink{}))
	if !cs.ServerSelectionTimeoutSet {
		opts.SetServerSelectionTimeout(defaultServerSelection)
	}
	if !cfg.Password.IsZero() {
		if opts.Auth == nil || opts.Auth.Username == "" {
			return fail(driver.ErrSyntax, errors.New("a password was given but the DSN names no user"))
		}
		opts.Auth.Password = cfg.Password.Reveal()
		opts.Auth.PasswordSet = true
	}
	db := cs.Database
	if db == "" {
		db = fallbackDB
	}
	return opts, db, nil
}

// discardSink drops the client's log messages, so the MONGODB_LOG_*
// environment variables can never make it write to the terminal.
type discardSink struct{}

func (discardSink) Info(int, string, ...any)    {}
func (discardSink) Error(error, string, ...any) {}

// hasPath reports whether a URI names a database.
func hasPath(dsn string) bool {
	cs, err := connstring.Parse(dsn)
	return err == nil && cs.Database != ""
}

// scrubError masks every password of cfg in the message of a *driver.Error,
// and the other secrets the URI's options carry (optionSecrets), keeping
// its kind, code and the original error (through Unwrap).
func scrubError(err error, cfg driver.ConnConfig) error {
	var de *driver.Error
	if !errors.As(err, &de) || de.Err == nil {
		return err
	}
	msg := de.Err.Error()
	clean := msg
	for _, s := range optionSecrets(cfg.DSN) { // longest first
		clean = strings.ReplaceAll(clean, s, redactedMark)
	}
	clean = driver.RedactMessage(clean, cfg)
	if clean != msg {
		de.Err = maskedError{msg: clean, err: de.Err}
	}
	return err
}

const redactedMark = "xxxxx"

// optionSecrets returns the secret values in a URI's query options, in the
// URI's spelling and decoded, longest first: an option whose name holds
// "password", "secret" or "token" (tlsCertificateKeyFilePassword…), and
// each authMechanismProperties value whose name does
// (AWS_SESSION_TOKEN:…). driver.RedactMessage masks the password keys
// already; the rest are MongoDB's.
func optionSecrets(dsn string) []string {
	_, query, ok := strings.Cut(dsn, "?")
	if !ok {
		return nil
	}
	secret := func(name string) bool {
		n := strings.ToLower(name)
		return strings.Contains(n, "password") || strings.Contains(n, "secret") || strings.Contains(n, "token")
	}
	var out []string
	add := func(raw string) {
		if len(raw) < 4 {
			return // too short to mask without mangling the message
		}
		out = append(out, raw)
		if dec, err := url.QueryUnescape(raw); err == nil && dec != raw {
			out = append(out, dec)
		}
	}
	for kv := range strings.SplitSeq(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		switch {
		case strings.EqualFold(k, "authMechanismProperties"):
			dv := v
			if d, err := url.QueryUnescape(v); err == nil {
				dv = d
			}
			for _, raw := range []string{v, dv} {
				for prop := range strings.SplitSeq(raw, ",") {
					if name, val, ok := strings.Cut(prop, ":"); ok && secret(name) {
						add(val)
					}
				}
			}
		case secret(k):
			add(v)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return slices.Compact(out)
}

// maskedError shows a masked message but keeps the original error reachable.
type maskedError struct {
	msg string
	err error
}

func (m maskedError) Error() string { return m.msg }
func (m maskedError) Unwrap() error { return m.err }
