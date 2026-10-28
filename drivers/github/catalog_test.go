package github

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func namespaceNames(nss []driver.Namespace) []string {
	out := make([]string, len(nss))
	for i, ns := range nss {
		out[i] = ns.Name
	}
	return out
}

// A pinned repository is the one namespace, and the default.
func TestNamespacesOfPinnedRepository(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	nss, err := c.Namespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nss) != 1 || nss[0].Name != "octo/widgets" || !nss[0].Default || nss[0].Label != "repository" || nss[0].Comment != "Widgets for everyone" {
		t.Errorf("Namespaces = %+v", nss)
	}
}

// A user owner is listed through /users/{owner}/repos.
func TestNamespacesOfUserOwner(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("hubot", ""), false, fakeToken)
	nss, err := c.Namespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := namespaceNames(nss); !slices.Equal(got, []string{"hubot/scripts"}) {
		t.Errorf("Namespaces = %v", got)
	}
	if len(f.requestsTo("/users/hubot")) != 1 {
		t.Error("the owner's type was not looked up")
	}
	reqs := f.requestsTo("/users/hubot/repos")
	if len(reqs) != 1 || reqs[0].Query.Get("type") != "owner" || reqs[0].Query.Get("per_page") != "100" || reqs[0].Query.Get("page") != "1" {
		t.Errorf("listing requests = %+v", reqs)
	}
	for _, ns := range nss {
		if ns.Default {
			t.Errorf("%s is marked default without a pinned repository", ns.Name)
		}
	}
}

// An organization owner is listed through /orgs/{owner}/repos, private
// repositories included with a token.
func TestNamespacesOfOrganizationOwner(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("acme", ""), false, fakeToken)
	nss, err := c.Namespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := namespaceNames(nss); !slices.Equal(got, []string{"acme/alpha", "acme/beta"}) {
		t.Errorf("Namespaces = %v", got)
	}
	reqs := f.requestsTo("/orgs/acme/repos")
	if len(reqs) != 1 || reqs[0].Query.Get("type") != "all" {
		t.Errorf("listing requests = %+v", reqs)
	}
	if len(f.requestsTo("/users/acme/repos")) != 0 {
		t.Error("an organization was listed through /users")
	}
}

// Without an owner the token's own repositories are listed, with the ones
// it collaborates on and its organizations'; with the token's own login as
// owner, only the ones it owns.
func TestNamespacesOfTheTokenOwner(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("", ""), false, fakeToken)
	nss, err := c.Namespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"acme/alpha", "acme/beta", "hubot/scripts", "octo/gadgets", "octo/widgets"}
	if got := namespaceNames(nss); !slices.Equal(got, want) {
		t.Errorf("Namespaces = %v, want %v", got, want)
	}
	reqs := f.requestsTo("/user/repos")
	if len(reqs) != 1 || reqs[0].Query.Get("affiliation") != "owner,collaborator,organization_member" {
		t.Errorf("listing requests = %+v", reqs)
	}

	own := f.openDSN(t, f.dsn("octo", ""), false, fakeToken)
	f.resetLog()
	nss, err = own.Namespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := namespaceNames(nss); !slices.Equal(got, []string{"octo/gadgets", "octo/widgets"}) {
		t.Errorf("Namespaces of the token's login = %v", got)
	}
	reqs = f.requestsTo("/user/repos")
	if len(reqs) != 1 || reqs[0].Query.Get("affiliation") != "owner" {
		t.Errorf("listing requests = %+v", reqs)
	}
}

// Anonymous and without an owner there is nothing to list: ErrAuth.
func TestNamespacesAnonymousWithoutOwnerIsErrAuth(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("", ""), false, "")
	_, err := c.Namespaces(context.Background())
	wantKind(t, "anonymous Namespaces", err, driver.ErrAuth)
	if len(f.writes()) != 0 || len(f.requestsTo("/user")) != 0 {
		t.Errorf("requests = %+v", f.log())
	}
}

// Collections are the ten fixed kinds, in order; only issues carry a row
// estimate, GitHub's open_issues_count.
func TestCollectionsListsTheTenKinds(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	cols, err := c.Collections(context.Background(), driver.NamespaceRef{Name: "octo/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"issues", "pull_requests", "issue_comments", "commits", "branches", "tags", "releases", "labels", "milestones", "workflow_runs"}
	got := make([]string, len(cols))
	for i, col := range cols {
		got[i] = col.Ref.Name
		if col.Ref.Namespace != "octo/widgets" || col.Type != driver.CollectionTable || col.Comment == "" {
			t.Errorf("collection %+v", col)
		}
		switch {
		case col.Ref.Name == "issues" && col.RowEstimate != f.openIssues("octo", "widgets"):
			t.Errorf("issues RowEstimate = %d, want open_issues_count %d", col.RowEstimate, f.openIssues("octo", "widgets"))
		case col.Ref.Name != "issues" && col.RowEstimate != -1:
			t.Errorf("%s RowEstimate = %d, want -1", col.Ref.Name, col.RowEstimate)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("Collections = %v, want %v", got, want)
	}
	// An empty namespace means the pinned repository.
	again, err := c.Collections(context.Background(), driver.NamespaceRef{})
	if err != nil || len(again) != len(want) || again[0].Ref.Namespace != "octo/widgets" {
		t.Errorf("Collections of the default namespace = %+v, %v", again, err)
	}
}

// A missing repository, or a namespace that is not owner/repo, is ErrNotFound.
func TestCollectionsOfMissingRepositoryIsErrNotFound(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, err := c.Collections(context.Background(), driver.NamespaceRef{Name: "octo/nothing"})
	wantKind(t, "Collections of a missing repository", err, driver.ErrNotFound)
	_, err = c.Collections(context.Background(), driver.NamespaceRef{Name: "not-a-repo"})
	wantKind(t, "Collections of a bare name", err, driver.ErrNotFound)
}

// Without a pinned repository the namespace must be given.
func TestCollectionsWithoutRepositoryIsErrSyntax(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.openDSN(t, f.dsn("octo", ""), false, fakeToken)
	_, err := c.Collections(context.Background(), driver.NamespaceRef{})
	wantKind(t, "Collections without a namespace", err, driver.ErrSyntax)
	cols, err := c.Collections(context.Background(), driver.NamespaceRef{Name: "octo/gadgets"})
	if err != nil || len(cols) != 10 {
		t.Errorf("Collections of octo/gadgets = %d, %v", len(cols), err)
	}
}

func fieldByName(t *testing.T, s *driver.CollectionSchema, name string) driver.Field {
	t.Helper()
	for _, f := range s.Fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field %q in %s", name, s.Ref.Name)
	return driver.Field{}
}

// Describe is static: the key, the columns GitHub computes as Generated,
// the number it assigns as AutoIncrement, the editable ones plain.
func TestDescribeIssues(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	s, err := c.Describe(context.Background(), driver.CollectionRef{Name: "issues"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref != (driver.CollectionRef{Namespace: "octo/widgets", Name: "issues"}) || s.Type != driver.CollectionTable || !slices.Equal(s.PrimaryKey, []string{"number"}) {
		t.Errorf("schema = %+v", s)
	}
	if s.RowEstimate != f.openIssues("octo", "widgets") || s.SizeBytes != -1 {
		t.Errorf("RowEstimate = %d, SizeBytes = %d", s.RowEstimate, s.SizeBytes)
	}
	number := fieldByName(t, s, "number")
	if !number.AutoIncrement || number.Generated || number.Nullable || number.Type != driver.TypeInt {
		t.Errorf("number = %+v, want an auto-increment, non-null key", number)
	}
	title := fieldByName(t, s, "title")
	if title.Generated || title.Nullable || title.Type != driver.TypeString {
		t.Errorf("title = %+v, want a required, writable string", title)
	}
	for _, name := range []string{"user", "comments", "created_at", "html_url"} {
		if fd := fieldByName(t, s, name); !fd.Generated || !fd.Nullable {
			t.Errorf("%s = %+v, want generated and nullable", name, fd)
		}
	}
	labels := fieldByName(t, s, "labels")
	if labels.Generated || labels.Type != driver.TypeList {
		t.Errorf("labels = %+v, want a writable list", labels)
	}
	if fd := fieldByName(t, s, "created_at"); fd.Type != driver.TypeTime || fd.NativeType != "timestamp" {
		t.Errorf("created_at = %+v", fd)
	}
}

// A label's name is its key and writable (a rename), so it is neither
// generated nor auto-increment; its id is computed.
func TestDescribeLabelsNameIsWritable(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	s, err := c.Describe(context.Background(), driver.CollectionRef{Namespace: "octo/widgets", Name: "labels"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.PrimaryKey, []string{"name"}) {
		t.Errorf("PrimaryKey = %v", s.PrimaryKey)
	}
	name := fieldByName(t, s, "name")
	if name.Generated || name.AutoIncrement || name.Nullable {
		t.Errorf("name = %+v, want a plain, required key", name)
	}
	if id := fieldByName(t, s, "id"); !id.Generated || id.AutoIncrement {
		t.Errorf("id = %+v, want generated", id)
	}
	if s.RowEstimate != -1 {
		t.Errorf("RowEstimate = %d, want -1", s.RowEstimate)
	}
}

// A collection without writes says so, and all its columns are generated.
func TestDescribeTagsIsReadOnly(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	s, err := c.Describe(context.Background(), driver.CollectionRef{Name: "tags"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Comment, "read-only") {
		t.Errorf("Comment = %q, want a read-only note", s.Comment)
	}
	for _, fd := range s.Fields {
		if !fd.Generated || fd.AutoIncrement {
			t.Errorf("%s = %+v, want generated", fd.Name, fd)
		}
	}
}

// A collection GitHub does not have, or a repository that is missing, is
// ErrNotFound.
func TestDescribeMissingIsErrNotFound(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.open(t, false)
	_, err := c.Describe(context.Background(), driver.CollectionRef{Name: "people"})
	wantKind(t, "Describe of an unknown collection", err, driver.ErrNotFound)
	_, err = c.Describe(context.Background(), driver.CollectionRef{Namespace: "octo/nothing", Name: "issues"})
	wantKind(t, "Describe in a missing repository", err, driver.ErrNotFound)
}
