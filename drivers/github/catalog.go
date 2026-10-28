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
	c     *conn
	mu    sync.Mutex
	repos map[string]*repoInfo // by lower-cased owner/repo
	login string
	who   bool // login was looked up
}

func newRepoCache(c *conn) *repoCache { return &repoCache{c: c, repos: map[string]*repoInfo{}} }

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

// resolveRepo names the repository a call works on: ns when given, else
// the DSN's. Without either the call cannot run.
func (c *conn) resolveRepo(op, ns string) (owner, repo string, err error) {
	if ns == "" {
		ns = c.dsn.repoName()
	}
	if ns == "" {
		return "", "", syntaxErr(op, `name the repository: a namespace "owner/repo", "repo" in the query, or github://owner/repo in the DSN`)
	}
	owner, repo, ok := splitRepo(ns)
	if !ok {
		return "", "", notFoundErr(op, "%q is not a repository: a namespace is owner/repo", ns)
	}
	return owner, repo, nil
}

// collectionRef resolves ref to its collection and repository, checking
// that both exist.
func (c *conn) collectionRef(ctx context.Context, op string, ref driver.CollectionRef) (*collection, string, string, error) {
	col := collectionByName(ref.Name)
	if col == nil {
		return nil, "", "", notFoundErr(op, "collection %q does not exist (the collections are %s)", ref.Name, strings.Join(collectionNames(), ", "))
	}
	owner, repo, err := c.resolveRepo(op, ref.Namespace)
	if err != nil {
		return nil, "", "", err
	}
	if _, err := c.repos.get(ctx, op, owner, repo); err != nil {
		return nil, "", "", err
	}
	return col, owner, repo, nil
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
	return out[:min(len(out), maxRepos)], nil
}

// repoListing picks the endpoint that lists the connection's repositories.
func (c *conn) repoListing(ctx context.Context, op string) (string, url.Values, error) {
	viewer, err := c.repos.viewer(ctx, op)
	if err != nil {
		return "", nil, err
	}
	switch {
	case c.dsn.owner == "" && viewer == "":
		return "", nil, driver.Wrap(driver.ErrAuth, Name, op, errAnonymousListing)
	case c.dsn.owner == "":
		return "/user/repos", url.Values{"affiliation": {"owner,collaborator,organization_member"}, "sort": {"full_name"}}, nil
	case strings.EqualFold(c.dsn.owner, viewer):
		return "/user/repos", url.Values{"affiliation": {"owner"}, "sort": {"full_name"}}, nil
	}
	raw, err := c.client.getJSON(ctx, "/users/"+url.PathEscape(c.dsn.owner), nil)
	if err != nil {
		return "", nil, c.fail(ctx, op, err)
	}
	var who struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &who)
	if who.Type == "Organization" {
		return "/orgs/" + url.PathEscape(c.dsn.owner) + "/repos", url.Values{"type": {"all"}, "sort": {"full_name"}}, nil
	}
	return "/users/" + url.PathEscape(c.dsn.owner) + "/repos", url.Values{"type": {"owner"}, "sort": {"full_name"}}, nil
}

var errAnonymousListing = errors.New("listing your repositories needs a token: give one, or name an owner or a repository in the DSN (github://owner/repo)")

// Collections lists the resource kinds of a repository, in a fixed order.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	owner, repo, err := c.resolveRepo(op, ns.Name)
	if err != nil {
		return nil, err
	}
	info, err := c.repos.get(ctx, op, owner, repo)
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
	col, owner, repo, err := c.collectionRef(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	info, err := c.repos.get(ctx, op, owner, repo)
	if err != nil {
		return nil, err
	}
	est := int64(-1)
	if col.name == "issues" {
		est = info.openIssues
	}
	comment := col.comment
	if col.insert == nil && col.update == nil && col.delete == nil {
		comment += "; read-only"
	}
	return &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: info.fullName, Name: col.name},
		Type:        driver.CollectionTable,
		Fields:      col.fields(),
		PrimaryKey:  []string{col.keyCol},
		RowEstimate: est,
		SizeBytes:   -1,
		Comment:     comment,
	}, nil
}
