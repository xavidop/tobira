//go:build integration

package github

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// The integration suite reads the public xavidop/tobira repository on
// api.github.com, read-only, with the token in GITHUB_TOKEN (CI passes the
// workflow's own). Without a token it is skipped: anonymous calls share 60
// requests an hour per address, too few for a shared runner.

const integrationRepo = "xavidop/tobira"

func openIntegration(t *testing.T) driver.Conn {
	t.Helper()
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN is not set")
	}
	if ep := os.Getenv("TOBIRA_TEST_GITHUB_ENDPOINT"); ep != "" {
		t.Logf("endpoint override %s is ignored: this suite is for api.github.com", ep)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := New().Open(ctx, driver.ConnConfig{DSN: "github://" + integrationRepo, Password: driver.NewSecret(token), ReadOnly: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestIntegrationCatalog(t *testing.T) {
	c := openIntegration(t)
	ctx := context.Background()
	nss, err := c.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nss) != 1 || nss[0].Name != integrationRepo || !nss[0].Default {
		t.Errorf("namespaces = %+v", nss)
	}
	colls, err := c.Collections(ctx, driver.NamespaceRef{Name: integrationRepo})
	if err != nil {
		t.Fatal(err)
	}
	if len(colls) != len(collections) {
		t.Errorf("%d collections, want %d", len(colls), len(collections))
	}
	sch, err := c.Describe(ctx, driver.CollectionRef{Namespace: integrationRepo, Name: "issues"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sch.PrimaryKey) != 1 || sch.PrimaryKey[0] != "number" {
		t.Errorf("issues key = %v", sch.PrimaryKey)
	}
	if _, err := c.Collections(ctx, driver.NamespaceRef{Name: "xavidop/no-such-repository-tobira"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing repository: %v", err)
	}
}

func TestIntegrationReads(t *testing.T) {
	c := openIntegration(t)
	ctx := context.Background()
	b, ok := driver.As[driver.Browser](c)
	if !ok {
		t.Fatal("not a Browser")
	}
	rs, err := b.Browse(ctx, driver.CollectionRef{Namespace: integrationRepo, Name: "commits"}, driver.BrowseOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	rows, stats, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || stats.NextCursor == "" {
		t.Errorf("commits page: %d rows, cursor %q", len(rows), stats.NextCursor)
	}
	for _, q := range []string{
		`{"collection":"releases","limit":3}`,
		`{"collection":"branches"}`,
		`{"collection":"issues","state":"all","limit":3}`,
		`{"collection":"pull_requests","state":"closed","limit":3}`,
		`{"collection":"issue_comments","limit":2}`,
		`{"collection":"labels"}`,
		`{"collection":"milestones"}`,
		`{"collection":"workflow_runs","branch":"main","limit":2}`,
		`{"op":"get","collection":"pull_requests","number":46}`,
		`{"op":"get","collection":"tags","name":"v1.4.1"}`,
		`{"op":"get","collection":"commits","sha":"efcb4bb9bb162ede77bf811e31278b6614b7a2fc"}`,
		`{"op":"search","type":"pull_requests","q":"repo:xavidop/tobira is:merged","limit":3}`,
		`{"op":"repositories"}`,
		`{"op":"search","type":"repositories","q":"tobira in:name user:xavidop","limit":3}`,
		`{"op":"rest","path":"/repos/xavidop/tobira/topics"}`,
		`{"op":"graphql","query":"{ repository(owner:\"xavidop\", name:\"tobira\") { stargazerCount } }"}`,
		`{"op":"rate_limit"}`,
	} {
		rs, err := c.Query(ctx, driver.Query{Text: q, ReadOnly: true})
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		rows, _, err := driver.Collect(ctx, rs, 0)
		if err != nil {
			t.Errorf("%s: %v", q, err)
		}
		if len(rows) == 0 && !strings.Contains(q, "milestones") && !strings.Contains(q, "issue_comments") && !strings.Contains(q, `"issues"`) {
			t.Errorf("%s: no rows", q)
		}
	}
	// Issue 46 is a pull request: the issues collection does not serve it.
	if _, err := c.Query(ctx, driver.Query{Text: `{"op":"get","collection":"issues","number":46}`, ReadOnly: true}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a pull request read through issues: %v", err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"op":"rest","method":"POST","path":"/repos/xavidop/tobira/issues","body":{"title":"x"}}`}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("a write on a read-only connection: %v", err)
	}
}
