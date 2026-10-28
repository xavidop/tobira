package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// The query ops.
const (
	opList         = "list"
	opGet          = "get"
	opSearch       = "search"
	opRepositories = "repositories"
	opRest         = "rest"
	opGraphQL      = "graphql"
	opRateLimit    = "rate_limit"
)

const (
	// defaultListRows and maxListRows bound the rows a list, search, rest or
	// repositories query returns (maxListRows is 10 requests of 100, and
	// GitHub's own cap on search results).
	defaultListRows = 100
	maxListRows     = 1000
)

// opKeys are the members each op accepts, besides the collection's own
// parameters for list.
var opKeys = map[string][]string{
	opList:         {"collection", "repo", "limit"},
	opGet:          {"collection", "repo", "key", "number", "id", "sha", "name"},
	opSearch:       {"type", "q", "sort", "order", "limit"},
	opRepositories: {"limit"},
	opRest:         {"method", "path", "params", "body", "limit"},
	opGraphQL:      {"query", "variables"},
	opRateLimit:    {},
}

// searchTypes are the search endpoints a search query may name.
var searchTypes = []string{"issues", "pull_requests", "commits", "repositories"}

// restMethods are the methods a rest query may use.
var restMethods = []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete}

// request is a parsed native query.
type request struct {
	op         string
	repo       string // owner/repo, "" = the query's namespace or the DSN's
	collection *collection
	params     url.Values // list: the endpoint's parameters
	key        string     // get
	limit      int        // rows cap; 0 = default
	// search
	searchType string
	q          string
	// rest
	method string
	path   string
	body   json.RawMessage
	// graphql
	document  string
	variables json.RawMessage
}

// parseRequest parses a native query: a JSON object with an "op" (list,
// the default, get, search, repositories, rest, graphql or rate_limit) and
// the op's members. Any other member, a duplicate key or trailing text is
// refused before a byte is sent. With readOnly, a rest method other than
// GET and a GraphQL document that is not plainly a query are ErrReadOnly,
// and so is every refusal of a text the guard cannot read (fail closed);
// otherwise a malformed text is ErrSyntax.
func parseRequest(text string, readOnly bool) (request, error) {
	refuse := func(kind error, format string, args ...any) (request, error) {
		msg := fmt.Sprintf(format, args...)
		if readOnly && kind != driver.ErrReadOnly && kind != driver.ErrNotFound {
			kind, msg = driver.ErrReadOnly, "read-only: "+msg
		}
		return request{}, driver.Wrap(kind, Name, "query", errors.New(msg))
	}
	fields, err := topLevelObject(text)
	if err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	r := request{op: opList}
	if raw, ok := fields["op"]; ok {
		if err := decodeString(raw, &r.op); err != nil {
			return refuse(driver.ErrSyntax, `"op" must be a string`)
		}
		delete(fields, "op")
	}
	allowed, ok := opKeys[r.op]
	if !ok {
		return refuse(driver.ErrSyntax, "unknown op %q: the ops are list, get, search, repositories, rest, graphql and rate_limit", r.op)
	}
	str := func(k string, dst *string) error {
		if raw, ok := fields[k]; ok {
			if err := decodeString(raw, dst); err != nil {
				return fmt.Errorf("%q must be a string", k)
			}
		}
		return nil
	}
	var cname string
	if err := errors.Join(str("repo", &r.repo), str("collection", &cname), str("type", &r.searchType), str("q", &r.q),
		str("method", &r.method), str("path", &r.path), str("query", &r.document)); err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	if r.repo != "" {
		if _, _, ok := splitRepo(r.repo); !ok {
			return refuse(driver.ErrSyntax, `"repo" must be owner/repo`)
		}
	}
	if raw, ok := fields["limit"]; ok {
		if err := strictDecode(raw, &r.limit); err != nil || r.limit < 0 {
			return refuse(driver.ErrSyntax, `"limit" must be a whole number, 0 or more`)
		}
	}
	if _, ok := fields["collection"]; ok {
		r.collection = collectionByName(cname)
		if r.collection == nil {
			return refuse(driver.ErrNotFound, "collection %q does not exist (the collections are %s)", cname, strings.Join(collectionNames(), ", "))
		}
	}
	for k := range fields {
		if slices.Contains(allowed, k) {
			continue
		}
		if r.op == opList && r.collection != nil && slices.Contains(r.collection.params, k) {
			continue
		}
		return refuse(driver.ErrSyntax, "unknown member %q for op %s", k, r.op)
	}
	switch r.op {
	case opList:
		if r.collection == nil {
			return refuse(driver.ErrSyntax, `name the "collection" to list (%s)`, strings.Join(collectionNames(), ", "))
		}
		r.params = url.Values{}
		for _, k := range r.collection.params {
			raw, ok := fields[k]
			if !ok {
				continue
			}
			v, err := scalarText(raw)
			if err != nil {
				return refuse(driver.ErrSyntax, "%q must be a string, number or boolean", k)
			}
			r.params.Set(k, v)
		}
	case opGet:
		if r.collection == nil {
			return refuse(driver.ErrSyntax, `name the "collection" to read from`)
		}
		given := 0
		for _, k := range []string{"key", "number", "id", "sha", "name"} {
			raw, ok := fields[k]
			if !ok {
				continue
			}
			if k != "key" && k != r.collection.keyCol {
				return refuse(driver.ErrSyntax, "%s are keyed by %q, not %q", r.collection.name, r.collection.keyCol, k)
			}
			v, err := scalarText(raw)
			if err != nil || v == "" {
				return refuse(driver.ErrSyntax, "%q must be a string or a number", k)
			}
			r.key = v
			given++
		}
		if given != 1 {
			return refuse(driver.ErrSyntax, "give the %s's %q", strings.TrimSuffix(r.collection.name, "s"), r.collection.keyCol)
		}
	case opSearch:
		if !slices.Contains(searchTypes, r.searchType) {
			return refuse(driver.ErrSyntax, `"type" must be one of %s`, strings.Join(searchTypes, ", "))
		}
		if strings.TrimSpace(r.q) == "" {
			return refuse(driver.ErrSyntax, `give the search "q", in GitHub's search syntax`)
		}
		r.params = url.Values{}
		for _, k := range []string{"sort", "order"} {
			var v string
			if err := str(k, &v); err != nil {
				return refuse(driver.ErrSyntax, "%v", err)
			}
			if v != "" {
				r.params.Set(k, v)
			}
		}
	case opRest:
		if r.method == "" {
			r.method = http.MethodGet
		}
		r.method = strings.ToUpper(r.method)
		if !slices.Contains(restMethods, r.method) {
			return refuse(driver.ErrSyntax, `"method" must be one of %s`, strings.Join(restMethods, ", "))
		}
		if readOnly && r.method != http.MethodGet {
			return refuse(driver.ErrReadOnly, "read-only: a rest %s is refused; only GET runs", r.method)
		}
		if err := checkPath(r.path); err != nil {
			return refuse(driver.ErrSyntax, "%v", err)
		}
		r.params = url.Values{}
		if raw, ok := fields["params"]; ok {
			var params map[string]json.RawMessage
			if err := strictDecode(raw, &params); err != nil {
				return refuse(driver.ErrSyntax, `"params" must be an object of strings, numbers or booleans`)
			}
			for k, v := range params {
				s, err := scalarText(v)
				if err != nil {
					return refuse(driver.ErrSyntax, `"params" must be an object of strings, numbers or booleans`)
				}
				r.params.Set(k, s)
			}
		}
		if raw, ok := fields["body"]; ok {
			if r.method == http.MethodGet {
				return refuse(driver.ErrSyntax, "a GET takes no body: give parameters in params")
			}
			r.body = raw
		}
	case opGraphQL:
		if strings.TrimSpace(r.document) == "" {
			return refuse(driver.ErrSyntax, `give the GraphQL "query" document`)
		}
		if raw, ok := fields["variables"]; ok {
			if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
				return refuse(driver.ErrSyntax, `"variables" must be an object`)
			}
			r.variables = raw
		}
		if readOnly {
			if err := classifyGraphQL(r.document); err != nil {
				return refuse(driver.ErrReadOnly, "read-only: %v", err)
			}
		}
	}
	return r, nil
}

// scalarText renders a JSON string, number or boolean as a query parameter.
func scalarText(raw json.RawMessage) (string, error) {
	v := anyValue(raw)
	switch v.Type() {
	case driver.TypeString, driver.TypeInt, driver.TypeDecimal:
		s, _ := text(v)
		return s, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return strconv.FormatBool(b), nil
	}
	return "", errors.New("not a scalar")
}

func decodeString(raw json.RawMessage, dst *string) error {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '"' {
		return errors.New("not a string")
	}
	return json.Unmarshal(raw, dst)
}

// strictDecode decodes raw into dst, refusing null and type mismatches.
func strictDecode(raw json.RawMessage, dst any) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("null")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// topLevelObject decodes text as exactly one JSON object and returns its
// members. A duplicate key is an error: encoding/json would keep the last
// one, so {"op":"list","op":"rest"} must not be read as either.
func topLevelObject(text string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	tok, err := dec.Token()
	if err != nil {
		return nil, errors.New("the request must be a JSON object")
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("the request must be a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		key, _ := tok.(string)
		if _, dup := fields[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		fields[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("the request must be one JSON object, with nothing after it")
	}
	return fields, nil
}

// rowCap is the rows a query returns: its own limit (default
// defaultListRows, at most maxListRows), cut further by Query.Limit.
func rowCap(r request, q driver.Query) (limit int, warning string) {
	limit = r.limit
	switch {
	case limit == 0:
		limit = defaultListRows
	case limit > maxListRows:
		warning = fmt.Sprintf(`"limit" was cut from %d to %d rows (%d requests)`, limit, maxListRows, maxListRows/maxPerPage)
		limit = maxListRows
	}
	if q.Limit > 0 && q.Limit < limit {
		limit = q.Limit
	}
	return limit, warning
}

// Query runs a native request (see parseRequest).
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	const op = "query"
	start := time.Now()
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if len(q.Params) > 0 {
		return nil, syntaxErr(op, "github queries take no positional parameters")
	}
	r, err := parseRequest(q.Text, c.readOnly || q.ReadOnly)
	if err != nil {
		return nil, err
	}
	var (
		cols     []driver.Column
		rows     []driver.Row
		stats    = driver.Stats{RowsAffected: -1}
		warnings []string
	)
	switch r.op {
	case opList:
		cols, rows, stats, err = c.runList(ctx, op, r, q)
	case opGet:
		cols, rows, err = c.runGet(ctx, op, r, q)
	case opSearch:
		cols, rows, stats, err = c.runSearch(ctx, op, r, q)
	case opRepositories:
		cols, rows, stats, err = c.runRepositories(ctx, op, r, q)
	case opRest:
		cols, rows, stats, err = c.runRest(ctx, op, r, q)
	case opGraphQL:
		cols, rows, warnings, err = c.runGraphQL(ctx, op, r)
	case opRateLimit:
		cols, rows, err = c.runRateLimit(ctx, op)
	}
	if err != nil {
		return nil, err
	}
	stats.RowsAffected = -1
	stats.Duration = time.Since(start)
	stats.Warnings = append(stats.Warnings, warnings...)
	if n := c.client.rateNotice(); n != "" {
		stats.Notices = append(stats.Notices, n)
	}
	return driver.NewSliceResultSet(cols, rows, stats), nil
}

// collect reads pages of a list request until limit rows, the last page
// or maxRequests requests. more reports rows left unread; warning says
// when the request cap stopped the read.
func (c *conn) collect(ctx context.Context, op string, req listRequest, limit int) (rows []driver.Row, more bool, warning string, err error) {
	for page := 1; len(rows) < limit; page++ {
		items, next, err := c.fetchPage(ctx, op, req, page)
		if err != nil {
			return nil, false, "", err
		}
		for _, it := range items {
			if len(rows) == limit {
				return rows, true, "", nil
			}
			row, err := req.col.row(it)
			if err != nil {
				return nil, false, "", c.fail(ctx, op, err)
			}
			rows = append(rows, row)
		}
		if !next {
			break
		}
		if len(rows) == limit {
			return rows, true, "", nil
		}
		if page >= maxRequests {
			return rows, true, fmt.Sprintf("the listing stopped after %d requests (%d rows); narrow it with the endpoint's parameters", maxRequests, len(rows)), nil
		}
	}
	return rows, false, "", nil
}

// runList lists a collection with the endpoint's own parameters.
func (c *conn) runList(ctx context.Context, op string, r request, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats, error) {
	owner, repo, err := c.resolveRepo(op, firstNonEmpty(r.repo, q.Namespace))
	if err != nil {
		return nil, nil, driver.Stats{}, err
	}
	if _, err := c.repos.get(ctx, op, owner, repo); err != nil {
		return nil, nil, driver.Stats{}, err
	}
	limit, warning := rowCap(r, q)
	query := url.Values{}
	for k, v := range r.collection.defaults {
		query[k] = v
	}
	for k, v := range r.params {
		query[k] = v
	}
	perPage := min(limit, maxPerPage)
	if r.collection.dropPullRequests {
		perPage = maxPerPage // pull requests are dropped after the read: read full pages
	}
	req := listRequest{col: r.collection, owner: owner, repo: repo, query: query, perPage: perPage}
	rows, more, stopped, err := c.collect(ctx, op, req, limit)
	if err != nil {
		return nil, nil, driver.Stats{}, err
	}
	stats := driver.Stats{Truncated: more}
	for _, w := range []string{warning, stopped} {
		if w != "" {
			stats.Warnings = append(stats.Warnings, w)
		}
	}
	return r.collection.resultColumns(driver.CollectionRef{Namespace: owner + "/" + repo, Name: r.collection.name}), rows, stats, nil
}

// runGet reads one item by key. Tags have no single-item endpoint: the
// listing is searched instead.
func (c *conn) runGet(ctx context.Context, op string, r request, q driver.Query) ([]driver.Column, []driver.Row, error) {
	owner, repo, err := c.resolveRepo(op, firstNonEmpty(r.repo, q.Namespace))
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.repos.get(ctx, op, owner, repo); err != nil {
		return nil, nil, err
	}
	ref := driver.CollectionRef{Namespace: owner + "/" + repo, Name: r.collection.name}
	row, err := c.getItem(ctx, op, r.collection, owner, repo, r.key)
	if err != nil {
		return nil, nil, err
	}
	return r.collection.resultColumns(ref), []driver.Row{row}, nil
}

// getItem reads one item of a collection by key.
func (c *conn) getItem(ctx context.Context, op string, col *collection, owner, repo, key string) (driver.Row, error) {
	if col.get != nil {
		raw, err := c.client.getJSON(ctx, col.get(owner, repo, key), nil)
		if err != nil {
			return nil, c.fail(ctx, op, err)
		}
		row, err := col.row(raw)
		if err != nil {
			return nil, c.fail(ctx, op, err)
		}
		if col.dropPullRequests && isPullRequest(raw) {
			return nil, notFoundErr(op, "%s is a pull request: read it from pull_requests", key)
		}
		return row, nil
	}
	req := listRequest{col: col, owner: owner, repo: repo, query: url.Values{}, perPage: maxPerPage}
	keyIdx := indexOf(col.columns, col.keyCol)
	for page := 1; page <= maxListRows/maxPerPage; page++ {
		items, next, err := c.fetchPage(ctx, op, req, page)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			row, err := col.row(it)
			if err != nil {
				return nil, c.fail(ctx, op, err)
			}
			if s, ok := text(row[keyIdx]); ok && s == key {
				return row, nil
			}
		}
		if !next {
			break
		}
	}
	return nil, notFoundErr(op, "no %s %q in the first %d", strings.TrimSuffix(col.name, "s"), key, maxListRows)
}

// runSearch runs a GitHub search. Results are capped by GitHub at 1000.
func (c *conn) runSearch(ctx context.Context, op string, r request, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats, error) {
	limit, warning := rowCap(r, q)
	endpoint, text := "/search/"+r.searchType, r.q
	colsDef := searchColumns(r.searchType)
	if r.searchType == "pull_requests" {
		endpoint, text, colsDef = "/search/issues", r.q+" is:pr", searchColumns("issues")
	}
	var rows []driver.Row
	stats := driver.Stats{}
	if warning != "" {
		stats.Warnings = append(stats.Warnings, warning)
	}
	var total int64
	incomplete := false
	for page := 1; len(rows) < limit; page++ {
		query := url.Values{"q": {text}, "per_page": {strconv.Itoa(min(limit, maxPerPage))}, "page": {strconv.Itoa(page)}}
		for k, v := range r.params {
			query[k] = v
		}
		resp, err := c.client.do(ctx, http.MethodGet, c.client.url(endpoint, query), nil)
		if err != nil {
			return nil, nil, driver.Stats{}, c.fail(ctx, op, err)
		}
		var body struct {
			Total      int64             `json:"total_count"`
			Incomplete bool              `json:"incomplete_results"`
			Items      []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(resp.body, &body); err != nil {
			return nil, nil, driver.Stats{}, c.fail(ctx, op, err)
		}
		total, incomplete = body.Total, incomplete || body.Incomplete
		for _, it := range body.Items {
			if len(rows) == limit {
				break
			}
			obj, err := objectOf(it)
			if err != nil {
				return nil, nil, driver.Stats{}, c.fail(ctx, op, err)
			}
			row := make(driver.Row, len(colsDef))
			for i, col := range colsDef {
				row[i] = col.value(obj)
			}
			rows = append(rows, row)
		}
		if !resp.hasNext() || len(body.Items) == 0 {
			break
		}
	}
	stats.Truncated = total > int64(len(rows))
	if incomplete {
		stats.Warnings = append(stats.Warnings, "GitHub reports incomplete results: the search timed out before covering everything")
	}
	if total > maxListRows {
		stats.Notices = append(stats.Notices, fmt.Sprintf("%d matches; GitHub returns at most %d", total, maxListRows))
	}
	cols := make([]driver.Column, len(colsDef))
	for i, col := range colsDef {
		cols[i] = driver.Column{Name: col.name, NativeType: col.native, Type: col.typ}
	}
	return cols, rows, stats, nil
}

// runRepositories lists the connection's repositories with their details.
func (c *conn) runRepositories(ctx context.Context, op string, r request, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats, error) {
	limit, warning := rowCap(r, q)
	stats := driver.Stats{}
	if warning != "" {
		stats.Warnings = append(stats.Warnings, warning)
	}
	var rows []driver.Row
	if c.dsn.repo != "" {
		raw, err := c.client.getJSON(ctx, "/repos/"+url.PathEscape(c.dsn.owner)+"/"+url.PathEscape(c.dsn.repo), nil)
		if err != nil {
			return nil, nil, driver.Stats{}, c.fail(ctx, op, err)
		}
		row, err := rowFrom(repositoryColumns, raw)
		if err != nil {
			return nil, nil, driver.Stats{}, c.fail(ctx, op, err)
		}
		rows = append(rows, row)
	} else {
		path, query, err := c.repoListing(ctx, op)
		if err != nil {
			return nil, nil, driver.Stats{}, err
		}
		for page := 1; len(rows) < limit; page++ {
			pq := url.Values{}
			for k, v := range query {
				pq[k] = v
			}
			pq.Set("per_page", strconv.Itoa(min(limit, maxPerPage)))
			pq.Set("page", strconv.Itoa(page))
			items, next, err := c.client.page(ctx, path, pq, "")
			if err != nil {
				return nil, nil, driver.Stats{}, c.fail(ctx, op, err)
			}
			for _, it := range items {
				if len(rows) == limit {
					stats.Truncated = true
					break
				}
				row, err := rowFrom(repositoryColumns, it)
				if err != nil {
					return nil, nil, driver.Stats{}, c.fail(ctx, op, err)
				}
				rows = append(rows, row)
			}
			if next && len(rows) == limit {
				stats.Truncated = true
			}
			if !next || stats.Truncated {
				break
			}
		}
	}
	cols := make([]driver.Column, len(repositoryColumns))
	for i, col := range repositoryColumns {
		cols[i] = driver.Column{Name: col.name, NativeType: col.native, Type: col.typ, Key: col.key}
	}
	return cols, rows, stats, nil
}

// rowFrom reads one item with a column set.
func rowFrom(cols []column, item json.RawMessage) (driver.Row, error) {
	obj, err := objectOf(item)
	if err != nil {
		return nil, err
	}
	row := make(driver.Row, len(cols))
	for i, col := range cols {
		row[i] = col.value(obj)
	}
	return row, nil
}

// runRest sends one REST call. An array answer is one row per element,
// following GitHub's pagination up to the limit; an object is one row; a
// scalar is one "value" cell; no content is no rows.
func (c *conn) runRest(ctx context.Context, op string, r request, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats, error) {
	limit, warning := rowCap(r, q)
	stats := driver.Stats{}
	if warning != "" {
		stats.Warnings = append(stats.Warnings, warning)
	}
	var body any
	if r.body != nil {
		body = r.body
	}
	var items []json.RawMessage
	query := url.Values{}
	for k, v := range r.params {
		query[k] = v
	}
	paginate := r.method == http.MethodGet && query.Get("page") == ""
	if paginate && query.Get("per_page") == "" {
		query.Set("per_page", strconv.Itoa(min(limit, maxPerPage)))
	}
	for page := 1; ; page++ {
		if paginate && page > 1 {
			query.Set("page", strconv.Itoa(page))
		}
		resp, err := c.client.do(ctx, r.method, c.client.url(r.path, query), body)
		if err != nil {
			return nil, nil, driver.Stats{}, c.fail(ctx, op, err)
		}
		if resp.status == http.StatusNoContent || len(bytes.TrimSpace(resp.body)) == 0 {
			stats.Notices = append(stats.Notices, fmt.Sprintf("%d %s", resp.status, http.StatusText(resp.status)))
			return []driver.Column{}, nil, stats, nil
		}
		t := bytes.TrimSpace(resp.body)
		if t[0] != '[' {
			if page > 1 {
				break
			}
			var cols []driver.Column
			var row driver.Row
			if t[0] == '{' {
				obj, err := objectOf(resp.body)
				if err != nil {
					return nil, nil, driver.Stats{}, c.fail(ctx, op, err)
				}
				cols, row = objectRow(obj)
			} else {
				cols, row = []driver.Column{{Name: "value", Type: anyValue(resp.body).Type()}}, driver.Row{anyValue(resp.body)}
			}
			if resp.status != http.StatusOK {
				stats.Notices = append(stats.Notices, fmt.Sprintf("%d %s", resp.status, http.StatusText(resp.status)))
			}
			return cols, []driver.Row{row}, stats, nil
		}
		var pageItems []json.RawMessage
		if err := json.Unmarshal(resp.body, &pageItems); err != nil {
			return nil, nil, driver.Stats{}, c.fail(ctx, op, err)
		}
		for _, it := range pageItems {
			if len(items) == limit {
				stats.Truncated = true
				break
			}
			items = append(items, it)
		}
		if stats.Truncated || !paginate || !resp.hasNext() || len(pageItems) == 0 {
			stats.Truncated = stats.Truncated || paginate && resp.hasNext() && len(items) >= limit
			break
		}
	}
	cols, rows := unionRows(items)
	return cols, rows, stats, nil
}

// objectRow shows one JSON object as a row: a column per member, in key
// order, nested values as JSON.
func objectRow(obj map[string]json.RawMessage) ([]driver.Column, driver.Row) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cols := make([]driver.Column, len(keys))
	row := make(driver.Row, len(keys))
	for i, k := range keys {
		row[i] = anyValue(obj[k])
		cols[i] = driver.Column{Name: k, Type: row[i].Type()}
	}
	return cols, row
}

// unionRows shows JSON array elements as rows over the union of their
// members, in first-seen order; an element that is not an object is a
// "value" cell.
func unionRows(items []json.RawMessage) ([]driver.Column, []driver.Row) {
	var names []string
	index := map[string]int{}
	objs := make([]map[string]json.RawMessage, len(items))
	scalar := false
	for i, it := range items {
		obj, err := objectOf(it)
		if err != nil {
			scalar = true
			continue
		}
		objs[i] = obj
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := index[k]; !ok {
				index[k] = len(names)
				names = append(names, k)
			}
		}
	}
	if scalar {
		if _, ok := index["value"]; !ok {
			index["value"] = len(names)
			names = append(names, "value")
		}
	}
	cols := make([]driver.Column, len(names))
	for i, n := range names {
		cols[i] = driver.Column{Name: n, Type: driver.TypeUnknown}
	}
	rows := make([]driver.Row, len(items))
	for i, it := range items {
		row := make(driver.Row, len(names))
		for j := range row {
			row[j] = driver.Null()
		}
		if objs[i] == nil {
			row[index["value"]] = anyValue(it)
		} else {
			for k, raw := range objs[i] {
				row[index[k]] = anyValue(raw)
			}
		}
		for j, v := range row {
			if cols[j].Type == driver.TypeUnknown && !v.IsNull() {
				cols[j].Type = v.Type()
			}
		}
		rows[i] = row
	}
	for j := range cols {
		if cols[j].Type == driver.TypeUnknown {
			cols[j].Type = driver.TypeNull
		}
	}
	return cols, rows
}

// runGraphQL posts a document. The answer's data is one row with a column
// per top-level field; errors beside data are warnings, errors without
// data fail the query.
func (c *conn) runGraphQL(ctx context.Context, op string, r request) ([]driver.Column, []driver.Row, []string, error) {
	body := map[string]any{"query": r.document}
	if r.variables != nil {
		body["variables"] = r.variables
	}
	resp, err := c.client.do(ctx, http.MethodPost, c.client.graphql, body)
	if err != nil {
		return nil, nil, nil, c.fail(ctx, op, err)
	}
	var out struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return nil, nil, nil, c.fail(ctx, op, fmt.Errorf("the GraphQL answer is not JSON: %w", err))
	}
	var warnings []string
	for _, e := range out.Errors {
		msg := e.Message
		if e.Type != "" {
			msg = e.Type + ": " + msg
		}
		warnings = append(warnings, c.client.masked(msg))
	}
	if out.Data == nil {
		if len(warnings) > 0 {
			return nil, nil, nil, syntaxErr(op, "%s", strings.Join(warnings, "; "))
		}
		return nil, nil, nil, c.fail(ctx, op, errors.New("the GraphQL answer has no data"))
	}
	cols, row := objectRow(out.Data)
	return cols, []driver.Row{row}, warnings, nil
}

// runRateLimit reads the rate limits: one row per resource. The call costs
// no request.
func (c *conn) runRateLimit(ctx context.Context, op string) ([]driver.Column, []driver.Row, error) {
	raw, err := c.client.getJSON(ctx, "/rate_limit", nil)
	if err != nil {
		return nil, nil, c.fail(ctx, op, err)
	}
	var out struct {
		Resources map[string]struct {
			Limit     int64 `json:"limit"`
			Used      int64 `json:"used"`
			Remaining int64 `json:"remaining"`
			Reset     int64 `json:"reset"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, c.fail(ctx, op, err)
	}
	names := make([]string, 0, len(out.Resources))
	for n := range out.Resources {
		names = append(names, n)
	}
	sort.Strings(names)
	cols := []driver.Column{
		{Name: "resource", Type: driver.TypeString}, {Name: "limit", Type: driver.TypeInt}, {Name: "used", Type: driver.TypeInt},
		{Name: "remaining", Type: driver.TypeInt}, {Name: "reset", NativeType: "timestamp", Type: driver.TypeTime},
	}
	rows := make([]driver.Row, len(names))
	for i, n := range names {
		r := out.Resources[n]
		rows[i] = driver.Row{driver.String(n), driver.Int(r.Limit), driver.Int(r.Used), driver.Int(r.Remaining), driver.Time(time.Unix(r.Reset, 0).UTC())}
	}
	return cols, rows, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
