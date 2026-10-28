package github

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// Info is static and every capability it declares is found on a connection.
func TestInfoDeclaresWhatTheConnOffers(t *testing.T) {
	info := New().Info()
	if info.Name != Name || info.Dialect == "" || info.Kind != driver.KindOther || !contains(info.Schemes, "github") {
		t.Errorf("Info = %+v", info)
	}
	if !driver.HasCapability(info, driver.CapReadOnly) {
		t.Error("CapReadOnly is not declared")
	}
	f := newFakeGitHub(t)
	c := f.open(t, false)
	for _, cap := range info.Capabilities {
		var ok bool
		switch cap {
		case driver.CapBrowser:
			_, ok = driver.As[driver.Browser](c)
		case driver.CapMutator:
			_, ok = driver.As[driver.Mutator](c)
		case driver.CapExplainer:
			_, ok = driver.As[driver.Explainer](c)
		case driver.CapCompleter:
			_, ok = driver.As[driver.Completer](c)
		case driver.CapReadOnly:
			ok = true
		default:
			t.Errorf("capability %q is declared but not checked here", cap)
			continue
		}
		if !ok {
			t.Errorf("capability %q is declared but driver.As does not find it", cap)
		}
	}
	for _, got := range driver.CapabilitiesOf(c) {
		if !contains(info.Capabilities, got) {
			t.Errorf("the connection offers %q, which Info does not declare", got)
		}
	}
}

func contains[T comparable](xs []T, x T) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Open pings the pinned repository with the bearer token, and Ping keeps
// working afterwards.
func TestOpenPingsThePinnedRepository(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	reqs := f.requestsTo("/repos/octo/widgets")
	if len(reqs) != 1 || reqs[0].Method != "GET" || reqs[0].Auth != "Bearer "+fakeToken {
		t.Fatalf("Open sent %+v, want one authenticated GET of the repository", reqs)
	}
	for _, r := range f.log() {
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") == "" || !strings.HasPrefix(r.Header.Get("User-Agent"), "tobira-github-driver/") {
			t.Errorf("request headers = %v", r.Header)
		}
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Without a repository, Ping reads the rate limit, which costs nothing.
func TestOpenWithoutRepositoryPingsTheRateLimit(t *testing.T) {
	f := newFakeGitHub(t)
	f.openDSN(t, f.dsn("", ""), false, fakeToken)
	if reqs := f.requestsTo("/rate_limit"); len(reqs) != 1 {
		t.Errorf("requests to /rate_limit: %d, want 1 (log %+v)", len(reqs), f.log())
	}
}

// A rejected token fails Open with ErrAuth, and the token stays out of the
// message.
func TestOpenWithBadTokenIsErrAuth(t *testing.T) {
	f := newFakeGitHub(t)
	_, err := New().Open(context.Background(), f.cfg(false, "ghp_wrong_token_9876543210"))
	wantKind(t, "open with a bad token", err, driver.ErrAuth)
	if strings.Contains(err.Error(), "ghp_wrong") {
		t.Errorf("the error carries the token: %v", err)
	}
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("the error does not carry GitHub's message: %v", err)
	}
}

// A token in the DSN's password position is accepted and never printed.
func TestOpenTakesTheTokenFromTheDSNPassword(t *testing.T) {
	f := newFakeGitHub(t)
	dsn := "github://:" + fakeToken + "@octo/widgets?endpoint=" + url.QueryEscape(f.base())
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if reqs := f.log(); len(reqs) == 0 || reqs[0].Auth != "Bearer "+fakeToken {
		t.Errorf("the DSN token was not sent: %+v", reqs)
	}
	bad := "github://:ghp_in_dsn_badtoken@octo/widgets?endpoint=" + url.QueryEscape(f.base())
	_, err = New().Open(context.Background(), driver.ConnConfig{DSN: bad})
	wantKind(t, "open with a bad DSN token", err, driver.ErrAuth)
	if strings.Contains(err.Error(), "ghp_in_dsn") {
		t.Errorf("the error carries the DSN token: %v", err)
	}
}

// A repository that does not exist fails Open with ErrNotFound.
func TestOpenMissingRepositoryIsErrNotFound(t *testing.T) {
	f := newFakeGitHub(t)
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: f.dsn("octo", "nothing"), Password: driver.NewSecret(fakeToken)})
	wantKind(t, "open on a missing repository", err, driver.ErrNotFound)
}

// An endpoint nobody listens on fails Open with ErrConnection.
func TestOpenUnreachableEndpointIsErrConnection(t *testing.T) {
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: "github://octo/widgets?endpoint=http://127.0.0.1:1"})
	wantKind(t, "open on an unreachable endpoint", err, driver.ErrConnection)
}

// Public data is readable without a token; no Authorization header is sent.
func TestOpenWithoutTokenReadsPublicData(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("octo", "widgets"), false, "")
	_, rows, _ := queryText(t, c, `{"collection":"labels"}`)
	if len(rows) != 7 {
		t.Errorf("labels without a token: %d rows, want 7", len(rows))
	}
	for _, r := range f.log() {
		if r.Auth != "" {
			t.Errorf("an Authorization header was sent without a token: %q", r.Auth)
		}
	}
	_, err := c.Query(context.Background(), driver.Query{Text: `{"op":"get","collection":"issues","number":1,"repo":"acme/beta"}`})
	wantKind(t, "a private repository without a token", err, driver.ErrNotFound)
}

// Close is idempotent and every call on a closed connection is ErrConnection.
func TestCloseIsIdempotentAndClosedConnRefuses(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	ctx := context.Background()
	ref := driver.CollectionRef{Namespace: "octo/widgets", Name: "issues"}
	calls := map[string]func() error{
		"ping":        func() error { return c.Ping(ctx) },
		"namespaces":  func() error { _, err := c.Namespaces(ctx); return err },
		"collections": func() error { _, err := c.Collections(ctx, driver.NamespaceRef{Name: "octo/widgets"}); return err },
		"describe":    func() error { _, err := c.Describe(ctx, ref); return err },
		"query":       func() error { _, err := c.Query(ctx, driver.Query{Text: `{"collection":"issues"}`}); return err },
		"browse":      func() error { _, err := c.Browse(ctx, ref, driver.BrowseOptions{}); return err },
		"insert": func() error {
			_, err := c.Insert(ctx, ref, driver.Object{{Key: "title", Value: driver.String("x")}})
			return err
		},
		"update": func() error {
			return c.Update(ctx, ref, driver.Object{{Key: "number", Value: driver.Int(1)}}, driver.Object{{Key: "title", Value: driver.String("x")}})
		},
		"delete": func() error { return c.Delete(ctx, ref, driver.Object{{Key: "number", Value: driver.Int(1)}}) },
		"explain": func() error {
			_, err := c.Explain(ctx, driver.Query{Text: `{"op":"rate_limit"}`}, driver.ExplainOptions{})
			return err
		},
		"complete": func() error { _, err := c.Complete(ctx, driver.CompletionRequest{Text: `{"`, Cursor: 2}); return err },
	}
	before := len(f.log())
	for name, call := range calls {
		err := call()
		if !errors.Is(err, driver.ErrConnection) {
			t.Errorf("%s on a closed connection: %v, want ErrConnection", name, err)
		}
	}
	if after := len(f.log()); after != before {
		t.Errorf("a closed connection sent %d requests", after-before)
	}
}
