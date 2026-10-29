package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// parsed is a parseRequest that must succeed.
func parsed(t *testing.T, text string, readOnly bool) request {
	t.Helper()
	r, err := parseRequest(text, readOnly)
	if err != nil {
		t.Fatalf("parseRequest(%s): %v", text, err)
	}
	return r
}

func TestParseRequestList(t *testing.T) {
	t.Parallel()
	r := parsed(t, `{"collection":"issues"}`, false)
	if r.op != opList || r.collection != "issues" || len(r.params) != 0 || r.limit != 0 || r.repo != "" {
		t.Errorf("default list = %+v", r)
	}
	r = parsed(t, `{"op":"list","collection":"issues","repo":"octo/widgets","state":"open","since":"2024-01-01T00:00:00Z","limit":5}`, false)
	want := url.Values{"state": {"open"}, "since": {"2024-01-01T00:00:00Z"}}
	if !reflect.DeepEqual(r.params, want) || r.repo != "octo/widgets" || r.limit != 5 {
		t.Errorf("list = %+v, want params %v, repo octo/widgets, limit 5", r, want)
	}
	// Numbers and booleans become query text.
	r = parsed(t, `{"collection":"workflow_runs","check_suite_id":42,"exclude_pull_requests":true,"created":">=2024-01-01"}`, false)
	want = url.Values{"check_suite_id": {"42"}, "exclude_pull_requests": {"true"}, "created": {">=2024-01-01"}}
	if !reflect.DeepEqual(r.params, want) {
		t.Errorf("params = %v, want %v", r.params, want)
	}
	r = parsed(t, `{"collection":"branches","protected":false}`, false)
	if got := r.params.Get("protected"); got != "false" {
		t.Errorf("protected = %q, want false", got)
	}
	r = parsed(t, `{"collection":"commits","sha":"main","path":"docs/index.md","author":"octocat"}`, false)
	if r.params.Get("sha") != "main" || r.params.Get("path") != "docs/index.md" || r.params.Get("author") != "octocat" {
		t.Errorf("commit params = %v", r.params)
	}
}

func TestParseRequestGet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text string
		col  string
		key  string
	}{
		{`{"op":"get","collection":"issues","number":12}`, "issues", "12"},
		{`{"op":"get","collection":"pull_requests","number":"7"}`, "pull_requests", "7"},
		{`{"op":"get","collection":"milestones","number":3}`, "milestones", "3"},
		{`{"op":"get","collection":"commits","sha":"abc123"}`, "commits", "abc123"},
		{`{"op":"get","collection":"labels","name":"bug"}`, "labels", "bug"},
		{`{"op":"get","collection":"branches","name":"main"}`, "branches", "main"},
		{`{"op":"get","collection":"tags","name":"v1.0.0"}`, "tags", "v1.0.0"},
		{`{"op":"get","collection":"releases","id":99}`, "releases", "99"},
		{`{"op":"get","collection":"issue_comments","id":5}`, "issue_comments", "5"},
		{`{"op":"get","collection":"workflow_runs","id":123456}`, "workflow_runs", "123456"},
		{`{"op":"get","collection":"issues","key":"7"}`, "issues", "7"},
		{`{"op":"get","collection":"labels","key":"help wanted","repo":"octo/widgets"}`, "labels", "help wanted"},
	}
	for _, tc := range tests {
		r := parsed(t, tc.text, false)
		if r.op != opGet || r.collection != tc.col || r.key != tc.key {
			t.Errorf("%s = op %s, collection %s, key %q; want get, %s, %q", tc.text, r.op, r.collection, r.key, tc.col, tc.key)
		}
	}
}

func TestParseRequestSearch(t *testing.T) {
	t.Parallel()
	r := parsed(t, `{"op":"search","type":"issues","q":"is:open label:bug","sort":"created","order":"desc","limit":10}`, false)
	want := url.Values{"sort": {"created"}, "order": {"desc"}}
	if r.op != opSearch || r.searchType != "issues" || r.q != "is:open label:bug" || !reflect.DeepEqual(r.params, want) || r.limit != 10 {
		t.Errorf("search = %+v", r)
	}
	for _, typ := range searchTypes {
		r := parsed(t, `{"op":"search","type":"`+typ+`","q":"x"}`, false)
		if r.searchType != typ || len(r.params) != 0 {
			t.Errorf("search %s = %+v", typ, r)
		}
	}
}

func TestParseRequestRest(t *testing.T) {
	t.Parallel()
	r := parsed(t, `{"op":"rest","path":"/rate_limit"}`, false)
	if r.method != http.MethodGet || r.path != "/rate_limit" || r.body != nil || len(r.params) != 0 {
		t.Errorf("rest = %+v", r)
	}
	r = parsed(t, `{"op":"rest","method":"post","path":"/repos/octo/widgets/issues","body":{"title":"x"}}`, false)
	if r.method != http.MethodPost || string(r.body) != `{"title":"x"}` {
		t.Errorf("rest post = %+v", r)
	}
	for _, m := range []string{"patch", "PUT", "Delete"} {
		if r := parsed(t, `{"op":"rest","method":"`+m+`","path":"/x"}`, false); r.method != strings.ToUpper(m) {
			t.Errorf("method %s = %s", m, r.method)
		}
	}
	r = parsed(t, `{"op":"rest","path":"/search/code","params":{"q":"x y","per_page":5,"all":true,"ratio":0.5}}`, false)
	want := url.Values{"q": {"x y"}, "per_page": {"5"}, "all": {"true"}, "ratio": {"0.5"}}
	if !reflect.DeepEqual(r.params, want) {
		t.Errorf("params = %v, want %v", r.params, want)
	}
	r = parsed(t, `{"op":"rest","method":"DELETE","path":"/repos/o/r/labels/x","body":null}`, false)
	if string(r.body) != "null" {
		t.Errorf("delete body = %q", r.body)
	}
}

func TestParseRequestGraphQL(t *testing.T) {
	t.Parallel()
	r := parsed(t, `{"op":"graphql","query":"{ viewer { login } }","variables":{"n":1}}`, false)
	if r.op != opGraphQL || r.document != "{ viewer { login } }" || string(r.variables) != `{"n":1}` {
		t.Errorf("graphql = %+v", r)
	}
	r = parsed(t, `{"op":"graphql","query":"mutation { x }"}`, false)
	if r.variables != nil || r.document != "mutation { x }" {
		t.Errorf("graphql without variables = %+v", r)
	}
	// Without the read-only guard a mutation parses.
	if _, err := parseRequest(`{"op":"graphql","query":"mutation { addStar(input: {starrableId: \"x\"}) { clientMutationId } }"}`, false); err != nil {
		t.Errorf("mutation without read-only: %v", err)
	}
}

func TestParseRequestOtherOps(t *testing.T) {
	t.Parallel()
	if r := parsed(t, `{"op":"rate_limit"}`, false); r.op != opRateLimit {
		t.Errorf("rate_limit = %+v", r)
	}
	if r := parsed(t, `{"op":"repositories","limit":5}`, false); r.op != opRepositories || r.limit != 5 {
		t.Errorf("repositories = %+v", r)
	}
	if r := parsed(t, `{"op":"repositories"}`, false); r.op != opRepositories || r.limit != 0 {
		t.Errorf("repositories = %+v", r)
	}
}

func TestParseRequestRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		kind error
		hint string
		// notJSON marks a text that is not a JSON object at all: nothing
		// could run, so it is ErrSyntax under the read-only guard too.
		notJSON bool
	}{
		{"empty text", ``, driver.ErrSyntax, `one JSON object, such as {"collection":"issues","state":"open"}`, true},
		{"blank text", `  `, driver.ErrSyntax, "JSON object", true},
		{"SQL", `select 1`, driver.ErrSyntax, "the ops are list, get, search, repositories, rest, graphql and rate_limit", true},
		{"malformed JSON", `{"op":`, driver.ErrSyntax, "invalid JSON", false},
		{"array", `[]`, driver.ErrSyntax, "JSON object", true},
		{"string", `"list"`, driver.ErrSyntax, "JSON object", true},
		{"two objects", `{"collection":"issues"} {"collection":"labels"}`, driver.ErrSyntax, "nothing after it", false},
		{"trailing text", `{"collection":"issues"} x`, driver.ErrSyntax, "nothing after it", false},
		{"duplicate key", `{"op":"list","op":"rest"}`, driver.ErrSyntax, `duplicate key "op"`, false},
		{"trailing comma", `{"collection":"issues",}`, driver.ErrSyntax, "invalid JSON", false},
		{"op not a string", `{"op":1}`, driver.ErrSyntax, `"op" must be a string`, false},
		{"unknown op", `{"op":"delete"}`, driver.ErrSyntax, `unknown op "delete"`, false},
		{"list without a collection", `{}`, driver.ErrSyntax, `name the "collection"`, false},
		{"list with a null collection", `{"collection":null}`, driver.ErrSyntax, `"collection" must be a string`, false},
		{"unknown collection", `{"collection":"gists"}`, driver.ErrNotFound, `collection "gists" does not exist`, false},
		{"unknown collection on get", `{"op":"get","collection":"gists","id":1}`, driver.ErrNotFound, `collection "gists" does not exist`, false},
		{"unknown list member", `{"collection":"issues","color":"red"}`, driver.ErrSyntax, `unknown member "color" for op list`, false},
		{"another collection's parameter", `{"collection":"branches","state":"open"}`, driver.ErrSyntax, `unknown member "state"`, false},
		{"object parameter", `{"collection":"issues","state":{"a":1}}`, driver.ErrSyntax, `"state" must be a string, number or boolean`, false},
		{"array parameter", `{"collection":"issues","labels":["bug"]}`, driver.ErrSyntax, `"labels" must be a string, number or boolean`, false},
		{"null parameter", `{"collection":"issues","state":null}`, driver.ErrSyntax, `"state" must be a string, number or boolean`, false},
		{"repo with three segments", `{"collection":"issues","repo":"a/b/c"}`, driver.ErrSyntax, `"repo" must be owner/repo, or an owner`, false},
		{"repo that is not an owner", `{"collection":"issues","repo":"-bad"}`, driver.ErrSyntax, `"repo" must be owner/repo, or an owner`, false},
		{"repo with an empty name", `{"collection":"issues","repo":"octo/"}`, driver.ErrSyntax, `"repo" must be owner/repo, or an owner`, false},
		{"get repository not a string", `{"op":"get","collection":"issues","number":1,"repository":1}`, driver.ErrSyntax, `"repository" must be a string`, false},
		{"get full_name on issues", `{"op":"get","collection":"issues","full_name":"octo/widgets"}`, driver.ErrSyntax, `issues are keyed by "number", not "full_name"`, false},
		{"get number on repositories", `{"op":"get","collection":"repositories","number":1}`, driver.ErrSyntax, `repositories are keyed by "full_name", not "number"`, false},
		{"list repository parameter", `{"collection":"issues","repository":"alpha"}`, driver.ErrSyntax, `unknown member "repository" for op list`, false},
		{"get with an owner member", `{"op":"get","collection":"issues","number":1,"q":"x"}`, driver.ErrSyntax, `unknown member "q" for op get`, false},
		{"repo not a string", `{"collection":"issues","repo":1}`, driver.ErrSyntax, `"repo" must be a string`, false},
		{"negative limit", `{"collection":"issues","limit":-1}`, driver.ErrSyntax, `"limit" must be a whole number`, false},
		{"fractional limit", `{"collection":"issues","limit":1.5}`, driver.ErrSyntax, `"limit" must be a whole number`, false},
		{"text limit", `{"collection":"issues","limit":"5"}`, driver.ErrSyntax, `"limit" must be a whole number`, false},
		{"null limit", `{"collection":"issues","limit":null}`, driver.ErrSyntax, `"limit" must be a whole number`, false},
		{"get without a collection", `{"op":"get","number":1}`, driver.ErrSyntax, `name the "collection"`, false},
		{"get with the wrong key member", `{"op":"get","collection":"issues","id":12}`, driver.ErrSyntax, `issues are keyed by "number", not "id"`, false},
		{"get with sha on labels", `{"op":"get","collection":"labels","sha":"x"}`, driver.ErrSyntax, `labels are keyed by "name", not "sha"`, false},
		{"get with two keys", `{"op":"get","collection":"issues","key":1,"number":1}`, driver.ErrSyntax, `give the issue's "number"`, false},
		{"get without a key", `{"op":"get","collection":"releases"}`, driver.ErrSyntax, `give the release's "id"`, false},
		{"get with an empty key", `{"op":"get","collection":"labels","name":""}`, driver.ErrSyntax, `"name" must be a string or a number`, false},
		{"get with a null key", `{"op":"get","collection":"issues","number":null}`, driver.ErrSyntax, `"number" must be a string or a number`, false},
		{"get with an object key", `{"op":"get","collection":"issues","key":{"n":1}}`, driver.ErrSyntax, `"key" must be a string or a number`, false},
		{"get with a list parameter", `{"op":"get","collection":"issues","number":1,"state":"open"}`, driver.ErrSyntax, `unknown member "state" for op get`, false},
		{"search without a type", `{"op":"search","q":"x"}`, driver.ErrSyntax, `"type" must be one of`, false},
		{"search with an unknown type", `{"op":"search","type":"users","q":"x"}`, driver.ErrSyntax, `"type" must be one of`, false},
		{"search without q", `{"op":"search","type":"issues"}`, driver.ErrSyntax, `give the search "q"`, false},
		{"search with a blank q", `{"op":"search","type":"issues","q":"  "}`, driver.ErrSyntax, `give the search "q"`, false},
		{"search with a repo", `{"op":"search","type":"issues","q":"x","repo":"o/r"}`, driver.ErrSyntax, `unknown member "repo" for op search`, false},
		{"search sort not a string", `{"op":"search","type":"issues","q":"x","sort":1}`, driver.ErrSyntax, `"sort" must be a string`, false},
		{"rest with an unknown method", `{"op":"rest","method":"TRACE","path":"/x"}`, driver.ErrSyntax, `"method" must be one of`, false},
		{"rest without a path", `{"op":"rest"}`, driver.ErrSyntax, `must start with "/"`, false},
		{"rest path without a slash", `{"op":"rest","path":"rate_limit"}`, driver.ErrSyntax, `must start with "/"`, false},
		{"rest path with a scheme", `{"op":"rest","path":"https://evil.example.com/x"}`, driver.ErrSyntax, `must start with "/"`, false},
		{"rest path with an embedded scheme", `{"op":"rest","path":"/x/https://evil.example.com"}`, driver.ErrSyntax, "no scheme, host, query or fragment", false},
		{"rest path with a query", `{"op":"rest","path":"/x?per_page=1"}`, driver.ErrSyntax, "no scheme, host, query or fragment", false},
		{"rest path with a fragment", `{"op":"rest","path":"/x#y"}`, driver.ErrSyntax, "no scheme, host, query or fragment", false},
		{"rest path with a parent segment", `{"op":"rest","path":"/repos/../admin"}`, driver.ErrSyntax, "empty, . or .. segments", false},
		{"rest path ending in a parent segment", `{"op":"rest","path":"/repos/.."}`, driver.ErrSyntax, "empty, . or .. segments", false},
		{"rest path with an empty segment", `{"op":"rest","path":"/repos//octo"}`, driver.ErrSyntax, "empty, . or .. segments", false},
		{"rest path with whitespace", `{"op":"rest","path":"/repos/o ctopus"}`, driver.ErrSyntax, "whitespace", false},
		{"rest path with a newline", `{"op":"rest","path":"/repos\n"}`, driver.ErrSyntax, "whitespace", false},
		{"rest params not an object", `{"op":"rest","path":"/x","params":"a=b"}`, driver.ErrSyntax, `"params" must be an object`, false},
		{"rest params with an array", `{"op":"rest","path":"/x","params":{"a":[1]}}`, driver.ErrSyntax, `"params" must be an object`, false},
		{"rest params with a null", `{"op":"rest","path":"/x","params":{"a":null}}`, driver.ErrSyntax, `"params" must be an object`, false},
		{"rest body on a GET", `{"op":"rest","path":"/x","body":{"a":1}}`, driver.ErrSyntax, "a GET takes no body", false},
		{"rest with a collection", `{"op":"rest","path":"/x","collection":"issues"}`, driver.ErrSyntax, `unknown member "collection" for op rest`, false},
		{"graphql without a document", `{"op":"graphql"}`, driver.ErrSyntax, `give the GraphQL "query"`, false},
		{"graphql with a blank document", `{"op":"graphql","query":" "}`, driver.ErrSyntax, `give the GraphQL "query"`, false},
		{"graphql document not a string", `{"op":"graphql","query":{"a":1}}`, driver.ErrSyntax, `"query" must be a string`, false},
		{"graphql variables as an array", `{"op":"graphql","query":"{ a }","variables":[1]}`, driver.ErrSyntax, `"variables" must be an object`, false},
		{"graphql variables as a string", `{"op":"graphql","query":"{ a }","variables":"x"}`, driver.ErrSyntax, `"variables" must be an object`, false},
		{"graphql variables as null", `{"op":"graphql","query":"{ a }","variables":null}`, driver.ErrSyntax, `"variables" must be an object`, false},
		{"graphql with a path", `{"op":"graphql","query":"{ a }","path":"/x"}`, driver.ErrSyntax, `unknown member "path" for op graphql`, false},
		{"rate_limit with a member", `{"op":"rate_limit","limit":1}`, driver.ErrSyntax, `unknown member "limit" for op rate_limit`, false},
		{"repositories with a collection", `{"op":"repositories","collection":"issues"}`, driver.ErrSyntax, `unknown member "collection" for op repositories`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseRequest(tc.text, false)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("parseRequest = %v, want %v", err, tc.kind)
			}
			if !strings.Contains(err.Error(), tc.hint) {
				t.Errorf("message %q does not mention %q", err, tc.hint)
			}
			var de *driver.Error
			if !errors.As(err, &de) || de.Driver != Name || de.Op != "query" {
				t.Errorf("attribution = %+v, want github/query", de)
			}
			// Read-only: a syntax refusal fails closed as ErrReadOnly; a
			// missing collection stays ErrNotFound, and a text that is not a
			// JSON object stays ErrSyntax with the same hint.
			_, roErr := parseRequest(tc.text, true)
			switch {
			case tc.notJSON:
				if !errors.Is(roErr, driver.ErrSyntax) || errors.Is(roErr, driver.ErrReadOnly) || roErr.Error() != err.Error() {
					t.Errorf("read-only parseRequest = %v, want the same ErrSyntax as %v", roErr, err)
				}
			case tc.kind == driver.ErrNotFound:
				if !errors.Is(roErr, driver.ErrNotFound) || errors.Is(roErr, driver.ErrReadOnly) {
					t.Errorf("read-only parseRequest = %v, want ErrNotFound", roErr)
				}
			default:
				var roDe *driver.Error
				if !errors.Is(roErr, driver.ErrReadOnly) || errors.Is(roErr, driver.ErrSyntax) || !errors.As(roErr, &roDe) {
					t.Fatalf("read-only parseRequest = %v, want ErrReadOnly", roErr)
				}
				if !strings.HasPrefix(roDe.Err.Error(), "read-only: ") {
					t.Errorf("read-only message %q lacks the read-only prefix", roDe.Err)
				}
			}
		})
	}
}

func TestParseRequestReadOnly(t *testing.T) {
	t.Parallel()
	// Writes are refused.
	for _, text := range []string{
		`{"op":"rest","method":"POST","path":"/repos/o/r/issues","body":{"title":"x"}}`,
		`{"op":"rest","method":"patch","path":"/repos/o/r/issues/1","body":{"state":"closed"}}`,
		`{"op":"rest","method":"PUT","path":"/x"}`,
		`{"op":"rest","method":"DELETE","path":"/repos/o/r/labels/x"}`,
		`{"op":"graphql","query":"mutation { addStar(input: {starrableId: \"x\"}) { clientMutationId } }"}`,
		`{"op":"graphql","query":"subscription { x }"}`,
		`{"op":"graphql","query":"query { a } mutation { b }"}`,
		`{"op":"graphql","query":"fragment F on Repository { name } query { viewer { login } }"}`,
	} {
		_, err := parseRequest(text, true)
		var de *driver.Error
		if !errors.Is(err, driver.ErrReadOnly) || !errors.As(err, &de) {
			t.Errorf("read-only %s = %v, want ErrReadOnly", text, err)
		} else if !strings.HasPrefix(de.Err.Error(), "read-only: ") {
			t.Errorf("read-only %s message %q lacks the prefix", text, de.Err)
		}
		// The same text passes without the guard, so it is the guard refusing.
		if _, err := parseRequest(text, false); err != nil {
			t.Errorf("without the guard %s = %v, want no error", text, err)
		}
	}
	// Reads parse the same with and without the guard.
	for _, text := range []string{
		`{"collection":"issues","state":"open","limit":3}`,
		`{"op":"get","collection":"labels","name":"bug"}`,
		`{"op":"search","type":"repositories","q":"tobira in:name","sort":"stars"}`,
		`{"op":"repositories"}`,
		`{"op":"rest","path":"/rate_limit","params":{"a":1}}`,
		`{"op":"rest","method":"get","path":"/user"}`,
		`{"op":"graphql","query":"{ viewer { login } }","variables":{"a":1}}`,
		`{"op":"graphql","query":"query Repo($o: String!) { repository(owner: $o, name: \"mutation\") { id } }","variables":{"o":"octo"}}`,
		`{"op":"rate_limit"}`,
	} {
		open := parsed(t, text, false)
		guarded := parsed(t, text, true)
		if !reflect.DeepEqual(open, guarded) {
			t.Errorf("read-only changed the parse of %s:\n%+v\n%+v", text, open, guarded)
		}
	}
}

func TestRowCap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reqLimit, queryLimit int
		want                 int
		warns                bool
	}{
		{0, 0, defaultListRows, false},
		{50, 0, 50, false},
		{maxListRows, 0, maxListRows, false},
		{1500, 0, maxListRows, true},
		{0, 10, 10, false},
		{50, 10, 10, false},
		{50, 200, 50, false},
		{1500, 20, 20, true},
		{10, 10, 10, false},
	}
	for _, tc := range tests {
		got, warning := rowCap(request{limit: tc.reqLimit}, driver.Query{Limit: tc.queryLimit})
		if got != tc.want || (warning != "") != tc.warns {
			t.Errorf("rowCap(%d, %d) = %d, %q; want %d, warning %v", tc.reqLimit, tc.queryLimit, got, warning, tc.want, tc.warns)
		}
		if tc.warns && (!strings.Contains(warning, "1500") || !strings.Contains(warning, "1000") || !strings.Contains(warning, "10 requests")) {
			t.Errorf("warning %q should name the cut, the cap and the requests", warning)
		}
	}
}

func TestScalarText(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		`"a b"`: "a b", `""`: "", `12`: "12", `-7`: "-7", `1.5`: "1.5", `1e2`: "100", `0.1`: "0.1",
		`true`: "true", `false`: "false", `99999999999999999999`: "99999999999999999999", ` 3 `: "3",
	} {
		got, err := scalarText(json.RawMessage(raw))
		if err != nil || got != want {
			t.Errorf("scalarText(%s) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{`null`, `[1]`, `{}`, `{"a":1}`, ``, `nope`} {
		if got, err := scalarText(json.RawMessage(raw)); err == nil {
			t.Errorf("scalarText(%s) = %q, want an error", raw, got)
		}
	}
}

func TestTopLevelObject(t *testing.T) {
	t.Parallel()
	fields, err := topLevelObject(` {"a": 1, "b": {"c": [1, 2]}, "d": "x"} `)
	if err != nil {
		t.Fatalf("topLevelObject: %v", err)
	}
	if len(fields) != 3 || string(fields["a"]) != "1" || string(fields["b"]) != `{"c": [1, 2]}` || string(fields["d"]) != `"x"` {
		t.Errorf("fields = %v", fields)
	}
	fields, err = topLevelObject(`{}`)
	if err != nil || len(fields) != 0 {
		t.Errorf("empty object = %v, %v", fields, err)
	}
	for text, hint := range map[string]string{
		``:                     "JSON object",
		`[]`:                   "JSON object",
		`1`:                    "JSON object",
		`"s"`:                  "JSON object",
		`{"a":1} {"b":2}`:      "nothing after it",
		`{"a":1} trailing`:     "nothing after it",
		`{"a":1}}`:             "nothing after it",
		`{"a":1,"a":2}`:        `duplicate key "a"`,
		`{"a":1,"b":2,"a":1}`:  `duplicate key "a"`,
		`{"a":`:                "invalid JSON",
		`{"a":1,}`:             "invalid JSON",
		`{"a" 1}`:              "invalid JSON",
		`{"a":1 "b":2}`:        "invalid JSON",
		`{"a":tru}`:            "invalid JSON",
		"{\"a\":\"\x01\"}":     "invalid JSON",
		`{"a":1,"b":[1,2}`:     "invalid JSON",
		`{"a":{"b":1}, "c":}`:  "invalid JSON",
		`{"a":1, "b":2, "c":]`: "invalid JSON",
	} {
		_, err := topLevelObject(text)
		if err == nil {
			t.Errorf("topLevelObject(%q) accepted, want an error", text)
			continue
		}
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("topLevelObject(%q) = %v, want %q", text, err, hint)
		}
	}
}

func TestStrictDecode(t *testing.T) {
	t.Parallel()
	var n int
	if err := strictDecode(json.RawMessage(` 5 `), &n); err != nil || n != 5 {
		t.Errorf("int = %d, %v", n, err)
	}
	for _, raw := range []string{`"5"`, `null`, `1.5`, `true`, `[5]`, ``} {
		var n int
		if err := strictDecode(json.RawMessage(raw), &n); err == nil {
			t.Errorf("strictDecode(%s) into int accepted", raw)
		}
	}
	var m map[string]json.RawMessage
	if err := strictDecode(json.RawMessage(`{"a":1}`), &m); err != nil || string(m["a"]) != "1" {
		t.Errorf("map = %v, %v", m, err)
	}
	for _, raw := range []string{`"x"`, `[1]`, `null`, `1`} {
		var m map[string]json.RawMessage
		if err := strictDecode(json.RawMessage(raw), &m); err == nil {
			t.Errorf("strictDecode(%s) into a map accepted", raw)
		}
	}
	var s struct {
		A int `json:"a"`
	}
	if err := strictDecode(json.RawMessage(`{"a":1,"b":2}`), &s); err == nil {
		t.Error("an unknown struct member was accepted")
	}
}

func TestDecodeString(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{`"x"`: "x", ` "y" `: "y", `""`: "", `"a\"b"`: `a"b`, `"\u00e9"`: "é"} {
		var s string
		if err := decodeString(json.RawMessage(raw), &s); err != nil || s != want {
			t.Errorf("decodeString(%s) = %q, %v; want %q", raw, s, err, want)
		}
	}
	for _, raw := range []string{`1`, `null`, `true`, `["x"]`, `{"a":"x"}`, ``, `  `, `"unterminated`} {
		var s string
		if err := decodeString(json.RawMessage(raw), &s); err == nil {
			t.Errorf("decodeString(%s) accepted as %q", raw, s)
		}
	}
}
