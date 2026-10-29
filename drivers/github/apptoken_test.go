package github

import (
	"context"
	"net/http"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// A GitHub App's installation token, such as the one Actions gives a
// workflow, cannot read /user: the connection then has no viewer, and an
// owner's repositories are still listed through the owner's own endpoint.
func TestAppTokenWithoutAViewerListsAnOwner(t *testing.T) {
	f := newFakeGitHub(t)
	f.setHook(func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path == "/user" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
			return true
		}
		return false
	})
	c := f.openDSN(t, f.dsn("hubot", ""), true, fakeToken)
	ctx := context.Background()
	nss, err := c.Namespaces(ctx)
	if err != nil {
		t.Fatalf("Namespaces with an app token: %v", err)
	}
	if len(nss) < 2 || nss[0].Name != "hubot" || nss[1].Name != "hubot/scripts" {
		t.Errorf("namespaces = %+v", nss)
	}
	if n := len(f.requestsTo("/user")); n != 1 {
		t.Errorf("/user was asked %d times, want once (the refusal is remembered)", n)
	}
	if len(f.requestsTo("/users/hubot/repos")) == 0 {
		t.Error("the owner's repositories were not listed through /users/hubot/repos")
	}
	rs, err := c.Browse(ctx, driver.CollectionRef{Namespace: "hubot", Name: "issues"}, driver.BrowseOptions{Limit: 5})
	if err != nil {
		t.Fatalf("owner issues with an app token: %v", err)
	}
	if _, _, err := driver.Collect(ctx, rs, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Query(ctx, driver.Query{Text: `{"collection":"repositories"}`, ReadOnly: true}); err != nil {
		t.Errorf("repositories query with an app token: %v", err)
	}
}
