package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

const (
	// maxRepos caps a repository listing (10 requests of 100).
	maxRepos = 1000
	// repoTTL is how long repository metadata is kept.
	repoTTL = 60 * time.Second
)

// repoInfo is what Collections and Describe need to know of a repository.
type repoInfo struct {
	fullName      string
	defaultBranch string
	openIssues    int64 // open issues and pull requests, as GitHub counts them
	private       bool
	description   string
	at            time.Time
}

// repoCache keeps repository metadata for repoTTL, and the token's login.
type repoCache struct {
	c      *conn
	mu     sync.Mutex
	repos  map[string]*repoInfo  // by lower-cased owner/repo
	owners map[string]*ownerInfo // by lower-cased login
	login  string
	who    bool // login was looked up
}

// ownerInfo is what an owner namespace needs to know of a user or
// organization.
type ownerInfo struct {
	login string // as GitHub spells it
	org   bool
	at    time.Time
}

func newRepoCache(c *conn) *repoCache {
	return &repoCache{c: c, repos: map[string]*repoInfo{}, owners: map[string]*ownerInfo{}}
}

// owner reads a user or organization, from the cache when fresh. A missing
// owner is ErrNotFound.
func (k *repoCache) owner(ctx context.Context, op, login string) (*ownerInfo, error) {
	key := strings.ToLower(login)
	k.mu.Lock()
	info, ok := k.owners[key]
	k.mu.Unlock()
	if ok && time.Since(info.at) < repoTTL {
		return info, nil
	}
	raw, err := k.c.client.getJSON(ctx, "/users/"+url.PathEscape(login), nil)
	if err != nil {
		return nil, k.c.fail(ctx, op, err)
	}
	var body struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, k.c.fail(ctx, op, err)
	}
	info = &ownerInfo{login: body.Login, org: body.Type == "Organization", at: time.Now()}
	if info.login == "" {
		info.login = login
	}
	k.mu.Lock()
	k.owners[key] = info
	k.mu.Unlock()
	return info, nil
}

// get reads a repository, from the cache when fresh. A missing repository
// is ErrNotFound.
func (k *repoCache) get(ctx context.Context, op, owner, repo string) (*repoInfo, error) {
	key := strings.ToLower(owner + "/" + repo)
	k.mu.Lock()
	info, ok := k.repos[key]
	k.mu.Unlock()
	if ok && time.Since(info.at) < repoTTL {
		return info, nil
	}
	raw, err := k.c.client.getJSON(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo), nil)
	if err != nil {
		return nil, k.c.fail(ctx, op, err)
	}
	var body struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		OpenIssues    int64  `json:"open_issues_count"`
		Private       bool   `json:"private"`
		Description   string `json:"description"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, k.c.fail(ctx, op, err)
	}
	info = &repoInfo{fullName: body.FullName, defaultBranch: body.DefaultBranch, openIssues: body.OpenIssues, private: body.Private, description: body.Description, at: time.Now()}
	if info.fullName == "" {
		info.fullName = owner + "/" + repo
	}
	k.mu.Lock()
	k.repos[key] = info
	k.mu.Unlock()
	return info, nil
}

// viewer is the token's login, or "" without a token.
func (k *repoCache) viewer(ctx context.Context, op string) (string, error) {
	if k.c.client.token == "" {
		return "", nil
	}
	k.mu.Lock()
	login, done := k.login, k.who
	k.mu.Unlock()
	if done {
		return login, nil
	}
	raw, err := k.c.client.getJSON(ctx, "/user", nil)
	if err != nil {
		return "", k.c.fail(ctx, op, err)
	}
	var body struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", k.c.fail(ctx, op, err)
	}
	k.mu.Lock()
	k.login, k.who = body.Login, true
	k.mu.Unlock()
	return body.Login, nil
}

// splitRepo reads a namespace name as owner and repository.
func splitRepo(ns string) (owner, repo string, ok bool) {
	owner, repo, found := strings.Cut(ns, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") || !validOwner(owner) || !repoRe.MatchString(repo) {
		return "", "", false
	}
	return owner, repo, true
}

// scope is where a call works: one repository, or every repository of an
// owner (repo empty).
type scope struct{ owner, repo string }

// ownerWide reports an owner scope.
func (s scope) ownerWide() bool { return s.repo == "" }

// name is the namespace name: owner/repo, or the owner.
func (s scope) name() string {
	if s.repo == "" {
		return s.owner
	}
	return s.owner + "/" + s.repo
}

// resolveScope names the scope a call works on: ns when given (owner/repo
// or an owner), else the DSN's repository or owner, else the token's own
// login.
func (c *conn) resolveScope(ctx context.Context, op, ns string) (scope, error) {
	if ns == "" {
		switch {
		case c.dsn.repo != "":
			return scope{c.dsn.owner, c.dsn.repo}, nil
		case c.dsn.owner != "":
			return scope{owner: c.dsn.owner}, nil
		}
		viewer, err := c.repos.viewer(ctx, op)
		if err != nil {
			return scope{}, err
		}
		if viewer == "" {
			return scope{}, syntaxErr(op, `name the repository or owner: a namespace "owner/repo" or "owner", "repo" in the query, or github://owner/repo in the DSN`)
		}
		return scope{owner: viewer}, nil
	}
	if strings.Contains(ns, "/") {
		owner, repo, ok := splitRepo(ns)
		if !ok {
			return scope{}, notFoundErr(op, "%q is not a repository: a namespace is owner/repo, or an owner", ns)
		}
		return scope{owner, repo}, nil
	}
	if !validOwner(ns) {
		return scope{}, notFoundErr(op, "%q is not a GitHub owner: a namespace is owner/repo, or an owner", ns)
	}
	return scope{owner: ns}, nil
}

// collectionIn finds a collection by name in a scope.
func collectionIn(s scope, name string) *collection {
	if s.ownerWide() {
		return ownerCollectionByName(name)
	}
	return collectionByName(name)
}

// scopeCollectionNames lists the collections of a scope.
func scopeCollectionNames(s scope) []string {
	if s.ownerWide() {
		return ownerCollectionNames()
	}
	return collectionNames()
}

// collectionRef resolves ref to its collection and scope, checking that
// both exist.
func (c *conn) collectionRef(ctx context.Context, op string, ref driver.CollectionRef) (*collection, scope, error) {
	s, err := c.resolveScope(ctx, op, ref.Namespace)
	if err != nil {
		return nil, scope{}, err
	}
	col := collectionIn(s, ref.Name)
	if col == nil {
		return nil, scope{}, notFoundErr(op, "collection %q does not exist in %s (the collections are %s)", ref.Name, s.name(), strings.Join(scopeCollectionNames(s), ", "))
	}
	if err := c.checkScope(ctx, op, s); err != nil {
		return nil, scope{}, err
	}
	return col, s, nil
}

// checkScope checks that a scope's repository or owner exists.
func (c *conn) checkScope(ctx context.Context, op string, s scope) error {
	if s.ownerWide() {
		_, err := c.repos.owner(ctx, op, s.owner)
		return err
	}
	_, err := c.repos.get(ctx, op, s.owner, s.repo)
	return err
}

// ownerQualifier is the search qualifier selecting an owner's
// repositories: org: for an organization, user: for a user.
func (c *conn) ownerQualifier(ctx context.Context, op, owner string) (string, error) {
	info, err := c.repos.owner(ctx, op, owner)
	if err != nil {
		return "", err
	}
	if info.org {
		return "org:" + info.login, nil
	}
	return "user:" + info.login, nil
}

// Namespaces lists repositories, by full name: the DSN's one, the DSN
// owner's, or the token's own (with the ones it collaborates on and its
// organizations'), at most maxRepos.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if c.dsn.repo != "" {
		info, err := c.repos.get(ctx, op, c.dsn.owner, c.dsn.repo)
		if err != nil {
			return nil, err
		}
		return []driver.Namespace{{Name: info.fullName, Label: "repository", Default: true, Comment: info.description}}, nil
	}
	path, query, err := c.repoListing(ctx, op)
	if err != nil {
		return nil, err
	}
	var out []driver.Namespace
	ownerNS, err := c.ownerNamespace(ctx, op)
	if err != nil {
		return nil, err
	}
	for page := 1; len(out) < maxRepos; page++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("per_page", strconv.Itoa(maxPerPage))
		q.Set("page", strconv.Itoa(page))
		items, next, err := c.client.page(ctx, path, q, "")
		if err != nil {
			return nil, c.fail(ctx, op, err)
		}
		for _, it := range items {
			obj, err := objectOf(it)
			if err != nil {
				return nil, c.fail(ctx, op, err)
			}
			full, _ := member(obj, "full_name")
			var name, desc string
			_ = json.Unmarshal(full, &name)
			if d, ok := member(obj, "description"); ok {
				_ = json.Unmarshal(d, &desc)
			}
			if name != "" {
				out = append(out, driver.Namespace{Name: name, Label: "repository", Comment: desc})
			}
		}
		if !next {
			break
		}
	}
	slices.SortFunc(out, func(a, b driver.Namespace) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return append([]driver.Namespace{ownerNS}, out[:min(len(out), maxRepos)]...), nil
}

// ownerNamespace is the owner-wide namespace of a connection without a
// pinned repository: the DSN's owner, or the token's own login.
func (c *conn) ownerNamespace(ctx context.Context, op string) (driver.Namespace, error) {
	login := c.dsn.owner
	if login == "" {
		viewer, err := c.repos.viewer(ctx, op)
		if err != nil {
			return driver.Namespace{}, err
		}
		login = viewer
	}
	info, err := c.repos.owner(ctx, op, login)
	if err != nil {
		return driver.Namespace{}, err
	}
	kind := "user"
	if info.org {
		kind = "organization"
	}
	return driver.Namespace{Name: info.login, Label: "owner", Default: true, Comment: "every repository of the " + kind}, nil
}

// repoListing picks the endpoint that lists the connection's repositories.
func (c *conn) repoListing(ctx context.Context, op string) (string, url.Values, error) {
	return c.ownerListing(ctx, op, c.dsn.owner)
}

// ownerListing picks the endpoint that lists an owner's repositories: the
// token's own (with the ones it collaborates on and its organizations')
// when owner is empty, else the user's or the organization's.
func (c *conn) ownerListing(ctx context.Context, op, owner string) (string, url.Values, error) {
	viewer, err := c.repos.viewer(ctx, op)
	if err != nil {
		return "", nil, err
	}
	switch {
	case owner == "" && viewer == "":
		return "", nil, driver.Wrap(driver.ErrAuth, Name, op, errAnonymousListing)
	case owner == "":
		return "/user/repos", url.Values{"affiliation": {"owner,collaborator,organization_member"}, "sort": {"full_name"}}, nil
	case strings.EqualFold(owner, viewer):
		return "/user/repos", url.Values{"affiliation": {"owner"}, "sort": {"full_name"}}, nil
	}
	info, err := c.repos.owner(ctx, op, owner)
	if err != nil {
		return "", nil, err
	}
	if info.org {
		return "/orgs/" + url.PathEscape(info.login) + "/repos", url.Values{"type": {"all"}, "sort": {"full_name"}}, nil
	}
	return "/users/" + url.PathEscape(info.login) + "/repos", url.Values{"type": {"owner"}, "sort": {"full_name"}}, nil
}

var errAnonymousListing = errors.New("listing your repositories needs a token: give one, or name an owner or a repository in the DSN (github://owner/repo)")

// Collections lists the resource kinds of a repository, in a fixed order.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	s, err := c.resolveScope(ctx, op, ns.Name)
	if err != nil {
		return nil, err
	}
	if s.ownerWide() {
		info, err := c.repos.owner(ctx, op, s.owner)
		if err != nil {
			return nil, err
		}
		out := make([]driver.Collection, len(ownerCollections))
		for i, col := range ownerCollections {
			out[i] = driver.Collection{Ref: driver.CollectionRef{Namespace: info.login, Name: col.name}, Type: driver.CollectionTable, RowEstimate: -1, Comment: col.comment}
		}
		return out, nil
	}
	info, err := c.repos.get(ctx, op, s.owner, s.repo)
	if err != nil {
		return nil, err
	}
	out := make([]driver.Collection, len(collections))
	for i, col := range collections {
		est := int64(-1)
		if col.name == "issues" {
			est = info.openIssues
		}
		out[i] = driver.Collection{Ref: driver.CollectionRef{Namespace: info.fullName, Name: col.name}, Type: driver.CollectionTable, RowEstimate: est, Comment: col.comment}
	}
	return out, nil
}

// Describe returns the fixed columns of a collection. Writable columns are
// plain; the ones GitHub computes are Generated, and the key GitHub assigns
// is AutoIncrement.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	col, s, err := c.collectionRef(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	est := int64(-1)
	var ns string
	if s.ownerWide() {
		info, err := c.repos.owner(ctx, op, s.owner)
		if err != nil {
			return nil, err
		}
		ns = info.login
	} else {
		info, err := c.repos.get(ctx, op, s.owner, s.repo)
		if err != nil {
			return nil, err
		}
		ns = info.fullName
		if col.name == "issues" {
			est = info.openIssues
		}
	}
	comment := col.comment
	if col.insert == nil && col.update == nil && col.delete == nil && (col.search == nil || col.search.peer == "") {
		comment += "; read-only"
	}
	return &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: ns, Name: col.name},
		Type:        driver.CollectionTable,
		Fields:      col.fields(),
		PrimaryKey:  col.primaryKey(),
		RowEstimate: est,
		SizeBytes:   -1,
		Comment:     comment,
	}, nil
}
