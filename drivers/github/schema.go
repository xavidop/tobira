package github

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// A collection is one kind of repository resource, with a fixed set of
// columns read from the REST API's JSON. The tables below are the whole
// catalog: GitHub has no schema call, so Describe is static.

// column is one field of a collection.
type column struct {
	name     string
	native   string // what GitHub calls it, for Column.NativeType
	typ      driver.Type
	path     string                                            // dotted path in the item; "" with conv set
	conv     func(obj map[string]json.RawMessage) driver.Value // custom read, when path is not enough
	key      bool
	writable bool // the Mutator may set it; Describe lists the others as Generated
	required bool // an insert needs it; Describe lists the others as Nullable
	comment  string
}

// value reads the column from an item.
func (c column) value(obj map[string]json.RawMessage) driver.Value {
	if c.conv != nil {
		return c.conv(obj)
	}
	raw, ok := member(obj, c.path)
	if !ok {
		return driver.Null()
	}
	return typedValue(raw, c.typ)
}

// pushdown says how a Browse condition on a field becomes a query
// parameter of the list endpoint. encode returns the parameter value, or
// false when the condition's operator or value cannot be pushed.
type pushdown struct {
	param  string
	encode func(cond driver.Condition) (string, bool)
}

// endpoint builds the list path of a collection in a repository.
type endpoint func(owner, repo string) string

// keyed builds the path of one item.
type keyed func(owner, repo, key string) string

// mutation is one write endpoint and the members it accepts.
type mutation struct {
	method string
	path   keyed    // the item's path (update, delete) or the list path (insert, key ignored)
	fields []string // column names the body may carry
}

// collection is the static description of one resource kind.
type collection struct {
	name     string
	list     endpoint
	get      keyed  // "" = no single-item endpoint (tags): get lists and finds
	envelope string // member holding the items of a list answer, "" = bare array
	keyCol   string
	columns  []column
	// params are the list endpoint's own query parameters, allowed as members
	// of a list query.
	params []string
	// defaults are query parameters sent when the caller gives none.
	defaults url.Values
	// filters are the Browse conditions the server can apply; sorts the
	// fields it can order by, with the sort parameter's spelling.
	filters map[string]pushdown
	sorts   map[string]string
	// dropPullRequests: the issues endpoint lists pull requests too, which
	// have their own collection.
	dropPullRequests       bool
	insert, update, delete *mutation
	comment                string
	// Owner-wide collections (an owner namespace) read every repository of
	// a user or organization: search holds the search endpoint and the
	// qualifiers, listing marks the owner's repository listing. keys names
	// the primary key when it is not keyCol alone (repository and number).
	search  *searchSpec
	listing bool
	keys    []string
}

// primaryKey is the collection's key columns.
func (c *collection) primaryKey() []string {
	if len(c.keys) > 0 {
		return c.keys
	}
	return []string{c.keyCol}
}

// searchCommits is the commit search endpoint, which needs free text.
const searchCommits = "/search/commits"

// searchSpec describes an owner-wide collection read through GitHub search.
type searchSpec struct {
	endpoint   string               // /search/issues or /search/commits
	base       string               // qualifiers always sent, such as is:issue
	qualifiers map[string]qualifier // Browse conditions the search can take
	sorts      map[string]string    // fields the search can order by, with the sort value
	peer       string               // the repository collection edits go to, "" = read-only
}

// qualifier renders a Browse condition as a search qualifier, or false when
// the operator or value cannot be expressed.
type qualifier func(cond driver.Condition) (string, bool)

// col finds a column by name.
func (c *collection) col(name string) *column {
	for i := range c.columns {
		if c.columns[i].name == name {
			return &c.columns[i]
		}
	}
	return nil
}

// fields is the Describe view of the columns.
func (c *collection) fields() []driver.Field {
	out := make([]driver.Field, len(c.columns))
	for i, col := range c.columns {
		f := driver.Field{
			Name:       col.name,
			NativeType: col.native,
			Type:       col.typ,
			Nullable:   !col.required && !col.key,
			Generated:  !col.writable,
			Comment:    col.comment,
		}
		if col.key && !col.writable && c.insert != nil {
			// GitHub assigns the key of a new item (an issue number, a
			// release id): not computed from the row, just not chosen.
			f.Generated, f.AutoIncrement = false, true
		}
		out[i] = f
	}
	return out
}

// resultColumns is the ResultSet view of the columns, attributed to ref so
// the TUI can edit rows inline.
func (c *collection) resultColumns(ref driver.CollectionRef) []driver.Column {
	out := make([]driver.Column, len(c.columns))
	for i, col := range c.columns {
		out[i] = driver.Column{Name: col.name, NativeType: col.native, Type: col.typ, Source: &driver.CollectionRef{Namespace: ref.Namespace, Name: ref.Name}, Key: col.key}
	}
	return out
}

// row reads one item.
func (c *collection) row(item json.RawMessage) (driver.Row, error) {
	obj, err := objectOf(item)
	if err != nil {
		return nil, err
	}
	row := make(driver.Row, len(c.columns))
	for i, col := range c.columns {
		row[i] = col.value(obj)
	}
	return row, nil
}

// isPullRequest reports an issues-endpoint item that is a pull request.
func isPullRequest(item json.RawMessage) bool {
	obj, err := objectOf(item)
	if err != nil {
		return false
	}
	_, ok := member(obj, "pull_request")
	return ok
}

// Readers shared by several collections.

func login(p string) func(map[string]json.RawMessage) driver.Value {
	return func(obj map[string]json.RawMessage) driver.Value {
		raw, ok := member(obj, p+".login")
		if !ok {
			return driver.Null()
		}
		return typedValue(raw, driver.TypeString)
	}
}

// names reads an array of objects as the list of their "name" (or
// "login") members.
func names(p, field string) func(map[string]json.RawMessage) driver.Value {
	return func(obj map[string]json.RawMessage) driver.Value {
		raw, ok := member(obj, p)
		if !ok {
			return driver.Null()
		}
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return driver.JSON(raw)
		}
		out := make([]driver.Value, 0, len(items))
		for _, it := range items {
			if v, ok := member(it, field); ok {
				out = append(out, typedValue(v, driver.TypeString))
			}
		}
		return driver.List(out...)
	}
}

// count reads the length of an array member.
func count(p string) func(map[string]json.RawMessage) driver.Value {
	return func(obj map[string]json.RawMessage) driver.Value {
		raw, ok := member(obj, p)
		if !ok {
			return driver.Int(0)
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return driver.Null()
		}
		return driver.Int(int64(len(items)))
	}
}

// present reports whether a member exists (an issue's pull_request).
func present(p string) func(map[string]json.RawMessage) driver.Value {
	return func(obj map[string]json.RawMessage) driver.Value {
		_, ok := member(obj, p)
		return driver.Bool(ok)
	}
}

// trailingNumber reads the last path segment of a URL member as an integer
// (the issue number of an issue_url).
func trailingNumber(p string) func(map[string]json.RawMessage) driver.Value {
	return func(obj map[string]json.RawMessage) driver.Value {
		raw, ok := member(obj, p)
		if !ok {
			return driver.Null()
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return driver.Null()
		}
		n, err := strconv.ParseInt(path.Base(s), 10, 64)
		if err != nil {
			return driver.String(s)
		}
		return driver.Int(n)
	}
}

// repoFromURL reads "owner/repo" from a repository_url member.
func repoFromURL(p string) func(map[string]json.RawMessage) driver.Value {
	return func(obj map[string]json.RawMessage) driver.Value {
		raw, ok := member(obj, p)
		if !ok {
			return driver.Null()
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return driver.Null()
		}
		parts := strings.Split(strings.Trim(s, "/"), "/")
		if len(parts) < 2 {
			return driver.String(s)
		}
		return driver.String(parts[len(parts)-2] + "/" + parts[len(parts)-1])
	}
}

// Pushdown encoders.

// eqText pushes an equality on a text field.
func eqText(param string) pushdown {
	return pushdown{param: param, encode: func(cond driver.Condition) (string, bool) {
		if cond.Op != driver.OpEq {
			return "", false
		}
		return text(cond.Value)
	}}
}

// eqTextOrNone pushes an equality, is null ("none") or is not null ("*")
// on a field GitHub filters that way (assignee, milestone).
func eqTextOrNone(param string) pushdown {
	return pushdown{param: param, encode: func(cond driver.Condition) (string, bool) {
		switch cond.Op {
		case driver.OpEq:
			return text(cond.Value)
		case driver.OpIsNull:
			return "none", true
		case driver.OpNotNull:
			return "*", true
		}
		return "", false
	}}
}

// containsLabel pushes "labels contains x" (and equals, for one label) as
// labels=x.
func containsLabel(param string) pushdown {
	return pushdown{param: param, encode: func(cond driver.Condition) (string, bool) {
		if cond.Op != driver.OpContains && cond.Op != driver.OpEq {
			return "", false
		}
		return text(cond.Value)
	}}
}

// sinceTime pushes "field >= t" as since=t (GitHub's since is inclusive).
func sinceTime(param string) pushdown {
	return pushdown{param: param, encode: func(cond driver.Condition) (string, bool) {
		if cond.Op != driver.OpGe {
			return "", false
		}
		return timeText(cond.Value)
	}}
}

// untilTime pushes "field <= t" as until=t.
func untilTime(param string) pushdown {
	return pushdown{param: param, encode: func(cond driver.Condition) (string, bool) {
		if cond.Op != driver.OpLe {
			return "", false
		}
		return timeText(cond.Value)
	}}
}

// createdRange pushes a bound on created_at as GitHub's created=>=t /
// created=<=t (the workflow runs endpoint).
func createdRange(param string) pushdown {
	return pushdown{param: param, encode: func(cond driver.Condition) (string, bool) {
		t, ok := timeText(cond.Value)
		if !ok {
			return "", false
		}
		switch cond.Op {
		case driver.OpGe:
			return ">=" + t, true
		case driver.OpLe:
			return "<=" + t, true
		case driver.OpGt:
			return ">" + t, true
		case driver.OpLt:
			return "<" + t, true
		}
		return "", false
	}}
}

// eqBool pushes an equality on a boolean field (protected=true).
func eqBool(param string) pushdown {
	return pushdown{param: param, encode: func(cond driver.Condition) (string, bool) {
		if cond.Op != driver.OpEq {
			return "", false
		}
		b, ok := cond.Value.AsBool()
		if !ok {
			return "", false
		}
		return strconv.FormatBool(b), true
	}}
}

// timeText renders a time (or RFC 3339 text) for a query parameter.
func timeText(v driver.Value) (string, bool) {
	if t, ok := v.AsTime(); ok {
		return t.UTC().Format("2006-01-02T15:04:05Z"), true
	}
	if s, ok := v.AsText(); ok && s != "" {
		return s, true
	}
	return "", false
}

// Column sets.

func repoPath(suffix string) endpoint {
	return func(owner, repo string) string { return "/repos/" + owner + "/" + repo + suffix }
}

func itemPath(suffix string) keyed {
	return func(owner, repo, key string) string {
		return "/repos/" + owner + "/" + repo + suffix + "/" + url.PathEscape(key)
	}
}

var issueColumns = []column{
	{name: "number", native: "integer", typ: driver.TypeInt, path: "number", key: true, comment: "issue number"},
	{name: "title", native: "string", typ: driver.TypeString, path: "title", writable: true, required: true},
	{name: "state", native: "open | closed", typ: driver.TypeString, path: "state", writable: true},
	{name: "state_reason", native: "completed | not_planned | reopened", typ: driver.TypeString, path: "state_reason", writable: true},
	{name: "user", native: "login", typ: driver.TypeString, conv: login("user"), comment: "who opened it"},
	{name: "labels", native: "label names", typ: driver.TypeList, conv: names("labels", "name"), writable: true},
	{name: "assignees", native: "logins", typ: driver.TypeList, conv: names("assignees", "login"), writable: true},
	{name: "milestone", native: "milestone title", typ: driver.TypeString, path: "milestone.title", writable: true, comment: "edit with a title or a number"},
	{name: "comments", native: "integer", typ: driver.TypeInt, path: "comments"},
	{name: "locked", native: "boolean", typ: driver.TypeBool, path: "locked"},
	{name: "author_association", native: "string", typ: driver.TypeString, path: "author_association"},
	{name: "created_at", native: "timestamp", typ: driver.TypeTime, path: "created_at"},
	{name: "updated_at", native: "timestamp", typ: driver.TypeTime, path: "updated_at"},
	{name: "closed_at", native: "timestamp", typ: driver.TypeTime, path: "closed_at"},
	{name: "body", native: "markdown", typ: driver.TypeString, path: "body", writable: true},
	{name: "html_url", native: "url", typ: driver.TypeString, path: "html_url"},
}

var pullColumns = []column{
	{name: "number", native: "integer", typ: driver.TypeInt, path: "number", key: true, comment: "pull request number"},
	{name: "title", native: "string", typ: driver.TypeString, path: "title", writable: true, required: true},
	{name: "state", native: "open | closed", typ: driver.TypeString, path: "state", writable: true},
	{name: "draft", native: "boolean", typ: driver.TypeBool, path: "draft", writable: true},
	{name: "user", native: "login", typ: driver.TypeString, conv: login("user")},
	{name: "head", native: "branch", typ: driver.TypeString, path: "head.ref", writable: true, required: true, comment: "the branch with the changes; another fork's as user:branch"},
	{name: "base", native: "branch", typ: driver.TypeString, path: "base.ref", writable: true, required: true, comment: "the branch to merge into"},
	{name: "labels", native: "label names", typ: driver.TypeList, conv: names("labels", "name")},
	{name: "assignees", native: "logins", typ: driver.TypeList, conv: names("assignees", "login")},
	{name: "requested_reviewers", native: "logins", typ: driver.TypeList, conv: names("requested_reviewers", "login")},
	{name: "milestone", native: "milestone title", typ: driver.TypeString, path: "milestone.title"},
	{name: "merged_at", native: "timestamp", typ: driver.TypeTime, path: "merged_at"},
	{name: "merge_commit_sha", native: "sha", typ: driver.TypeString, path: "merge_commit_sha"},
	{name: "created_at", native: "timestamp", typ: driver.TypeTime, path: "created_at"},
	{name: "updated_at", native: "timestamp", typ: driver.TypeTime, path: "updated_at"},
	{name: "closed_at", native: "timestamp", typ: driver.TypeTime, path: "closed_at"},
	{name: "body", native: "markdown", typ: driver.TypeString, path: "body", writable: true},
	{name: "html_url", native: "url", typ: driver.TypeString, path: "html_url"},
}

var commentColumns = []column{
	{name: "id", native: "integer", typ: driver.TypeInt, path: "id", key: true},
	{name: "issue", native: "issue number", typ: driver.TypeInt, conv: trailingNumber("issue_url"), writable: true, required: true, comment: "the issue or pull request commented on"},
	{name: "user", native: "login", typ: driver.TypeString, conv: login("user")},
	{name: "body", native: "markdown", typ: driver.TypeString, path: "body", writable: true, required: true},
	{name: "author_association", native: "string", typ: driver.TypeString, path: "author_association"},
	{name: "created_at", native: "timestamp", typ: driver.TypeTime, path: "created_at"},
	{name: "updated_at", native: "timestamp", typ: driver.TypeTime, path: "updated_at"},
	{name: "html_url", native: "url", typ: driver.TypeString, path: "html_url"},
}

var commitColumns = []column{
	{name: "sha", native: "sha", typ: driver.TypeString, path: "sha", key: true},
	{name: "message", native: "string", typ: driver.TypeString, path: "commit.message"},
	{name: "author", native: "login", typ: driver.TypeString, conv: login("author"), comment: "the GitHub account, when one matches"},
	{name: "author_name", native: "string", typ: driver.TypeString, path: "commit.author.name"},
	{name: "author_email", native: "string", typ: driver.TypeString, path: "commit.author.email"},
	{name: "author_date", native: "timestamp", typ: driver.TypeTime, path: "commit.author.date"},
	{name: "committer", native: "login", typ: driver.TypeString, conv: login("committer")},
	{name: "committer_name", native: "string", typ: driver.TypeString, path: "commit.committer.name"},
	{name: "committer_date", native: "timestamp", typ: driver.TypeTime, path: "commit.committer.date"},
	{name: "parents", native: "shas", typ: driver.TypeList, conv: names("parents", "sha")},
	{name: "html_url", native: "url", typ: driver.TypeString, path: "html_url"},
}

var branchColumns = []column{
	{name: "name", native: "string", typ: driver.TypeString, path: "name", key: true},
	{name: "sha", native: "sha", typ: driver.TypeString, path: "commit.sha", comment: "the branch head"},
	{name: "protected", native: "boolean", typ: driver.TypeBool, path: "protected"},
}

var tagColumns = []column{
	{name: "name", native: "string", typ: driver.TypeString, path: "name", key: true},
	{name: "sha", native: "sha", typ: driver.TypeString, path: "commit.sha", comment: "the tagged commit"},
	{name: "zipball_url", native: "url", typ: driver.TypeString, path: "zipball_url"},
	{name: "tarball_url", native: "url", typ: driver.TypeString, path: "tarball_url"},
}

var releaseColumns = []column{
	{name: "id", native: "integer", typ: driver.TypeInt, path: "id", key: true},
	{name: "tag_name", native: "tag", typ: driver.TypeString, path: "tag_name", writable: true, required: true},
	{name: "target_commitish", native: "branch or sha", typ: driver.TypeString, path: "target_commitish", writable: true},
	{name: "name", native: "string", typ: driver.TypeString, path: "name", writable: true},
	{name: "draft", native: "boolean", typ: driver.TypeBool, path: "draft", writable: true},
	{name: "prerelease", native: "boolean", typ: driver.TypeBool, path: "prerelease", writable: true},
	{name: "author", native: "login", typ: driver.TypeString, conv: login("author")},
	{name: "assets", native: "count", typ: driver.TypeInt, conv: count("assets")},
	{name: "created_at", native: "timestamp", typ: driver.TypeTime, path: "created_at"},
	{name: "published_at", native: "timestamp", typ: driver.TypeTime, path: "published_at"},
	{name: "body", native: "markdown", typ: driver.TypeString, path: "body", writable: true},
	{name: "html_url", native: "url", typ: driver.TypeString, path: "html_url"},
}

var labelColumns = []column{
	{name: "name", native: "string", typ: driver.TypeString, path: "name", key: true, writable: true, required: true},
	{name: "color", native: "hex, no #", typ: driver.TypeString, path: "color", writable: true},
	{name: "description", native: "string", typ: driver.TypeString, path: "description", writable: true},
	{name: "default", native: "boolean", typ: driver.TypeBool, path: "default"},
	{name: "id", native: "integer", typ: driver.TypeInt, path: "id"},
}

var milestoneColumns = []column{
	{name: "number", native: "integer", typ: driver.TypeInt, path: "number", key: true},
	{name: "title", native: "string", typ: driver.TypeString, path: "title", writable: true, required: true},
	{name: "state", native: "open | closed", typ: driver.TypeString, path: "state", writable: true},
	{name: "description", native: "string", typ: driver.TypeString, path: "description", writable: true},
	{name: "creator", native: "login", typ: driver.TypeString, conv: login("creator")},
	{name: "open_issues", native: "integer", typ: driver.TypeInt, path: "open_issues"},
	{name: "closed_issues", native: "integer", typ: driver.TypeInt, path: "closed_issues"},
	{name: "due_on", native: "timestamp", typ: driver.TypeTime, path: "due_on", writable: true},
	{name: "created_at", native: "timestamp", typ: driver.TypeTime, path: "created_at"},
	{name: "updated_at", native: "timestamp", typ: driver.TypeTime, path: "updated_at"},
	{name: "closed_at", native: "timestamp", typ: driver.TypeTime, path: "closed_at"},
	{name: "html_url", native: "url", typ: driver.TypeString, path: "html_url"},
}

var workflowRunColumns = []column{
	{name: "id", native: "integer", typ: driver.TypeInt, path: "id", key: true},
	{name: "name", native: "workflow name", typ: driver.TypeString, path: "name"},
	{name: "display_title", native: "string", typ: driver.TypeString, path: "display_title"},
	{name: "run_number", native: "integer", typ: driver.TypeInt, path: "run_number"},
	{name: "run_attempt", native: "integer", typ: driver.TypeInt, path: "run_attempt"},
	{name: "event", native: "string", typ: driver.TypeString, path: "event"},
	{name: "status", native: "queued | in_progress | completed | …", typ: driver.TypeString, path: "status"},
	{name: "conclusion", native: "success | failure | cancelled | …", typ: driver.TypeString, path: "conclusion"},
	{name: "workflow_id", native: "integer", typ: driver.TypeInt, path: "workflow_id"},
	{name: "head_branch", native: "branch", typ: driver.TypeString, path: "head_branch"},
	{name: "head_sha", native: "sha", typ: driver.TypeString, path: "head_sha"},
	{name: "actor", native: "login", typ: driver.TypeString, conv: login("actor")},
	{name: "created_at", native: "timestamp", typ: driver.TypeTime, path: "created_at"},
	{name: "updated_at", native: "timestamp", typ: driver.TypeTime, path: "updated_at"},
	{name: "run_started_at", native: "timestamp", typ: driver.TypeTime, path: "run_started_at"},
	{name: "html_url", native: "url", typ: driver.TypeString, path: "html_url"},
}

// repositoryColumns are the rows of a repository search and of the
// repositories op.
var repositoryColumns = []column{
	{name: "full_name", native: "owner/name", typ: driver.TypeString, path: "full_name", key: true},
	{name: "owner", native: "login", typ: driver.TypeString, conv: login("owner")},
	{name: "name", native: "string", typ: driver.TypeString, path: "name"},
	{name: "private", native: "boolean", typ: driver.TypeBool, path: "private"},
	{name: "fork", native: "boolean", typ: driver.TypeBool, path: "fork"},
	{name: "archived", native: "boolean", typ: driver.TypeBool, path: "archived"},
	{name: "description", native: "string", typ: driver.TypeString, path: "description"},
	{name: "language", native: "string", typ: driver.TypeString, path: "language"},
	{name: "default_branch", native: "branch", typ: driver.TypeString, path: "default_branch"},
	{name: "stars", native: "integer", typ: driver.TypeInt, path: "stargazers_count"},
	{name: "forks", native: "integer", typ: driver.TypeInt, path: "forks_count"},
	{name: "open_issues", native: "integer", typ: driver.TypeInt, path: "open_issues_count", comment: "open issues and pull requests"},
	{name: "topics", native: "strings", typ: driver.TypeList, path: "topics"},
	{name: "pushed_at", native: "timestamp", typ: driver.TypeTime, path: "pushed_at"},
	{name: "created_at", native: "timestamp", typ: driver.TypeTime, path: "created_at"},
	{name: "updated_at", native: "timestamp", typ: driver.TypeTime, path: "updated_at"},
	{name: "html_url", native: "url", typ: driver.TypeString, path: "html_url"},
}

// collections is the catalog of every repository, in display order.
var collections = []*collection{
	{
		name: "issues", list: repoPath("/issues"), get: itemPath("/issues"), keyCol: "number", columns: issueColumns,
		params:   []string{"state", "labels", "assignee", "creator", "mentioned", "milestone", "since", "sort", "direction"},
		defaults: url.Values{"state": {"all"}},
		filters: map[string]pushdown{
			"state": eqText("state"), "labels": containsLabel("labels"), "assignees": eqTextOrNone("assignee"),
			"user": eqText("creator"), "milestone": eqTextOrNone("milestone"), "updated_at": sinceTime("since"),
		},
		sorts:            map[string]string{"created_at": "created", "updated_at": "updated", "comments": "comments"},
		dropPullRequests: true,
		insert:           &mutation{method: http.MethodPost, path: func(o, r, _ string) string { return "/repos/" + o + "/" + r + "/issues" }, fields: []string{"title", "body", "labels", "assignees", "milestone"}},
		update:           &mutation{method: http.MethodPatch, path: itemPath("/issues"), fields: []string{"title", "body", "state", "state_reason", "labels", "assignees", "milestone"}},
		comment:          "issues of the repository (pull requests are in pull_requests)",
	},
	{
		name: "pull_requests", list: repoPath("/pulls"), get: itemPath("/pulls"), keyCol: "number", columns: pullColumns,
		params:   []string{"state", "head", "base", "sort", "direction"},
		defaults: url.Values{"state": {"all"}},
		filters:  map[string]pushdown{"state": eqText("state"), "head": eqText("head"), "base": eqText("base")},
		sorts:    map[string]string{"created_at": "created", "updated_at": "updated"},
		insert:   &mutation{method: http.MethodPost, path: func(o, r, _ string) string { return "/repos/" + o + "/" + r + "/pulls" }, fields: []string{"title", "head", "base", "body", "draft"}},
		update:   &mutation{method: http.MethodPatch, path: itemPath("/pulls"), fields: []string{"title", "body", "state", "base"}},
		comment:  "pull requests",
	},
	{
		name: "issue_comments", list: repoPath("/issues/comments"), get: itemPath("/issues/comments"), keyCol: "id", columns: commentColumns,
		params:  []string{"sort", "direction", "since"},
		filters: map[string]pushdown{"updated_at": sinceTime("since")},
		sorts:   map[string]string{"created_at": "created", "updated_at": "updated"},
		insert:  &mutation{method: http.MethodPost, path: func(o, r, issue string) string { return "/repos/" + o + "/" + r + "/issues/" + issue + "/comments" }, fields: []string{"issue", "body"}},
		update:  &mutation{method: http.MethodPatch, path: itemPath("/issues/comments"), fields: []string{"body"}},
		delete:  &mutation{method: http.MethodDelete, path: itemPath("/issues/comments")},
		comment: "comments on issues and pull requests",
	},
	{
		name: "commits", list: repoPath("/commits"), get: itemPath("/commits"), keyCol: "sha", columns: commitColumns,
		params:  []string{"sha", "path", "author", "committer", "since", "until"},
		filters: map[string]pushdown{"author": eqText("author"), "committer": eqText("committer"), "author_date": sinceTime("since"), "committer_date": untilTime("until")},
		comment: "commits of the default branch, newest first (sha= lists another branch)",
	},
	{
		name: "branches", list: repoPath("/branches"), get: itemPath("/branches"), keyCol: "name", columns: branchColumns,
		params:  []string{"protected"},
		filters: map[string]pushdown{"protected": eqBool("protected")},
		comment: "branches",
	},
	{
		name: "tags", list: repoPath("/tags"), keyCol: "name", columns: tagColumns,
		comment: "tags, newest first",
	},
	{
		name: "releases", list: repoPath("/releases"), get: itemPath("/releases"), keyCol: "id", columns: releaseColumns,
		insert:  &mutation{method: http.MethodPost, path: func(o, r, _ string) string { return "/repos/" + o + "/" + r + "/releases" }, fields: []string{"tag_name", "target_commitish", "name", "body", "draft", "prerelease"}},
		update:  &mutation{method: http.MethodPatch, path: itemPath("/releases"), fields: []string{"tag_name", "target_commitish", "name", "body", "draft", "prerelease"}},
		delete:  &mutation{method: http.MethodDelete, path: itemPath("/releases")},
		comment: "releases, newest first",
	},
	{
		name: "labels", list: repoPath("/labels"), get: itemPath("/labels"), keyCol: "name", columns: labelColumns,
		insert:  &mutation{method: http.MethodPost, path: func(o, r, _ string) string { return "/repos/" + o + "/" + r + "/labels" }, fields: []string{"name", "color", "description"}},
		update:  &mutation{method: http.MethodPatch, path: itemPath("/labels"), fields: []string{"name", "color", "description"}},
		delete:  &mutation{method: http.MethodDelete, path: itemPath("/labels")},
		comment: "labels",
	},
	{
		name: "milestones", list: repoPath("/milestones"), get: itemPath("/milestones"), keyCol: "number", columns: milestoneColumns,
		params:   []string{"state", "sort", "direction"},
		defaults: url.Values{"state": {"all"}},
		filters:  map[string]pushdown{"state": eqText("state")},
		sorts:    map[string]string{"due_on": "due_on"},
		insert:   &mutation{method: http.MethodPost, path: func(o, r, _ string) string { return "/repos/" + o + "/" + r + "/milestones" }, fields: []string{"title", "state", "description", "due_on"}},
		update:   &mutation{method: http.MethodPatch, path: itemPath("/milestones"), fields: []string{"title", "state", "description", "due_on"}},
		delete:   &mutation{method: http.MethodDelete, path: itemPath("/milestones")},
		comment:  "milestones",
	},
	{
		name: "workflow_runs", list: repoPath("/actions/runs"), get: itemPath("/actions/runs"), envelope: "workflow_runs", keyCol: "id", columns: workflowRunColumns,
		params: []string{"actor", "branch", "event", "status", "created", "exclude_pull_requests", "check_suite_id", "head_sha"},
		filters: map[string]pushdown{
			"actor": eqText("actor"), "head_branch": eqText("branch"), "event": eqText("event"),
			"status": eqText("status"), "conclusion": eqText("status"), "head_sha": eqText("head_sha"), "created_at": createdRange("created"),
		},
		comment: "GitHub Actions workflow runs, newest first",
	},
}

// collectionByName finds a collection.
func collectionByName(name string) *collection {
	for _, c := range collections {
		if c.name == name {
			return c
		}
	}
	return nil
}

// collectionNames lists the collections in display order.
func collectionNames() []string {
	out := make([]string, len(collections))
	for i, c := range collections {
		out[i] = c.name
	}
	return out
}

// searchColumns are the columns of a search result of each type: the
// collection's, plus the repository each item belongs to.
func searchColumns(typ string) []column {
	repoCol := column{name: "repository", native: "owner/name", typ: driver.TypeString, conv: repoFromURL("repository_url")}
	switch typ {
	case "issues":
		cols := slices.Clone(issueColumns)
		cols = append(cols, column{name: "pull_request", native: "boolean", typ: driver.TypeBool, conv: present("pull_request")}, repoCol)
		return cols
	case "commits":
		cols := slices.Clone(commitColumns)
		return append(cols, column{name: "repository", native: "owner/name", typ: driver.TypeString, path: "repository.full_name"})
	case "repositories":
		return repositoryColumns
	}
	return nil
}

// Search qualifier encoders.

// quoteQualifier renders a qualifier value, quoting it when it has spaces.
func quoteQualifier(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

// eqQualifier pushes an equality as name:value.
func eqQualifier(name string) qualifier {
	return func(cond driver.Condition) (string, bool) {
		if cond.Op != driver.OpEq {
			return "", false
		}
		s, ok := text(cond.Value)
		if !ok || s == "" {
			return "", false
		}
		return name + ":" + quoteQualifier(s), true
	}
}

// eqOrNoneQualifier pushes an equality as name:value, is null as no:name
// (assignee, milestone, label).
func eqOrNoneQualifier(name string) qualifier {
	return func(cond driver.Condition) (string, bool) {
		switch cond.Op {
		case driver.OpEq, driver.OpContains:
			s, ok := text(cond.Value)
			if !ok || s == "" {
				return "", false
			}
			return name + ":" + quoteQualifier(s), true
		case driver.OpIsNull:
			return "no:" + name, true
		}
		return "", false
	}
}

// rangeQualifier pushes comparisons as name:>=value and the like; times are
// rendered in GitHub's date-time form.
func rangeQualifier(name string) qualifier {
	return func(cond driver.Condition) (string, bool) {
		var s string
		if t, ok := cond.Value.AsTime(); ok {
			s = t.UTC().Format("2006-01-02T15:04:05Z")
		} else if v, ok := text(cond.Value); ok && v != "" {
			s = v
		} else {
			return "", false
		}
		switch cond.Op {
		case driver.OpEq:
			return name + ":" + s, true
		case driver.OpGe:
			return name + ":>=" + s, true
		case driver.OpGt:
			return name + ":>" + s, true
		case driver.OpLe:
			return name + ":<=" + s, true
		case driver.OpLt:
			return name + ":<" + s, true
		}
		return "", false
	}
}

// repoQualifier pushes "repository = owner/name" (or a bare name within the
// owner) as repo:owner/name.
func repoQualifier(owner string) qualifier {
	return func(cond driver.Condition) (string, bool) {
		if cond.Op != driver.OpEq {
			return "", false
		}
		s, ok := text(cond.Value)
		if !ok || s == "" {
			return "", false
		}
		if !strings.Contains(s, "/") {
			s = owner + "/" + s
		}
		return "repo:" + s, true
	}
}

// Owner-wide collections.

// withRepository appends the repository column read from repository_url.
func withRepository(cols []column) []column {
	out := slices.Clone(cols)
	return append(out, column{name: "repository", native: "owner/name", typ: driver.TypeString, conv: repoFromURL("repository_url"), comment: "the repository, owner/name"})
}

var ownerIssueColumns = withRepository(issueColumns)

var ownerCommitColumns = func() []column {
	cols := slices.Clone(commitColumns)
	return append(cols, column{name: "repository", native: "owner/name", typ: driver.TypeString, path: "repository.full_name", comment: "the repository, owner/name"})
}()

// issueSearchQualifiers are the conditions the issue search takes.
var issueSearchQualifiers = map[string]qualifier{
	"state": eqQualifier("state"), "labels": eqOrNoneQualifier("label"), "user": eqQualifier("author"),
	"assignees": eqOrNoneQualifier("assignee"), "milestone": eqOrNoneQualifier("milestone"),
	"created_at": rangeQualifier("created"), "updated_at": rangeQualifier("updated"), "closed_at": rangeQualifier("closed"),
	"comments": rangeQualifier("comments"),
}

var issueSearchSorts = map[string]string{"created_at": "created", "updated_at": "updated", "comments": "comments"}

// ownerCollections are the collections of an owner namespace, in display
// order: the owner's repositories, and its issues, pull requests and
// commits across every repository, through GitHub search.
var ownerCollections = []*collection{
	{
		name: "repositories", listing: true, keyCol: "full_name", columns: repositoryColumns,
		params:  []string{"type", "sort", "direction"},
		sorts:   map[string]string{"created_at": "created", "updated_at": "updated", "pushed_at": "pushed", "full_name": "full_name"},
		comment: "the owner's repositories",
	},
	{
		name: "issues", keyCol: "number", keys: []string{"repository", "number"}, columns: ownerIssueColumns,
		params:  []string{"q", "sort", "order"},
		search:  &searchSpec{endpoint: "/search/issues", base: "is:issue", qualifiers: issueSearchQualifiers, sorts: issueSearchSorts, peer: "issues"},
		comment: "issues across every repository of the owner (GitHub search; at most 1000)",
	},
	{
		name: "pull_requests", keyCol: "number", keys: []string{"repository", "number"}, columns: ownerIssueColumns,
		params:  []string{"q", "sort", "order"},
		search:  &searchSpec{endpoint: "/search/issues", base: "is:pr", qualifiers: issueSearchQualifiers, sorts: issueSearchSorts, peer: "pull_requests"},
		comment: "pull requests across every repository of the owner (GitHub search; the issue view: no head, base or merge data)",
	},
	{
		name: "commits", keyCol: "sha", keys: []string{"repository", "sha"}, columns: ownerCommitColumns,
		params: []string{"q", "sort", "order"},
		search: &searchSpec{endpoint: searchCommits, base: "", qualifiers: map[string]qualifier{
			"author": eqQualifier("author"), "committer": eqQualifier("committer"), "author_name": eqQualifier("author-name"),
			"author_email": eqQualifier("author-email"), "author_date": rangeQualifier("author-date"), "committer_date": rangeQualifier("committer-date"),
		}, sorts: map[string]string{"author_date": "author-date", "committer_date": "committer-date"}},
		comment: "commits across every repository of the owner (GitHub search, which needs text: filter by message; at most 1000)",
	},
}

// ownerCollectionByName finds an owner-wide collection.
func ownerCollectionByName(name string) *collection {
	for _, c := range ownerCollections {
		if c.name == name {
			return c
		}
	}
	return nil
}

// ownerCollectionNames lists the owner-wide collections in display order.
func ownerCollectionNames() []string {
	out := make([]string, len(ownerCollections))
	for i, c := range ownerCollections {
		out[i] = c.name
	}
	return out
}

// knownCollection reports a name that is a collection in some scope.
func knownCollection(name string) bool {
	return collectionByName(name) != nil || ownerCollectionByName(name) != nil
}
