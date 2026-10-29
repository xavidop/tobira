package github

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// fakeGitHub is a container-free, in-memory stand-in for the GitHub REST
// API (and its GraphQL endpoint) on httptest. It serves the endpoints the
// driver uses with GitHub's JSON shapes, honours per_page/page with a Link
// header, applies the list filters the driver pushes down, checks the
// bearer token, sends rate-limit headers and ETags (answering 304 to a
// matching If-None-Match), and records every request. It encodes our
// reading of the API documentation; it has not been checked against
// github.com.
type fakeGitHub struct {
	t     testing.TB
	srv   *httptest.Server
	token string

	mu     sync.Mutex
	owners map[string]string // login → "User" or "Organization"
	repos  []*fakeRepo
	// collaborations are repositories the token's user collaborates on,
	// listed by /user/repos with affiliation=collaborator.
	collaborations map[string]bool
	// rateLimit and rateRemaining feed the X-RateLimit headers of every
	// answer with a token (anonymous answers report 60).
	rateLimit, rateRemaining int
	requests                 []fakeRequest
	mutations                []string // GraphQL mutations received
	// hook, when set, may answer a request itself (return true).
	hook func(w http.ResponseWriter, r *http.Request, body []byte) bool
}

// fakeRequest records one request the fake received and the status it
// answered.
type fakeRequest struct {
	Method string
	Path   string
	Query  url.Values
	Auth   string
	Header http.Header
	Body   []byte
	Status int
}

// object is one JSON object of the fake's data, in the API's own shape.
type object = map[string]any

// fakeRepo is one repository with its collections.
type fakeRepo struct {
	id            int
	owner, name   string
	private       bool
	description   string
	language      string
	defaultBranch string
	stars, forks  int
	topics        []string

	issues     []object // issues and pull requests, as the issues endpoint lists them, by number
	pulls      []object
	comments   []object
	commits    []object // newest first
	branches   []object
	tags       []object // newest first
	releases   []object // newest first
	labels     []object
	milestones []object
	runs       []object // newest first
	nextNumber int
	nextID     int
}

const (
	fakeToken  = "ghp_test_token_0123456789"
	fakeViewer = "octo"
	// fakeRateReset is the fixed X-RateLimit-Reset epoch second.
	fakeRateReset = "1704070800"
	// editedAt is the updated_at every write stamps.
	editedAt = "2024-12-31T00:00:00Z"
)

// seedBase is the instant the seeded timestamps count from.
var seedBase = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// at renders the fixed timestamp n hours after seedBase.
func at(n int) string { return seedBase.Add(time.Duration(n) * time.Hour).Format(time.RFC3339) }

// atMin renders the fixed timestamp n minutes after seedBase.
func atMin(n int) string {
	return seedBase.Add(time.Duration(n) * time.Minute).Format(time.RFC3339)
}

// shaOf is a deterministic 40-hex commit sha for a name.
func shaOf(name string) string {
	sum := sha1.Sum([]byte(name))
	return hex.EncodeToString(sum[:])
}

// newFakeGitHub starts a fake seeded with the user octo (the token's
// owner, repositories widgets and gadgets), the user hubot (scripts, with
// octo as a collaborator) and the organization acme (alpha and beta).
func newFakeGitHub(t testing.TB) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t:              t,
		token:          fakeToken,
		owners:         map[string]string{"octo": "User", "hubot": "User", "acme": "Organization"},
		collaborations: map[string]bool{"hubot/scripts": true},
		rateLimit:      5000,
		rateRemaining:  4987,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.seed()
	return f
}

// base is the REST endpoint the driver is pointed at.
func (f *fakeGitHub) base() string { return f.srv.URL }

// dsn builds a DSN for an owner and repository; either may be "".
func (f *fakeGitHub) dsn(owner, repo string) string {
	d := "github://" + owner
	if repo != "" {
		d += "/" + repo
	}
	return d + "?endpoint=" + url.QueryEscape(f.base())
}

// cfg is a ConnConfig for octo/widgets with the given token ("" = none).
func (f *fakeGitHub) cfg(readOnly bool, token string) driver.ConnConfig {
	cfg := driver.ConnConfig{Name: "gh", DSN: f.dsn("octo", "widgets"), ReadOnly: readOnly}
	if token != "" {
		cfg.Password = driver.NewSecret(token)
	}
	return cfg
}

// open opens a connection to octo/widgets with the right token.
func (f *fakeGitHub) open(t testing.TB, readOnly bool) *conn {
	t.Helper()
	return f.openDSN(t, f.dsn("octo", "widgets"), readOnly, fakeToken)
}

// openDSN opens a connection with any DSN and token.
func (f *fakeGitHub) openDSN(t testing.TB, dsn string, readOnly bool, token string) *conn {
	t.Helper()
	cfg := driver.ConnConfig{Name: "gh", DSN: dsn, ReadOnly: readOnly}
	if token != "" {
		cfg.Password = driver.NewSecret(token)
	}
	c, err := New().Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// log is a copy of the requests received so far.
func (f *fakeGitHub) log() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

// requestsTo lists the requests to one path.
func (f *fakeGitHub) requestsTo(path string) []fakeRequest {
	var out []fakeRequest
	for _, r := range f.log() {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// writes lists the REST requests that are not GETs. GraphQL reads are
// POSTs too, so /graphql is left out; mutationLog has the mutations.
func (f *fakeGitHub) writes() []fakeRequest {
	var out []fakeRequest
	for _, r := range f.log() {
		if r.Method != http.MethodGet && r.Path != "/graphql" {
			out = append(out, r)
		}
	}
	return out
}

// resetLog forgets the requests so far.
func (f *fakeGitHub) resetLog() {
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
}

func (f *fakeGitHub) setHook(h func(w http.ResponseWriter, r *http.Request, body []byte) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = h
}

// setRate changes what the rate-limit headers report.
func (f *fakeGitHub) setRate(limit, remaining int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateLimit, f.rateRemaining = limit, remaining
}

// mutationLog lists the GraphQL mutations received.
func (f *fakeGitHub) mutationLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mutations...)
}

// find returns a copy of the first item of a repository's collection
// ("issues", "labels", …) matching match, or nil.
func (f *fakeGitHub) find(owner, repo, kind string, match func(object) bool) object {
	f.mu.Lock()
	defer f.mu.Unlock()
	rp := f.findRepo(owner, repo)
	if rp == nil {
		return nil
	}
	for _, it := range rp.collection(kind) {
		if match(it) {
			out := object{}
			for k, v := range it {
				out[k] = v
			}
			return out
		}
	}
	return nil
}

// count is the size of a repository's collection.
func (f *fakeGitHub) count(owner, repo, kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	rp := f.findRepo(owner, repo)
	if rp == nil {
		return 0
	}
	return len(rp.collection(kind))
}

// collection names a repository's list by the driver's collection name.
func (rp *fakeRepo) collection(kind string) []object {
	switch kind {
	case "issues":
		return rp.issues
	case "pull_requests":
		return rp.pulls
	case "issue_comments":
		return rp.comments
	case "commits":
		return rp.commits
	case "branches":
		return rp.branches
	case "tags":
		return rp.tags
	case "releases":
		return rp.releases
	case "labels":
		return rp.labels
	case "milestones":
		return rp.milestones
	case "workflow_runs":
		return rp.runs
	}
	return nil
}

// Seeding.

func (f *fakeGitHub) seed() {
	widgets := f.addRepo("octo", "widgets", "Widgets for everyone", "Go", false)
	widgets.stars, widgets.forks, widgets.topics = 42, 7, []string{"widgets", "go"}
	f.seedWidgets(widgets)
	gadgets := f.addRepo("octo", "gadgets", "Gadgets, the sequel", "Rust", false)
	f.seedSmall(gadgets, 3)
	scripts := f.addRepo("hubot", "scripts", "Hubot's scripts", "JavaScript", false)
	f.seedSmall(scripts, 2)
	alpha := f.addRepo("acme", "alpha", "Acme alpha", "Go", false)
	f.seedSmall(alpha, 4)
	beta := f.addRepo("acme", "beta", "Acme beta", "Python", true)
	f.seedSmall(beta, 1)
}

func (f *fakeGitHub) addRepo(owner, name, description, language string, private bool) *fakeRepo {
	rp := &fakeRepo{id: 1000 + len(f.repos), owner: owner, name: name, description: description, language: language,
		private: private, defaultBranch: "main", topics: []string{}, nextNumber: 1, nextID: 1}
	f.repos = append(f.repos, rp)
	return rp
}

// seedWidgets fills octo/widgets: 250 issues and 20 pull requests (so the
// issues endpoint lists 270 items over three pages of 100), 40 comments,
// 60 commits, 5 branches, 4 tags, 3 releases, 7 labels, 3 milestones and
// 30 workflow runs.
func (f *fakeGitHub) seedWidgets(rp *fakeRepo) {
	for _, l := range []struct{ name, color, desc string }{
		{"bug", "d73a4a", "Something isn't working"}, {"enhancement", "a2eeef", "New feature or request"},
		{"documentation", "0075ca", "Improvements or additions to documentation"}, {"good first issue", "7057ff", "Good for newcomers"},
		{"question", "d876e3", "Further information is requested"}, {"wontfix", "ffffff", "This will not be worked on"},
		{"duplicate", "cfd3d7", "This issue or pull request already exists"},
	} {
		f.addLabel(rp, l.name, l.color, l.desc, true)
	}
	f.addMilestone(rp, "v1.0", "The first release", "open", at(24*150))
	f.addMilestone(rp, "v2.0", "The next one", "open", at(24*300))
	m := f.addMilestone(rp, "v0.9", "The beta", "closed", at(24*10))
	m["closed_at"] = at(24 * 11)

	for i := 1; i <= 250; i++ {
		user := "hubot"
		if i%2 == 0 {
			user = "octo"
		}
		var labels []string
		if i%2 == 0 {
			labels = append(labels, "bug")
		}
		if i%5 == 0 {
			labels = append(labels, "enhancement")
		}
		if i%7 == 0 {
			labels = append(labels, "documentation")
		}
		var assignees []string
		switch i % 4 {
		case 0:
			assignees = []string{"octo"}
		case 2:
			assignees = []string{"hubot", "octo"}
		}
		var milestone object
		switch i % 3 {
		case 1:
			milestone = rp.milestones[0]
		case 2:
			milestone = rp.milestones[1]
		}
		title := fmt.Sprintf("Issue %d", i)
		if i%10 == 0 {
			title = fmt.Sprintf("Issue %d: widget crashes on start", i)
		}
		is := f.newIssue(rp, user, title, fmt.Sprintf("Body of issue %d.", i), labels, assignees, milestone, at(i))
		is["comments"] = i % 6
		is["updated_at"] = atMin(i*60 + 30)
		if i%3 == 0 {
			is["state"], is["state_reason"], is["closed_at"] = "closed", "completed", atMin(i*60+30)
		}
	}
	for j := 1; j <= 20; j++ {
		user := "octo"
		if j%3 == 0 {
			user = "hubot"
		}
		pr := f.newPull(rp, user, fmt.Sprintf("PR %d: improve the widget", j), fmt.Sprintf("Body of PR %d.", j),
			fmt.Sprintf("feature/%d", j), "main", j%5 == 0, at(300+j))
		if j%4 == 0 {
			pr["labels"] = f.labelObjects(rp, []string{"enhancement"})
		}
		if j%2 == 0 {
			pr["state"], pr["closed_at"] = "closed", at(400+j)
			pr["merged_at"], pr["merge_commit_sha"] = at(400+j), shaOf(fmt.Sprintf("merge %d", j))
			f.syncPullIssue(rp, pr)
		}
	}
	for c := 1; c <= 40; c++ {
		user := "hubot"
		if c%3 == 0 {
			user = "octo"
		}
		f.addComment(rp, c%10+1, user, fmt.Sprintf("Comment %d", c), at(500+c))
	}
	prev := ""
	var shas []string
	for i := 0; i < 60; i++ {
		sha := shaOf(fmt.Sprintf("octo/widgets commit %d", i))
		msg := fmt.Sprintf("Commit %d: add a widget", i)
		if i%4 == 0 {
			msg = fmt.Sprintf("Commit %d: fix a typo", i)
		}
		var author any = userObj("octo")
		if i%3 == 0 {
			author = nil
		}
		var parents []object
		if prev != "" {
			parents = append(parents, object{"sha": prev, "url": f.base() + "/repos/octo/widgets/commits/" + prev, "html_url": "https://github.com/octo/widgets/commit/" + prev})
		} else {
			parents = []object{}
		}
		d := at(2 * i)
		rp.commits = append([]object{{
			"sha": sha, "node_id": "C_" + sha[:8],
			"commit": object{
				"message":   msg,
				"author":    object{"name": "Octo Cat", "email": "octo@example.com", "date": d},
				"committer": object{"name": "GitHub", "email": "noreply@github.com", "date": d},
				"tree":      object{"sha": shaOf("tree " + sha)}, "comment_count": 0,
			},
			"author": author, "committer": userObj("web-flow"), "parents": parents,
			"html_url": "https://github.com/octo/widgets/commit/" + sha, "url": f.base() + "/repos/octo/widgets/commits/" + sha,
		}}, rp.commits...)
		shas = append(shas, sha)
		prev = sha
	}
	head := shas[len(shas)-1]
	for _, b := range []struct {
		name      string
		sha       string
		protected bool
	}{
		{"develop", shas[50], false}, {"feature/1", shas[40], false}, {"feature/2", shas[45], false}, {"main", head, true}, {"release/1.x", shas[30], false},
	} {
		rp.branches = append(rp.branches, object{"name": b.name, "commit": object{"sha": b.sha, "url": f.base() + "/repos/octo/widgets/commits/" + b.sha}, "protected": b.protected})
	}
	for i, tag := range []string{"v0.9.0", "v1.0.0", "v1.1.0", "v2.0.0-rc1"} {
		sha := shas[10*i+5]
		rp.tags = append([]object{{
			"name": tag, "commit": object{"sha": sha, "url": f.base() + "/repos/octo/widgets/commits/" + sha},
			"zipball_url": f.base() + "/repos/octo/widgets/zipball/refs/tags/" + tag, "tarball_url": f.base() + "/repos/octo/widgets/tarball/refs/tags/" + tag, "node_id": "T_" + tag,
		}}, rp.tags...)
	}
	for i, tag := range []string{"v0.9.0", "v1.0.0", "v1.1.0"} {
		rel := f.addRelease(rp, tag, "main", "Widgets "+tag, false, i == 0, "Release notes for "+tag, at(600+24*i))
		if i == 1 {
			rel["assets"] = []object{{"id": 77001, "name": "widgets-linux-amd64.tar.gz", "size": 1024, "content_type": "application/gzip", "download_count": 3, "browser_download_url": "https://github.com/octo/widgets/releases/download/" + tag + "/widgets-linux-amd64.tar.gz"}}
		}
	}
	for i := 1; i <= 30; i++ {
		name, event, branch := "CI", "push", "main"
		if i%3 == 0 {
			event, branch = "pull_request", fmt.Sprintf("feature/%d", i%20+1)
		}
		if i%5 == 0 {
			name = "Release"
		}
		status, conclusion := "completed", any("success")
		switch {
		case i%7 == 0:
			conclusion = "failure"
		case i%11 == 0:
			status, conclusion = "in_progress", nil
		}
		actor := "octo"
		if i%2 == 0 {
			actor = "hubot"
		}
		workflowID := 101
		if name == "Release" {
			workflowID = 102
		}
		rp.runs = append([]object{{
			"id": 9000 + i, "name": name, "display_title": fmt.Sprintf("Run %d of %s", i, name), "run_number": i, "run_attempt": 1,
			"event": event, "status": status, "conclusion": conclusion, "workflow_id": workflowID, "head_branch": branch, "head_sha": shas[i%len(shas)],
			"actor": userObj(actor), "triggering_actor": userObj(actor), "path": ".github/workflows/" + strings.ToLower(name) + ".yml",
			"created_at": at(700 + 3*i), "updated_at": at(700 + 3*i + 1), "run_started_at": at(700 + 3*i),
			"html_url": fmt.Sprintf("https://github.com/octo/widgets/actions/runs/%d", 9000+i), "url": fmt.Sprintf("%s/repos/octo/widgets/actions/runs/%d", f.base(), 9000+i),
		}}, rp.runs...)
	}
}

// seedSmall fills a repository with n open issues, one label and one
// branch.
func (f *fakeGitHub) seedSmall(rp *fakeRepo, n int) {
	f.addLabel(rp, "bug", "d73a4a", "Something isn't working", true)
	for i := 1; i <= n; i++ {
		f.newIssue(rp, rp.owner, fmt.Sprintf("%s issue %d", rp.name, i), "", []string{"bug"}, nil, nil, at(i))
	}
	sha := shaOf(rp.owner + "/" + rp.name + " commit 0")
	rp.branches = append(rp.branches, object{"name": "main", "commit": object{"sha": sha}, "protected": false})
	rp.commits = append(rp.commits, object{"sha": sha, "commit": object{"message": "Initial commit", "author": object{"name": rp.owner, "email": rp.owner + "@example.com", "date": at(0)}, "committer": object{"name": rp.owner, "email": rp.owner + "@example.com", "date": at(0)}}, "author": userObj(rp.owner), "committer": userObj(rp.owner), "parents": []object{}, "html_url": "https://github.com/" + rp.owner + "/" + rp.name + "/commit/" + sha})
}

func userObj(login string) object {
	typ := "User"
	if login == "acme" {
		typ = "Organization"
	}
	id := int(fnvOf(login) % 100000)
	return object{"login": login, "id": id, "node_id": "U_" + login, "type": typ, "site_admin": false,
		"html_url": "https://github.com/" + login, "avatar_url": "https://avatars.example.com/" + login}
}

func userObjs(logins []string) []object {
	out := make([]object, 0, len(logins))
	for _, l := range logins {
		out = append(out, userObj(l))
	}
	return out
}

func fnvOf(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func (rp *fakeRepo) full() string { return rp.owner + "/" + rp.name }

func (rp *fakeRepo) takeID() int {
	rp.nextID++
	return rp.id*10000 + rp.nextID
}

func (rp *fakeRepo) takeNumber() int {
	n := rp.nextNumber
	rp.nextNumber++
	return n
}

// openIssues counts open issues and pull requests, as GitHub's
// open_issues_count does.
func (rp *fakeRepo) openIssues() int {
	n := 0
	for _, is := range rp.issues {
		if str(is, "state") == "open" {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) repoObject(rp *fakeRepo) object {
	return object{
		"id": rp.id, "node_id": "R_" + rp.name, "name": rp.name, "full_name": rp.full(), "owner": userObj(rp.owner), "private": rp.private,
		"html_url": "https://github.com/" + rp.full(), "description": rp.description, "fork": false, "archived": false, "disabled": false,
		"url": f.base() + "/repos/" + rp.full(), "language": rp.language, "default_branch": rp.defaultBranch,
		"stargazers_count": rp.stars, "watchers_count": rp.stars, "forks_count": rp.forks, "open_issues_count": rp.openIssues(),
		"topics": rp.topics, "visibility": map[bool]string{true: "private", false: "public"}[rp.private],
		"pushed_at": at(500), "created_at": at(0), "updated_at": at(500),
	}
}

func (f *fakeGitHub) addLabel(rp *fakeRepo, name, color, desc string, isDefault bool) object {
	var d any = desc
	if desc == "" {
		d = nil
	}
	l := object{"id": rp.takeID(), "node_id": "L_" + name, "name": name, "color": color, "description": d, "default": isDefault,
		"url": f.base() + "/repos/" + rp.full() + "/labels/" + url.PathEscape(name)}
	rp.labels = append(rp.labels, l)
	return l
}

// labelObjects resolves label names, creating the unknown ones as GitHub
// does when an issue names a label that does not exist.
func (f *fakeGitHub) labelObjects(rp *fakeRepo, names []string) []object {
	out := make([]object, 0, len(names))
	for _, n := range names {
		var found object
		for _, l := range rp.labels {
			if str(l, "name") == n {
				found = l
				break
			}
		}
		if found == nil {
			found = f.addLabel(rp, n, "ededed", "", false)
		}
		out = append(out, found)
	}
	return out
}

func (f *fakeGitHub) addMilestone(rp *fakeRepo, title, desc, state, dueOn string) object {
	n := len(rp.milestones) + 1
	var due any = dueOn
	if dueOn == "" {
		due = nil
	}
	m := object{"id": rp.takeID(), "node_id": fmt.Sprintf("M_%d", n), "number": n, "title": title, "state": state, "description": desc,
		"creator": userObj("octo"), "open_issues": 0, "closed_issues": 0, "due_on": due, "created_at": at(0), "updated_at": at(1), "closed_at": nil,
		"html_url": fmt.Sprintf("https://github.com/%s/milestone/%d", rp.full(), n), "url": fmt.Sprintf("%s/repos/%s/milestones/%d", f.base(), rp.full(), n)}
	rp.milestones = append(rp.milestones, m)
	return m
}

func (f *fakeGitHub) newIssue(rp *fakeRepo, user, title, body string, labels, assignees []string, milestone object, created string) object {
	n := rp.takeNumber()
	var assignee any
	if len(assignees) > 0 {
		assignee = userObj(assignees[0])
	}
	var ms any
	if milestone != nil {
		ms = milestone
	}
	var b any = body
	if body == "" {
		b = nil
	}
	is := object{
		"id": rp.takeID(), "node_id": fmt.Sprintf("I_%d", n), "number": n, "title": title, "state": "open", "state_reason": nil,
		"user": userObj(user), "labels": f.labelObjects(rp, labels), "assignee": assignee, "assignees": userObjs(assignees),
		"milestone": ms, "comments": 0, "locked": false, "author_association": association(rp, user),
		"created_at": created, "updated_at": created, "closed_at": nil, "body": b,
		"html_url": fmt.Sprintf("https://github.com/%s/issues/%d", rp.full(), n), "url": fmt.Sprintf("%s/repos/%s/issues/%d", f.base(), rp.full(), n),
		"repository_url": f.base() + "/repos/" + rp.full(),
	}
	rp.issues = append(rp.issues, is)
	return is
}

func association(rp *fakeRepo, user string) string {
	if user == rp.owner {
		return "OWNER"
	}
	return "CONTRIBUTOR"
}

func (f *fakeGitHub) newPull(rp *fakeRepo, user, title, body, head, base string, draft bool, created string) object {
	n := rp.takeNumber()
	pr := object{
		"id": rp.takeID(), "node_id": fmt.Sprintf("PR_%d", n), "number": n, "title": title, "state": "open", "draft": draft, "locked": false,
		"user": userObj(user), "head": object{"ref": head, "sha": shaOf(head), "label": rp.owner + ":" + head}, "base": object{"ref": base, "sha": shaOf(base), "label": rp.owner + ":" + base},
		"labels": []object{}, "assignees": []object{}, "assignee": nil, "requested_reviewers": []object{userObj("hubot")}, "milestone": nil,
		"merged_at": nil, "merge_commit_sha": nil, "author_association": association(rp, user),
		"created_at": created, "updated_at": created, "closed_at": nil, "body": body,
		"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", rp.full(), n), "url": fmt.Sprintf("%s/repos/%s/pulls/%d", f.base(), rp.full(), n),
	}
	rp.pulls = append(rp.pulls, pr)
	rp.issues = append(rp.issues, f.issueOfPull(rp, pr))
	return pr
}

// issueOfPull is the issues-endpoint view of a pull request.
func (f *fakeGitHub) issueOfPull(rp *fakeRepo, pr object) object {
	n := num(pr, "number")
	return object{
		"id": pr["id"], "node_id": pr["node_id"], "number": n, "title": pr["title"], "state": pr["state"], "state_reason": nil,
		"user": pr["user"], "labels": pr["labels"], "assignee": pr["assignee"], "assignees": pr["assignees"], "milestone": pr["milestone"],
		"comments": 0, "locked": false, "author_association": pr["author_association"],
		"created_at": pr["created_at"], "updated_at": pr["updated_at"], "closed_at": pr["closed_at"], "body": pr["body"],
		"html_url": pr["html_url"], "url": fmt.Sprintf("%s/repos/%s/issues/%d", f.base(), rp.full(), n), "repository_url": f.base() + "/repos/" + rp.full(),
		"pull_request": object{"url": pr["url"], "html_url": pr["html_url"], "diff_url": str(pr, "html_url") + ".diff", "patch_url": str(pr, "html_url") + ".patch", "merged_at": pr["merged_at"]},
	}
}

// syncPullIssue refreshes the issues-endpoint view of a pull request.
func (f *fakeGitHub) syncPullIssue(rp *fakeRepo, pr object) {
	for i, is := range rp.issues {
		if num(is, "number") == num(pr, "number") {
			rp.issues[i] = f.issueOfPull(rp, pr)
			return
		}
	}
}

func (f *fakeGitHub) addComment(rp *fakeRepo, issue int, user, body, created string) object {
	id := rp.takeID()
	c := object{
		"id": id, "node_id": fmt.Sprintf("IC_%d", id), "issue_url": fmt.Sprintf("%s/repos/%s/issues/%d", f.base(), rp.full(), issue),
		"user": userObj(user), "body": body, "author_association": association(rp, user), "created_at": created, "updated_at": created,
		"html_url": fmt.Sprintf("https://github.com/%s/issues/%d#issuecomment-%d", rp.full(), issue, id), "url": fmt.Sprintf("%s/repos/%s/issues/comments/%d", f.base(), rp.full(), id),
	}
	rp.comments = append(rp.comments, c)
	for _, is := range rp.issues {
		if num(is, "number") == int64(issue) {
			is["comments"] = int(num(is, "comments")) + 1
		}
	}
	return c
}

func (f *fakeGitHub) addRelease(rp *fakeRepo, tag, target, name string, draft, prerelease bool, body, created string) object {
	id := rp.takeID()
	var published any = created
	if draft {
		published = nil
	}
	rel := object{
		"id": id, "node_id": fmt.Sprintf("RE_%d", id), "tag_name": tag, "target_commitish": target, "name": name, "draft": draft, "prerelease": prerelease,
		"author": userObj("octo"), "assets": []object{}, "created_at": created, "published_at": published, "body": body,
		"html_url": fmt.Sprintf("https://github.com/%s/releases/tag/%s", rp.full(), tag), "url": fmt.Sprintf("%s/repos/%s/releases/%d", f.base(), rp.full(), id),
		"upload_url":  fmt.Sprintf("%s/repos/%s/releases/%d/assets{?name,label}", f.base(), rp.full(), id),
		"tarball_url": fmt.Sprintf("%s/repos/%s/tarball/%s", f.base(), rp.full(), tag), "zipball_url": fmt.Sprintf("%s/repos/%s/zipball/%s", f.base(), rp.full(), tag),
	}
	rp.releases = append([]object{rel}, rp.releases...)
	return rel
}

// Readers of the fake's objects.

func str(o object, k string) string {
	s, _ := o[k].(string)
	return s
}

func num(o object, k string) int64 {
	switch v := o[k].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func tm(o object, k string) time.Time {
	t, _ := time.Parse(time.RFC3339, str(o, k))
	return t
}

func sub(o object, k string) object {
	m, _ := o[k].(object)
	return m
}

// names lists a member of each object of an array member (the "name" of
// each label, the "login" of each assignee).
func memberNames(o object, k, field string) []string {
	items, _ := o[k].([]object)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, str(it, field))
	}
	return out
}

// Serving.

// statusWriter records the status written.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	idx := len(f.requests)
	f.requests = append(f.requests, fakeRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Auth: r.Header.Get("Authorization"), Header: r.Header.Clone(), Body: body})
	hook := f.hook
	limit, remaining := f.rateLimit, f.rateRemaining
	f.mu.Unlock()
	sw := &statusWriter{ResponseWriter: w}
	defer func() {
		f.mu.Lock()
		f.requests[idx].Status = sw.status
		f.mu.Unlock()
	}()
	auth := r.Header.Get("Authorization")
	if auth == "" {
		limit, remaining = 60, 59
	}
	resource := "core"
	switch {
	case strings.HasPrefix(r.URL.Path, "/search/"):
		resource = "search"
	case r.URL.Path == "/graphql":
		resource = "graphql"
	}
	h := sw.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", fakeRateReset)
	h.Set("X-RateLimit-Resource", resource)
	h.Set("X-RateLimit-Used", strconv.Itoa(limit-remaining))
	h.Set("X-GitHub-Request-Id", fmt.Sprintf("FAKE:%04d", idx))
	if hook != nil && hook(sw, r, body) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	authed := false
	switch auth {
	case "":
	case "Bearer " + f.token:
		authed = true
	default:
		writeError(sw, http.StatusUnauthorized, "Bad credentials")
		return
	}
	f.route(sw, r, body, authed)
}

// writeJSON answers v, with an ETag on a 200 GET and a 304 when the
// request's If-None-Match already names it.
func (f *fakeGitHub) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	if r.Method == http.MethodGet && status == http.StatusOK {
		etag := fmt.Sprintf(`W/"%x"`, fnvOf(string(body)))
		h.Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError answers GitHub's error shape.
func writeError(w http.ResponseWriter, status int, msg string, errs ...object) {
	body := object{"message": msg, "documentation_url": "https://docs.github.com/rest"}
	if len(errs) > 0 {
		body["errors"] = errs
	}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// validationError is the 422 for a POST or PATCH missing a field.
func validationError(w http.ResponseWriter, resource, field string) {
	writeError(w, http.StatusUnprocessableEntity, "Validation Failed", object{"resource": resource, "field": field, "code": "missing_field"})
}

func notFound(w http.ResponseWriter) { writeError(w, http.StatusNotFound, "Not Found") }

// decodeBody reads a request body as a JSON object; an empty body is an
// empty object.
func decodeBody(w http.ResponseWriter, body []byte) (object, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return object{}, true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var o object
	if err := dec.Decode(&o); err != nil || o == nil {
		writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return nil, false
	}
	return o, true
}

// stringsOf reads a body member that is an array of strings.
func stringsOf(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// page answers one page of items with GitHub's Link header, wrapped in an
// envelope ({"total_count": …, "<envelope>": […]}) when one is named.
func (f *fakeGitHub) page(w http.ResponseWriter, r *http.Request, items []object, envelope string) {
	q := r.URL.Query()
	perPage := atoiDefault(q.Get("per_page"), 30)
	perPage = max(1, min(perPage, 100))
	page := max(1, atoiDefault(q.Get("page"), 1))
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	slice := append([]object{}, items[start:end]...)
	if link := linkHeader(r, page, perPage, len(items)); link != "" {
		w.Header().Set("Link", link)
	}
	if envelope != "" {
		f.writeJSON(w, r, http.StatusOK, object{"total_count": len(items), envelope: slice})
		return
	}
	f.writeJSON(w, r, http.StatusOK, slice)
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// linkHeader builds GitHub's Link header for a page, or "" when the listing
// fits in one page.
func linkHeader(r *http.Request, page, perPage, total int) string {
	last := max(1, (total+perPage-1)/perPage)
	if last == 1 {
		return ""
	}
	link := func(p int, rel string) string {
		u := *r.URL
		q := u.Query()
		q.Set("page", strconv.Itoa(p))
		q.Set("per_page", strconv.Itoa(perPage))
		u.RawQuery = q.Encode()
		return fmt.Sprintf(`<http://%s%s>; rel="%s"`, r.Host, u.String(), rel)
	}
	var parts []string
	if page > 1 {
		parts = append(parts, link(page-1, "prev"), link(1, "first"))
	}
	if page < last {
		parts = append(parts, link(page+1, "next"), link(last, "last"))
	}
	return strings.Join(parts, ", ")
}

// sortItems orders items by a top-level member: times by instant, numbers
// by value, text otherwise.
func sortItems(items []object, field string, desc bool) []object {
	out := slices.Clone(items)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i][field], out[j][field]
		less := false
		switch av := a.(type) {
		case string:
			bv, _ := b.(string)
			ta, errA := time.Parse(time.RFC3339, av)
			tb, errB := time.Parse(time.RFC3339, bv)
			if errA == nil && errB == nil {
				less = ta.Before(tb)
			} else {
				less = av < bv
			}
		case nil:
			less = b != nil
		default:
			less = num(out[i], field) < num(out[j], field)
		}
		if desc {
			return !less && !equalMember(a, b)
		}
		return less
	})
	return out
}

func equalMember(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func (f *fakeGitHub) findRepo(owner, name string) *fakeRepo {
	for _, rp := range f.repos {
		if strings.EqualFold(rp.owner, owner) && strings.EqualFold(rp.name, name) {
			return rp
		}
	}
	return nil
}

// ownerType is the kind of an owner, User or Organization, found without
// regard to case.
func (f *fakeGitHub) ownerType(login string) (string, bool) {
	_, typ, ok := f.ownerLogin(login)
	return typ, ok
}

// ownerLogin finds an owner without regard to case and gives back its
// login as seeded, the spelling GitHub answers with, and its kind.
func (f *fakeGitHub) ownerLogin(login string) (string, string, bool) {
	for l, typ := range f.owners {
		if strings.EqualFold(l, login) {
			return l, typ, true
		}
	}
	return "", "", false
}

func (f *fakeGitHub) route(w http.ResponseWriter, r *http.Request, body []byte, authed bool) {
	var seg []string
	if p := strings.Trim(r.URL.Path, "/"); p != "" {
		seg = strings.Split(p, "/")
	}
	q := r.URL.Query()
	needAuth := func() bool {
		if !authed {
			writeError(w, http.StatusUnauthorized, "Requires authentication")
		}
		return authed
	}
	switch {
	case len(seg) == 0:
		f.writeJSON(w, r, http.StatusOK, object{"current_user_url": f.base() + "/user", "rate_limit_url": f.base() + "/rate_limit", "repository_url": f.base() + "/repos/{owner}/{repo}"})
	case len(seg) == 1 && seg[0] == "rate_limit":
		limit, remaining := 60, 59
		if r.Header.Get("Authorization") != "" {
			limit, remaining = f.rateLimit, f.rateRemaining
		}
		reset, _ := strconv.Atoi(fakeRateReset)
		res := func(l, rem int) object { return object{"limit": l, "used": l - rem, "remaining": rem, "reset": reset} }
		f.writeJSON(w, r, http.StatusOK, object{"resources": object{"core": res(limit, remaining), "search": res(30, 28), "graphql": res(5000, 4999)}, "rate": res(limit, remaining)})
	case len(seg) == 1 && seg[0] == "graphql":
		if r.Method != http.MethodPost {
			notFound(w)
			return
		}
		if !needAuth() {
			return
		}
		f.graphql(w, r, body)
	case len(seg) == 2 && seg[0] == "search":
		f.search(w, r, seg[1])
	case seg[0] == "user":
		if !needAuth() {
			return
		}
		switch {
		case len(seg) == 1:
			u := userObj(fakeViewer)
			u["name"] = "Octo Cat"
			f.writeJSON(w, r, http.StatusOK, u)
		case len(seg) == 2 && seg[1] == "repos":
			f.listRepos(w, r, f.affiliated(q.Get("affiliation")), q)
		default:
			notFound(w)
		}
	case seg[0] == "users" && len(seg) >= 2:
		login, typ, ok := f.ownerLogin(seg[1])
		if !ok {
			notFound(w)
			return
		}
		switch {
		case len(seg) == 2:
			u := userObj(login)
			u["type"] = typ
			f.writeJSON(w, r, http.StatusOK, u)
		case len(seg) == 3 && seg[2] == "repos":
			f.listRepos(w, r, f.ownedBy(seg[1], authed), q)
		default:
			notFound(w)
		}
	case seg[0] == "orgs" && len(seg) == 3 && seg[2] == "repos":
		if typ, ok := f.ownerType(seg[1]); !ok || typ != "Organization" {
			notFound(w)
			return
		}
		f.listRepos(w, r, f.ownedBy(seg[1], authed), q)
	case seg[0] == "repos" && len(seg) >= 3:
		rp := f.findRepo(seg[1], seg[2])
		if rp == nil || (rp.private && !authed) {
			notFound(w)
			return
		}
		if r.Method != http.MethodGet && !needAuth() {
			return
		}
		f.repoRoute(w, r, rp, seg[3:], body)
	default:
		notFound(w)
	}
}

// affiliated lists the token's repositories for /user/repos.
func (f *fakeGitHub) affiliated(affiliation string) []*fakeRepo {
	kinds := map[string]bool{}
	if affiliation == "" {
		affiliation = "owner,collaborator,organization_member"
	}
	for _, k := range strings.Split(affiliation, ",") {
		kinds[strings.TrimSpace(k)] = true
	}
	var out []*fakeRepo
	for _, rp := range f.repos {
		typ, _ := f.ownerType(rp.owner)
		switch {
		case kinds["owner"] && strings.EqualFold(rp.owner, fakeViewer),
			kinds["collaborator"] && f.collaborations[rp.full()],
			kinds["organization_member"] && typ == "Organization":
			out = append(out, rp)
		}
	}
	return out
}

// ownedBy lists an owner's repositories, the private ones only with a token.
func (f *fakeGitHub) ownedBy(owner string, authed bool) []*fakeRepo {
	var out []*fakeRepo
	for _, rp := range f.repos {
		if strings.EqualFold(rp.owner, owner) && (authed || !rp.private) {
			out = append(out, rp)
		}
	}
	return out
}

func (f *fakeGitHub) listRepos(w http.ResponseWriter, r *http.Request, repos []*fakeRepo, q url.Values) {
	items := make([]object, 0, len(repos))
	for _, rp := range repos {
		items = append(items, f.repoObject(rp))
	}
	switch q.Get("sort") {
	case "full_name":
		sort.SliceStable(items, func(i, j int) bool {
			return strings.ToLower(str(items[i], "full_name")) < strings.ToLower(str(items[j], "full_name"))
		})
	case "updated", "pushed", "created":
		items = sortItems(items, q.Get("sort")+"_at", q.Get("direction") != "asc")
	}
	f.page(w, r, items, "")
}

func (f *fakeGitHub) repoRoute(w http.ResponseWriter, r *http.Request, rp *fakeRepo, rest []string, body []byte) {
	get := r.Method == http.MethodGet
	q := r.URL.Query()
	if len(rest) == 0 {
		if !get {
			notFound(w)
			return
		}
		f.writeJSON(w, r, http.StatusOK, f.repoObject(rp))
		return
	}
	switch rest[0] {
	case "topics":
		if len(rest) == 1 && get {
			f.writeJSON(w, r, http.StatusOK, object{"names": rp.topics})
			return
		}
	case "issues":
		switch {
		case len(rest) == 1 && get:
			f.page(w, r, f.filterIssues(rp, q), "")
			return
		case len(rest) == 1 && r.Method == http.MethodPost:
			f.createIssue(w, r, rp, body)
			return
		case rest[1] == "comments" && len(rest) == 2 && get:
			f.page(w, r, f.filterComments(rp.comments, q), "")
			return
		case rest[1] == "comments" && len(rest) == 3:
			f.commentRoute(w, r, rp, rest[2], body)
			return
		case rest[1] != "comments":
			n, err := strconv.Atoi(rest[1])
			if err != nil {
				break
			}
			is := findBy(rp.issues, "number", int64(n))
			if is == nil {
				notFound(w)
				return
			}
			switch {
			case len(rest) == 2 && get:
				f.writeJSON(w, r, http.StatusOK, is)
				return
			case len(rest) == 2 && r.Method == http.MethodPatch:
				f.updateIssue(w, r, rp, is, body)
				return
			case len(rest) == 3 && rest[2] == "comments" && get:
				var mine []object
				for _, c := range rp.comments {
					if strings.HasSuffix(str(c, "issue_url"), "/"+rest[1]) {
						mine = append(mine, c)
					}
				}
				f.page(w, r, f.filterComments(mine, q), "")
				return
			case len(rest) == 3 && rest[2] == "comments" && r.Method == http.MethodPost:
				in, ok := decodeBody(w, body)
				if !ok {
					return
				}
				if str(in, "body") == "" {
					validationError(w, "IssueComment", "body")
					return
				}
				c := f.addComment(rp, n, fakeViewer, str(in, "body"), editedAt)
				f.writeJSON(w, r, http.StatusCreated, c)
				return
			}
		}
	case "pulls":
		switch {
		case len(rest) == 1 && get:
			f.page(w, r, f.filterPulls(rp, q), "")
			return
		case len(rest) == 1 && r.Method == http.MethodPost:
			f.createPull(w, r, rp, body)
			return
		case len(rest) == 2:
			n, err := strconv.Atoi(rest[1])
			if err != nil {
				break
			}
			pr := findBy(rp.pulls, "number", int64(n))
			if pr == nil {
				notFound(w)
				return
			}
			switch r.Method {
			case http.MethodGet:
				f.writeJSON(w, r, http.StatusOK, pr)
				return
			case http.MethodPatch:
				in, ok := decodeBody(w, body)
				if !ok {
					return
				}
				for _, k := range []string{"title", "body", "state"} {
					if v, ok := in[k]; ok {
						pr[k] = v
					}
				}
				if v, ok := in["base"]; ok {
					pr["base"] = object{"ref": v, "sha": shaOf(fmt.Sprint(v)), "label": rp.owner + ":" + fmt.Sprint(v)}
				}
				if str(pr, "state") == "closed" && pr["closed_at"] == nil {
					pr["closed_at"] = editedAt
				}
				pr["updated_at"] = editedAt
				f.syncPullIssue(rp, pr)
				f.writeJSON(w, r, http.StatusOK, pr)
				return
			}
		}
	case "commits":
		switch {
		case len(rest) == 1 && get:
			f.page(w, r, f.filterCommits(rp, q), "")
			return
		case len(rest) == 2 && get:
			for _, c := range rp.commits {
				if strings.HasPrefix(str(c, "sha"), rest[1]) {
					f.writeJSON(w, r, http.StatusOK, c)
					return
				}
			}
			if b := findBy(rp.branches, "name", rest[1]); b != nil {
				if c := findBy(rp.commits, "sha", str(sub(b, "commit"), "sha")); c != nil {
					f.writeJSON(w, r, http.StatusOK, c)
					return
				}
			}
			writeError(w, http.StatusUnprocessableEntity, "No commit found for SHA: "+rest[1])
			return
		}
	case "branches":
		switch {
		case len(rest) == 1 && get:
			items := rp.branches
			if p := q.Get("protected"); p != "" {
				want := p == "true"
				items = nil
				for _, b := range rp.branches {
					if prot, _ := b["protected"].(bool); prot == want {
						items = append(items, b)
					}
				}
			}
			f.page(w, r, items, "")
			return
		case len(rest) >= 2 && get:
			if b := findBy(rp.branches, "name", strings.Join(rest[1:], "/")); b != nil {
				f.writeJSON(w, r, http.StatusOK, b)
				return
			}
			writeError(w, http.StatusNotFound, "Branch not found")
			return
		}
	case "tags":
		if len(rest) == 1 && get {
			f.page(w, r, rp.tags, "")
			return
		}
	case "releases":
		switch {
		case len(rest) == 1 && get:
			f.page(w, r, rp.releases, "")
			return
		case len(rest) == 1 && r.Method == http.MethodPost:
			in, ok := decodeBody(w, body)
			if !ok {
				return
			}
			if str(in, "tag_name") == "" {
				validationError(w, "Release", "tag_name")
				return
			}
			draft, _ := in["draft"].(bool)
			pre, _ := in["prerelease"].(bool)
			target := str(in, "target_commitish")
			if target == "" {
				target = rp.defaultBranch
			}
			rel := f.addRelease(rp, str(in, "tag_name"), target, str(in, "name"), draft, pre, str(in, "body"), editedAt)
			f.writeJSON(w, r, http.StatusCreated, rel)
			return
		case len(rest) == 2:
			id, err := strconv.Atoi(rest[1])
			if err != nil {
				break
			}
			rel := findBy(rp.releases, "id", int64(id))
			if rel == nil {
				notFound(w)
				return
			}
			switch r.Method {
			case http.MethodGet:
				f.writeJSON(w, r, http.StatusOK, rel)
				return
			case http.MethodPatch:
				in, ok := decodeBody(w, body)
				if !ok {
					return
				}
				for _, k := range []string{"tag_name", "target_commitish", "name", "body", "draft", "prerelease"} {
					if v, ok := in[k]; ok {
						rel[k] = v
					}
				}
				f.writeJSON(w, r, http.StatusOK, rel)
				return
			case http.MethodDelete:
				rp.releases = slices.DeleteFunc(rp.releases, func(o object) bool { return num(o, "id") == int64(id) })
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	case "labels":
		switch {
		case len(rest) == 1 && get:
			f.page(w, r, rp.labels, "")
			return
		case len(rest) == 1 && r.Method == http.MethodPost:
			in, ok := decodeBody(w, body)
			if !ok {
				return
			}
			if str(in, "name") == "" {
				validationError(w, "Label", "name")
				return
			}
			if findBy(rp.labels, "name", str(in, "name")) != nil {
				writeError(w, http.StatusUnprocessableEntity, "Validation Failed", object{"resource": "Label", "field": "name", "code": "already_exists"})
				return
			}
			color := str(in, "color")
			if color == "" {
				color = "ededed"
			}
			l := f.addLabel(rp, str(in, "name"), color, str(in, "description"), false)
			f.writeJSON(w, r, http.StatusCreated, l)
			return
		case len(rest) == 2:
			l := findBy(rp.labels, "name", rest[1])
			if l == nil {
				notFound(w)
				return
			}
			switch r.Method {
			case http.MethodGet:
				f.writeJSON(w, r, http.StatusOK, l)
				return
			case http.MethodPatch:
				in, ok := decodeBody(w, body)
				if !ok {
					return
				}
				if v, ok := in["new_name"]; ok {
					l["name"] = v
				}
				for _, k := range []string{"color", "description"} {
					if v, ok := in[k]; ok {
						l[k] = v
					}
				}
				f.writeJSON(w, r, http.StatusOK, l)
				return
			case http.MethodDelete:
				rp.labels = slices.DeleteFunc(rp.labels, func(o object) bool { return str(o, "name") == rest[1] })
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	case "milestones":
		switch {
		case len(rest) == 1 && get:
			f.page(w, r, f.filterMilestones(rp, q), "")
			return
		case len(rest) == 1 && r.Method == http.MethodPost:
			in, ok := decodeBody(w, body)
			if !ok {
				return
			}
			if str(in, "title") == "" {
				validationError(w, "Milestone", "title")
				return
			}
			state := str(in, "state")
			if state == "" {
				state = "open"
			}
			m := f.addMilestone(rp, str(in, "title"), str(in, "description"), state, str(in, "due_on"))
			f.writeJSON(w, r, http.StatusCreated, m)
			return
		case len(rest) == 2:
			n, err := strconv.Atoi(rest[1])
			if err != nil {
				break
			}
			m := findBy(rp.milestones, "number", int64(n))
			if m == nil {
				notFound(w)
				return
			}
			switch r.Method {
			case http.MethodGet:
				f.writeJSON(w, r, http.StatusOK, m)
				return
			case http.MethodPatch:
				in, ok := decodeBody(w, body)
				if !ok {
					return
				}
				for _, k := range []string{"title", "state", "description", "due_on"} {
					if v, ok := in[k]; ok {
						m[k] = v
					}
				}
				m["updated_at"] = editedAt
				f.writeJSON(w, r, http.StatusOK, m)
				return
			case http.MethodDelete:
				rp.milestones = slices.DeleteFunc(rp.milestones, func(o object) bool { return num(o, "number") == int64(n) })
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	case "actions":
		switch {
		case len(rest) == 2 && rest[1] == "runs" && get:
			f.page(w, r, f.filterRuns(rp, q), "workflow_runs")
			return
		case len(rest) == 3 && rest[1] == "runs" && get:
			id, _ := strconv.Atoi(rest[2])
			if run := findBy(rp.runs, "id", int64(id)); run != nil {
				f.writeJSON(w, r, http.StatusOK, run)
				return
			}
			notFound(w)
			return
		}
	}
	notFound(w)
}

// findBy finds the item whose member equals v (compared as text).
func findBy(items []object, field string, v any) object {
	want := fmt.Sprint(v)
	for _, it := range items {
		if fmt.Sprint(it[field]) == want {
			return it
		}
	}
	return nil
}

func (f *fakeGitHub) commentRoute(w http.ResponseWriter, r *http.Request, rp *fakeRepo, idText string, body []byte) {
	id, err := strconv.Atoi(idText)
	if err != nil {
		notFound(w)
		return
	}
	c := findBy(rp.comments, "id", int64(id))
	if c == nil {
		notFound(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.writeJSON(w, r, http.StatusOK, c)
	case http.MethodPatch:
		in, ok := decodeBody(w, body)
		if !ok {
			return
		}
		if str(in, "body") == "" {
			validationError(w, "IssueComment", "body")
			return
		}
		c["body"], c["updated_at"] = in["body"], editedAt
		f.writeJSON(w, r, http.StatusOK, c)
	case http.MethodDelete:
		rp.comments = slices.DeleteFunc(rp.comments, func(o object) bool { return num(o, "id") == int64(id) })
		w.WriteHeader(http.StatusNoContent)
	default:
		notFound(w)
	}
}

func (f *fakeGitHub) createIssue(w http.ResponseWriter, r *http.Request, rp *fakeRepo, body []byte) {
	in, ok := decodeBody(w, body)
	if !ok {
		return
	}
	if str(in, "title") == "" {
		validationError(w, "Issue", "title")
		return
	}
	var milestone object
	if in["milestone"] != nil {
		if milestone = findBy(rp.milestones, "number", in["milestone"]); milestone == nil {
			writeError(w, http.StatusUnprocessableEntity, "Validation Failed", object{"resource": "Issue", "field": "milestone", "code": "invalid"})
			return
		}
	}
	is := f.newIssue(rp, fakeViewer, str(in, "title"), str(in, "body"), stringsOf(in["labels"]), stringsOf(in["assignees"]), milestone, editedAt)
	f.writeJSON(w, r, http.StatusCreated, is)
}

func (f *fakeGitHub) updateIssue(w http.ResponseWriter, r *http.Request, rp *fakeRepo, is object, body []byte) {
	in, ok := decodeBody(w, body)
	if !ok {
		return
	}
	for _, k := range []string{"title", "body", "state", "state_reason"} {
		if v, ok := in[k]; ok {
			is[k] = v
		}
	}
	if v, ok := in["labels"]; ok {
		is["labels"] = f.labelObjects(rp, stringsOf(v))
	}
	if v, ok := in["assignees"]; ok {
		is["assignees"] = userObjs(stringsOf(v))
		is["assignee"] = nil
		if logins := stringsOf(v); len(logins) > 0 {
			is["assignee"] = userObj(logins[0])
		}
	}
	if v, ok := in["milestone"]; ok {
		if v == nil {
			is["milestone"] = nil
		} else if m := findBy(rp.milestones, "number", v); m != nil {
			is["milestone"] = m
		} else {
			writeError(w, http.StatusUnprocessableEntity, "Validation Failed", object{"resource": "Issue", "field": "milestone", "code": "invalid"})
			return
		}
	}
	switch str(is, "state") {
	case "closed":
		if is["closed_at"] == nil {
			is["closed_at"] = editedAt
		}
		if is["state_reason"] == nil {
			is["state_reason"] = "completed"
		}
	case "open":
		is["closed_at"] = nil
	}
	is["updated_at"] = editedAt
	f.writeJSON(w, r, http.StatusOK, is)
}

func (f *fakeGitHub) createPull(w http.ResponseWriter, r *http.Request, rp *fakeRepo, body []byte) {
	in, ok := decodeBody(w, body)
	if !ok {
		return
	}
	for _, field := range []string{"title", "head", "base"} {
		if str(in, field) == "" {
			validationError(w, "PullRequest", field)
			return
		}
	}
	draft, _ := in["draft"].(bool)
	pr := f.newPull(rp, fakeViewer, str(in, "title"), str(in, "body"), str(in, "head"), str(in, "base"), draft, editedAt)
	f.writeJSON(w, r, http.StatusCreated, pr)
}

// Filters, as the list endpoints apply them.

func (f *fakeGitHub) filterIssues(rp *fakeRepo, q url.Values) []object {
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	var since time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}
	var want []string
	if l := q.Get("labels"); l != "" {
		for _, n := range strings.Split(l, ",") {
			want = append(want, strings.TrimSpace(n))
		}
	}
	var out []object
	for _, is := range rp.issues {
		if state != "all" && str(is, "state") != state {
			continue
		}
		labels := memberNames(is, "labels", "name")
		if slices.ContainsFunc(want, func(n string) bool { return !slices.Contains(labels, n) }) {
			continue
		}
		assignees := memberNames(is, "assignees", "login")
		switch a := q.Get("assignee"); {
		case a == "":
		case a == "none" && len(assignees) > 0, a == "*" && len(assignees) == 0:
			continue
		case a != "none" && a != "*" && !slices.Contains(assignees, a):
			continue
		}
		if c := q.Get("creator"); c != "" && str(sub(is, "user"), "login") != c {
			continue
		}
		ms := sub(is, "milestone")
		switch m := q.Get("milestone"); {
		case m == "":
		case m == "none" && ms != nil, m == "*" && ms == nil:
			continue
		case m != "none" && m != "*":
			if ms == nil || (str(ms, "title") != m && fmt.Sprint(ms["number"]) != m) {
				continue
			}
		}
		if !since.IsZero() && tm(is, "updated_at").Before(since) {
			continue
		}
		out = append(out, is)
	}
	field := map[string]string{"": "created_at", "created": "created_at", "updated": "updated_at", "comments": "comments"}[q.Get("sort")]
	if field == "" {
		field = "created_at"
	}
	return sortItems(out, field, q.Get("direction") != "asc")
}

func (f *fakeGitHub) filterPulls(rp *fakeRepo, q url.Values) []object {
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	var out []object
	for _, pr := range rp.pulls {
		if state != "all" && str(pr, "state") != state {
			continue
		}
		if h := q.Get("head"); h != "" {
			ref := str(sub(pr, "head"), "ref")
			if h != ref && h != str(sub(pr, "head"), "label") {
				continue
			}
		}
		if b := q.Get("base"); b != "" && str(sub(pr, "base"), "ref") != b {
			continue
		}
		out = append(out, pr)
	}
	field := map[string]string{"created": "created_at", "updated": "updated_at", "popularity": "comments", "long-running": "created_at"}[q.Get("sort")]
	if field == "" {
		field = "created_at"
	}
	return sortItems(out, field, q.Get("direction") != "asc")
}

func (f *fakeGitHub) filterComments(all []object, q url.Values) []object {
	var since time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}
	var out []object
	for _, c := range all {
		if !since.IsZero() && tm(c, "updated_at").Before(since) {
			continue
		}
		out = append(out, c)
	}
	field := "created_at"
	if q.Get("sort") == "updated" {
		field = "updated_at"
	}
	return sortItems(out, field, q.Get("direction") == "desc")
}

func (f *fakeGitHub) filterCommits(rp *fakeRepo, q url.Values) []object {
	items := rp.commits
	if sha := q.Get("sha"); sha != "" && findBy(rp.branches, "name", sha) == nil {
		// A commit sha lists the history from that commit on.
		start := slices.IndexFunc(rp.commits, func(c object) bool { return strings.HasPrefix(str(c, "sha"), sha) })
		if start < 0 {
			return []object{}
		}
		items = rp.commits[start:]
	}
	var since, until time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}
	if s := q.Get("until"); s != "" {
		until, _ = time.Parse(time.RFC3339, s)
	}
	var out []object
	for _, c := range items {
		if a := q.Get("author"); a != "" {
			login := str(sub(c, "author"), "login")
			email := str(sub(sub(c, "commit"), "author"), "email")
			if login != a && email != a {
				continue
			}
		}
		d := tm(sub(sub(c, "commit"), "author"), "date")
		if !since.IsZero() && d.Before(since) || !until.IsZero() && d.After(until) {
			continue
		}
		out = append(out, c)
	}
	if out == nil {
		out = []object{}
	}
	return out
}

func (f *fakeGitHub) filterMilestones(rp *fakeRepo, q url.Values) []object {
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	var out []object
	for _, m := range rp.milestones {
		if state == "all" || str(m, "state") == state {
			out = append(out, m)
		}
	}
	field := "due_on"
	if q.Get("sort") == "completeness" {
		field = "closed_issues"
	}
	return sortItems(out, field, q.Get("direction") == "desc")
}

func (f *fakeGitHub) filterRuns(rp *fakeRepo, q url.Values) []object {
	var out []object
	for _, run := range rp.runs {
		if b := q.Get("branch"); b != "" && str(run, "head_branch") != b {
			continue
		}
		if e := q.Get("event"); e != "" && str(run, "event") != e {
			continue
		}
		if s := q.Get("status"); s != "" && str(run, "status") != s && fmt.Sprint(run["conclusion"]) != s {
			continue
		}
		if a := q.Get("actor"); a != "" && str(sub(run, "actor"), "login") != a {
			continue
		}
		if c := q.Get("created"); c != "" && !matchesCreated(str(run, "created_at"), c) {
			continue
		}
		out = append(out, run)
	}
	if out == nil {
		out = []object{}
	}
	return out
}

// matchesCreated applies GitHub's date qualifier (">=2024-01-01",
// "<2024-02-01T00:00:00Z", "2024-01-01..2024-01-31", a bare date) to an
// RFC 3339 timestamp.
func matchesCreated(created, q string) bool {
	t, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return false
	}
	parse := func(s string) (time.Time, bool) {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if d, err := time.Parse(layout, s); err == nil {
				return d, true
			}
		}
		return time.Time{}, false
	}
	if a, b, ok := strings.Cut(q, ".."); ok {
		if a != "*" {
			if lo, ok := parse(a); !ok || t.Before(lo) {
				return false
			}
		}
		if b != "*" {
			if hi, ok := parse(b); !ok || t.After(hi.Add(24*time.Hour-time.Nanosecond)) {
				return false
			}
		}
		return true
	}
	for _, op := range []string{">=", "<=", ">", "<"} {
		if rest, ok := strings.CutPrefix(q, op); ok {
			d, ok := parse(rest)
			if !ok {
				return false
			}
			switch op {
			case ">=":
				return !t.Before(d)
			case "<=":
				return !t.After(d)
			case ">":
				return t.After(d)
			default:
				return t.Before(d)
			}
		}
	}
	d, ok := parse(q)
	return ok && t.Format("2006-01-02") == d.Format("2006-01-02")
}

// Search.

// searchTerms splits a search text into qualifiers (name:value, the value
// unquoted) and free words, honouring "quoted phrases".
func searchTerms(q string) (qualifiers map[string][]string, words []string) {
	qualifiers = map[string][]string{}
	var tok strings.Builder
	quoted := false
	flush := func() {
		t := tok.String()
		tok.Reset()
		if t == "" {
			return
		}
		if k, v, ok := strings.Cut(t, ":"); ok && k != "" && !strings.HasPrefix(t, "http") {
			qualifiers[k] = append(qualifiers[k], strings.Trim(v, `"`))
			return
		}
		words = append(words, strings.ToLower(strings.Trim(t, `"`)))
	}
	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
			tok.WriteRune(r)
		case !quoted && (r == ' ' || r == '\t'):
			flush()
		default:
			tok.WriteRune(r)
		}
	}
	flush()
	return qualifiers, words
}

// inRange applies a search range qualifier (>=x, >x, <=x, <x, x) to a
// number or an RFC 3339 instant.
func inRange(qual string, actual any) bool {
	op, bound := "", qual
	for _, p := range []string{">=", "<=", ">", "<"} {
		if strings.HasPrefix(qual, p) {
			op, bound = p, qual[len(p):]
			break
		}
	}
	var cmp int
	if t, ok := actual.(time.Time); ok {
		b, err := time.Parse(time.RFC3339, bound)
		if err != nil {
			if b, err = time.Parse("2006-01-02", bound); err != nil {
				return false
			}
		}
		cmp = t.Compare(b)
	} else {
		b, err := strconv.ParseInt(bound, 10, 64)
		if err != nil {
			return false
		}
		n, _ := actual.(int64)
		switch {
		case n < b:
			cmp = -1
		case n > b:
			cmp = 1
		}
	}
	switch op {
	case ">=":
		return cmp >= 0
	case ">":
		return cmp > 0
	case "<=":
		return cmp <= 0
	case "<":
		return cmp < 0
	}
	return cmp == 0
}

// search serves /search/{kind}. Private repositories are searched with the
// token only, as GitHub does for the ones the token can see.
func (f *fakeGitHub) search(w http.ResponseWriter, r *http.Request, kind string) {
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("q")) == "" {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed", object{"resource": "Search", "field": "q", "code": "missing"})
		return
	}
	authed := r.Header.Get("Authorization") == "Bearer "+f.token
	qualifiers, words := searchTerms(q.Get("q"))
	has := func(k, v string) bool { return slices.Contains(qualifiers[k], v) }
	wordsIn := func(texts ...string) bool {
		joined := strings.ToLower(strings.Join(texts, " "))
		for _, w := range words {
			if !strings.Contains(joined, w) {
				return false
			}
		}
		return true
	}
	// GitHub ORs repo:, user: and org: together: an item matches when its
	// repository is one named, or its owner is.
	repoMatches := func(rp *fakeRepo) bool {
		var any, matched bool
		if rs := qualifiers["repo"]; len(rs) > 0 {
			any = true
			matched = slices.ContainsFunc(rs, func(s string) bool { return strings.EqualFold(s, rp.full()) })
		}
		for _, k := range []string{"user", "org"} {
			if os := qualifiers[k]; len(os) > 0 {
				any = true
				matched = matched || slices.ContainsFunc(os, func(s string) bool { return strings.EqualFold(s, rp.owner) })
			}
		}
		return !any || matched
	}
	// ranges applies every range qualifier named in fields to the item.
	ranges := func(it object, fields map[string]string) bool {
		for qual, member := range fields {
			for _, v := range qualifiers[qual] {
				var actual any
				switch x := it[member].(type) {
				case string:
					t, err := time.Parse(time.RFC3339, x)
					if err != nil {
						return false
					}
					actual = t
				case nil:
					return false
				default:
					actual = num(it, member)
				}
				if !inRange(v, actual) {
					return false
				}
			}
		}
		return true
	}
	var items []object
	switch kind {
	case "issues":
		for _, rp := range f.repos {
			if !repoMatches(rp) || (rp.private && !authed) {
				continue
			}
			for _, is := range rp.issues {
				_, isPR := is["pull_request"]
				switch {
				case has("is", "pr") && !isPR, has("is", "issue") && isPR, has("type", "pr") && !isPR, has("type", "issue") && isPR:
					continue
				case has("is", "open") && str(is, "state") != "open", has("is", "closed") && str(is, "state") != "closed":
					continue
				case has("state", "open") && str(is, "state") != "open", has("state", "closed") && str(is, "state") != "closed":
					continue
				}
				labels := memberNames(is, "labels", "name")
				if slices.ContainsFunc(qualifiers["label"], func(l string) bool { return !slices.Contains(labels, l) }) {
					continue
				}
				if as := qualifiers["author"]; len(as) > 0 && !slices.Contains(as, str(sub(is, "user"), "login")) {
					continue
				}
				assignees := memberNames(is, "assignees", "login")
				if as := qualifiers["assignee"]; len(as) > 0 && !slices.ContainsFunc(as, func(a string) bool { return slices.Contains(assignees, a) }) {
					continue
				}
				ms := sub(is, "milestone")
				if m := qualifiers["milestone"]; len(m) > 0 && (ms == nil || !slices.Contains(m, str(ms, "title"))) {
					continue
				}
				switch {
				case has("no", "assignee") && len(assignees) > 0, has("no", "milestone") && ms != nil, has("no", "label") && len(labels) > 0:
					continue
				}
				if !ranges(is, map[string]string{"comments": "comments", "created": "created_at", "updated": "updated_at", "closed": "closed_at"}) {
					continue
				}
				if !wordsIn(str(is, "title"), str(is, "body")) {
					continue
				}
				it := object{"score": 1.0}
				for k, v := range is {
					it[k] = v
				}
				items = append(items, it)
			}
		}
		sortField := map[string]string{"created": "created_at", "updated": "updated_at", "comments": "comments"}[q.Get("sort")]
		if sortField == "" {
			sortField = "created_at"
		}
		items = sortItems(items, sortField, q.Get("order") != "asc")
	case "repositories":
		for _, rp := range f.repos {
			if (rp.private && !authed) || !repoMatches(rp) {
				continue
			}
			if ls := qualifiers["language"]; len(ls) > 0 && !slices.ContainsFunc(ls, func(l string) bool { return strings.EqualFold(l, rp.language) }) {
				continue
			}
			if !wordsIn(rp.name, rp.description) {
				continue
			}
			it := f.repoObject(rp)
			it["score"] = 1.0
			items = append(items, it)
		}
		if q.Get("sort") == "stars" {
			items = sortItems(items, "stargazers_count", q.Get("order") != "asc")
		}
	case "commits":
		if len(words) == 0 {
			// GitHub's commit search needs text beside the qualifiers.
			writeError(w, http.StatusUnprocessableEntity, "Validation Failed", object{"resource": "Search", "field": "q", "code": "invalid", "message": "The search needs a term besides the qualifiers"})
			return
		}
		for _, rp := range f.repos {
			if (rp.private && !authed) || !repoMatches(rp) {
				continue
			}
			for _, c := range rp.commits {
				commit := sub(c, "commit")
				author, committer := sub(commit, "author"), sub(commit, "committer")
				if as := qualifiers["author"]; len(as) > 0 && !slices.Contains(as, str(sub(c, "author"), "login")) {
					continue
				}
				if cs := qualifiers["committer"]; len(cs) > 0 && !slices.Contains(cs, str(sub(c, "committer"), "login")) {
					continue
				}
				if ns := qualifiers["author-name"]; len(ns) > 0 && !slices.Contains(ns, str(author, "name")) {
					continue
				}
				if es := qualifiers["author-email"]; len(es) > 0 && !slices.Contains(es, str(author, "email")) {
					continue
				}
				dates := object{"author_date": author["date"], "committer_date": committer["date"]}
				if !ranges(dates, map[string]string{"author-date": "author_date", "committer-date": "committer_date"}) {
					continue
				}
				if !wordsIn(str(commit, "message")) {
					continue
				}
				it := object{"score": 1.0, "repository": f.repoObject(rp), "author_date": author["date"], "committer_date": committer["date"]}
				for k, v := range c {
					it[k] = v
				}
				items = append(items, it)
			}
		}
		if field := map[string]string{"author-date": "author_date", "committer-date": "committer_date"}[q.Get("sort")]; field != "" {
			items = sortItems(items, field, q.Get("order") != "asc")
		}
	default:
		notFound(w)
		return
	}
	perPage := max(1, min(atoiDefault(q.Get("per_page"), 30), 100))
	page := max(1, atoiDefault(q.Get("page"), 1))
	total := len(items)
	shown := min(total, 1000) // GitHub serves the first 1000 results only
	start := min((page-1)*perPage, shown)
	end := min(start+perPage, shown)
	if link := linkHeader(r, page, perPage, shown); link != "" {
		w.Header().Set("Link", link)
	}
	f.writeJSON(w, r, http.StatusOK, object{"total_count": total, "incomplete_results": false, "items": append([]object{}, items[start:end]...)})
}

// GraphQL.

func (f *fakeGitHub) graphql(w http.ResponseWriter, r *http.Request, body []byte) {
	var in struct {
		Query     string          `json:"query"`
		Variables json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.Query) == "" {
		writeError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	var out any
	switch {
	case strings.Contains(in.Query, "mutation"):
		f.mutations = append(f.mutations, in.Query)
		out = object{"data": object{"addComment": object{"clientMutationId": nil}}}
	case strings.Contains(in.Query, "nope"):
		out = object{"data": nil, "errors": []object{{"message": "Field 'nope' doesn't exist on type 'Query'", "type": "undefinedField", "path": []string{"query", "nope"}, "locations": []object{{"line": 1, "column": 3}}}}}
	case strings.Contains(in.Query, "viewer"):
		out = object{"data": object{"viewer": object{"login": fakeViewer}}}
	default:
		out = object{"data": object{}}
	}
	b, _ := json.Marshal(out)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// openIssues is the fake's open_issues_count for a repository.
func (f *fakeGitHub) openIssues(owner, repo string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	rp := f.findRepo(owner, repo)
	if rp == nil {
		return -1
	}
	return int64(rp.openIssues())
}

// Test helpers.

// queryText runs a native query and collects its rows.
func queryText(t testing.TB, c *conn, text string) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	return queryWith(t, c, driver.Query{Text: text})
}

// queryWith runs a query and collects its rows.
func queryWith(t testing.TB, c *conn, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("query %s: %v", q.Text, err)
	}
	rows, stats, err := driver.Collect(context.Background(), rs, 0)
	if err != nil {
		t.Fatalf("collect %s: %v", q.Text, err)
	}
	return rs.Columns(), rows, stats
}

// colIndex finds a column by name.
func colIndex(t testing.TB, cols []driver.Column, name string) int {
	t.Helper()
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	t.Fatalf("no column %q in %v", name, columnNames(cols))
	return -1
}

func columnNames(cols []driver.Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// cell reads one named cell of a row.
func cell(t testing.TB, cols []driver.Column, row driver.Row, name string) driver.Value {
	t.Helper()
	return row[colIndex(t, cols, name)]
}

// wantKind fails unless err is of the given kind.
func wantKind(t testing.TB, what string, err error, kind error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error, want %v", what, kind)
	}
	if !errors.Is(err, kind) {
		t.Fatalf("%s: %v, want kind %v", what, err, kind)
	}
}
