package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

const (
	// defaultPage is the Browse page when Limit is 0; maxPage caps it (10
	// requests of 100 items).
	defaultPage = 100
	maxPage     = 1000
	// maxScan caps the items one filtered Browse page examines in tobira,
	// and maxRequests the requests it sends: past either, the page ends
	// early and its cursor resumes the scan.
	maxScan     = 1000
	maxRequests = 10
)

// listRequest is a paginated call with its parameters fixed: a
// repository's list endpoint, an owner's repository listing (listPath) or
// a search (the qualifiers in query["q"]).
type listRequest struct {
	col         *collection
	owner, repo string     // repo "" = owner-wide
	query       url.Values // without per_page and page
	perPage     int
	local       string // the conditions applied in tobira, rendered for the hash
	listPath    string // an owner's repository listing endpoint
}

// path is the endpoint path.
func (r listRequest) path() string {
	switch {
	case r.col.search != nil:
		return r.col.search.endpoint
	case r.col.listing:
		return r.listPath
	}
	return r.col.list(r.owner, r.repo)
}

// envelope names the member holding the items of a page.
func (r listRequest) envelope() string {
	if r.col.search != nil {
		return "items"
	}
	return r.col.envelope
}

// pageQuery is the query of one page.
func (r listRequest) pageQuery(page int) url.Values {
	q := url.Values{}
	for k, v := range r.query {
		q[k] = v
	}
	q.Set("per_page", strconv.Itoa(r.perPage))
	q.Set("page", strconv.Itoa(page))
	return q
}

// hash identifies the request, so a cursor is only resumed with the same
// parameters and the same conditions applied in tobira.
func (r listRequest) hash() string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(r.col.name + "|" + strings.ToLower(r.owner+"/"+r.repo) + "|" + strings.ToLower(r.path()) + "|" + strconv.Itoa(r.perPage) + "|" + r.query.Encode() + "|" + r.local))
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

// renderConditions spells conditions out for a cursor's hash.
func renderConditions(conds []driver.Condition) string {
	parts := make([]string, len(conds))
	for i, c := range conds {
		parts[i] = c.Field + "\x00" + string(c.Op) + "\x00" + c.Value.String()
	}
	return strings.Join(parts, "\x01")
}

// fetchPage reads one page of items, dropping pull requests from an issues
// listing.
func (c *conn) fetchPage(ctx context.Context, op string, r listRequest, page int) ([]json.RawMessage, bool, error) {
	items, next, err := c.client.page(ctx, r.path(), r.pageQuery(page), r.envelope())
	if err != nil {
		return nil, false, c.fail(ctx, op, err)
	}
	if r.col.dropPullRequests {
		items = slices.DeleteFunc(items, isPullRequest)
	}
	return items, next, nil
}

// browseCursor is the position of a Browse page: the next API page to read,
// the items of it already shown (a filtered page may stop mid-page) and the
// hash of the request.
type browseCursor struct {
	page, skip int
	hash       string
}

func (c browseCursor) encode() string {
	return "p|" + strconv.Itoa(c.page) + "|" + strconv.Itoa(c.skip) + "|" + c.hash
}

func decodeCursor(s, hash string) (browseCursor, error) {
	if s == "" {
		return browseCursor{page: 1, hash: hash}, nil
	}
	bad := func(why string) (browseCursor, error) {
		return browseCursor{}, driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid cursor: "+why))
	}
	f := strings.Split(s, "|")
	if len(f) != 4 || f[0] != "p" {
		return bad("not a github browse cursor")
	}
	page, err1 := strconv.Atoi(f[1])
	skip, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil || page < 1 || skip < 0 {
		return bad("bad page or offset")
	}
	if f[3] != hash {
		return bad("it belongs to a browse with other filters, sorting or page size")
	}
	return browseCursor{page: page, skip: skip, hash: hash}, nil
}

// Browse pages through a collection. Conditions the list endpoint can take
// are sent as its parameters; the others, and sorting GitHub does not
// offer, are applied here (sorting is refused). A page reads at most
// maxScan items before ending early with a warning.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	const op = "browse"
	start := time.Now()
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	col, s, err := c.collectionRef(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	var warnings []string
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = defaultPage
	case limit > maxPage:
		warnings = append(warnings, fmt.Sprintf("the page size was cut from %d to %d rows", limit, maxPage))
		limit = maxPage
	}
	req, local, err := c.planBrowse(ctx, op, col, s, opts, limit)
	if err != nil {
		return nil, err
	}
	cur, err := decodeCursor(opts.Cursor, req.hash())
	if err != nil {
		return nil, err
	}
	refOut := driver.CollectionRef{Namespace: s.name(), Name: col.name}
	rows := make([]driver.Row, 0, limit)
	page, skip := cur.page, cur.skip
	scanned, requests, more := 0, 0, false
	for len(rows) < limit {
		items, next, err := c.fetchPage(ctx, op, req, page)
		if err != nil {
			return nil, err
		}
		requests++
		skip = min(skip, len(items))
		stopped := false
		for i := skip; i < len(items); i++ {
			scanned++
			row, err := col.row(items[i])
			if err != nil {
				return nil, c.fail(ctx, op, err)
			}
			ok, err := matchesAll(col.columns, row, local)
			if err != nil {
				return nil, syntaxErr(op, "%v", err)
			}
			if ok {
				rows = append(rows, row)
			}
			if len(rows) == limit || scanned >= maxScan {
				skip = i + 1
				stopped = true
				if scanned >= maxScan && len(rows) < limit {
					warnings = append(warnings, fmt.Sprintf("the filter looked at %d items and stopped early; the next page continues from there", maxScan))
				}
				break
			}
		}
		if stopped {
			more = skip < len(items) || next
			if skip == len(items) && next {
				page, skip = page+1, 0
			}
			break
		}
		if !next {
			break
		}
		page, skip = page+1, 0
		if requests >= maxRequests {
			more = true
			warnings = append(warnings, fmt.Sprintf("the page stopped after %d requests; the next page continues from there", maxRequests))
			break
		}
	}
	stats := driver.Stats{RowsAffected: -1, Duration: time.Since(start), Truncated: more, Warnings: warnings}
	if more {
		stats.NextCursor = browseCursor{page: page, skip: skip, hash: req.hash()}.encode()
	}
	if n := c.client.rateNotice(); n != "" {
		stats.Notices = append(stats.Notices, n)
	}
	return driver.NewSliceResultSet(col.resultColumns(refOut), rows, stats), nil
}

// planBrowse turns the Browse options into the list request and the
// conditions left for tobira to apply. A repository collection takes its
// endpoint's parameters; an owner-wide one takes search qualifiers, or the
// repository listing's sort.
func (c *conn) planBrowse(ctx context.Context, op string, col *collection, s scope, opts driver.BrowseOptions, limit int) (listRequest, []driver.Condition, error) {
	if opts.Filter.Raw != "" {
		return listRequest{}, nil, unsupportedErr(op, "a raw filter is not supported: run a list query with the endpoint's own parameters")
	}
	req := listRequest{col: col, owner: s.owner, repo: s.repo, query: url.Values{}}
	for k, v := range col.defaults {
		req.query[k] = v
	}
	var qualifiers []string
	var searchText []string // free text of a commit search (message contains …)
	ownerBase := ""
	if col.search != nil {
		base, err := c.ownerQualifier(ctx, op, s.owner)
		if err != nil {
			return listRequest{}, nil, err
		}
		ownerBase = base
		if col.search.base != "" {
			qualifiers = append(qualifiers, col.search.base)
		}
	}
	if col.listing {
		path, query, err := c.ownerListing(ctx, op, s.owner)
		if err != nil {
			return listRequest{}, nil, err
		}
		req.listPath = path
		for k, v := range query {
			req.query[k] = v
		}
	}
	var local []driver.Condition
	pushed := map[string]bool{} // parameters already set by a condition
	for _, cond := range opts.Filter.Conditions {
		if col.col(cond.Field) == nil {
			return listRequest{}, nil, syntaxErr(op, "unknown field %q in %s", cond.Field, col.name)
		}
		if col.search != nil {
			q := col.search.qualifiers[cond.Field]
			if cond.Field == "repository" {
				q = repoQualifier(s.owner)
			}
			if q != nil {
				if v, ok := q(cond); ok {
					if cond.Field == "repository" {
						// GitHub ORs user:, org: and repo: together: a
						// repository replaces the owner.
						ownerBase = ""
					}
					qualifiers = append(qualifiers, v)
					continue
				}
			}
			if col.search.endpoint == searchCommits && cond.Field == "message" && cond.Op == driver.OpContains {
				if text, ok := text(cond.Value); ok && strings.TrimSpace(text) != "" {
					searchText = append(searchText, quoteQualifier(strings.TrimSpace(text)))
					local = append(local, cond) // the search matches words; the substring check still runs
					continue
				}
			}
			local = append(local, cond)
			continue
		}
		if pd, ok := col.filters[cond.Field]; ok && !pushed[pd.param] {
			if v, ok := pd.encode(cond); ok {
				req.query.Set(pd.param, v)
				pushed[pd.param] = true
				continue
			}
		}
		local = append(local, cond)
	}
	if col.search != nil {
		if col.search.endpoint == searchCommits && len(searchText) == 0 {
			return listRequest{}, nil, unsupportedErr(op, "GitHub's commit search needs text: filter %s by message contains a word or phrase (a list query takes it in q)", col.name)
		}
		parts := append(searchText, ownerBase)
		parts = append(parts, qualifiers...)
		req.query.Set("q", joinTerms(parts...))
	}
	sorts, direction := col.sorts, "direction"
	if col.search != nil {
		sorts, direction = col.search.sorts, "order"
	}
	switch len(opts.Sort) {
	case 0:
	case 1:
		param, ok := sorts[opts.Sort[0].Field]
		if !ok {
			return listRequest{}, nil, unsupportedErr(op, "GitHub cannot sort %s by %q%s", col.name, opts.Sort[0].Field, sortableHintOf(col, sorts))
		}
		req.query.Set("sort", param)
		if opts.Sort[0].Desc {
			req.query.Set(direction, "desc")
		} else {
			req.query.Set(direction, "asc")
		}
	default:
		return listRequest{}, nil, unsupportedErr(op, "GitHub sorts by one field at a time")
	}
	req.perPage = min(limit, maxPerPage)
	if col.dropPullRequests {
		// Pull requests are dropped from the page after it is read, so a
		// small page could cost a request per row: read full pages.
		req.perPage = maxPerPage
	}
	req.local = renderConditions(local)
	return req, local, nil
}

// sortableHint lists what a collection can be sorted by.
func sortableHint(col *collection) string {
	if col.search != nil {
		return sortableHintOf(col, col.search.sorts)
	}
	return sortableHintOf(col, col.sorts)
}

func sortableHintOf(col *collection, sorts map[string]string) string {
	if len(sorts) == 0 {
		return ": it lists " + col.name + " in GitHub's own order only"
	}
	fields := make([]string, 0, len(sorts))
	for f := range sorts {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	return ": it sorts by " + strings.Join(fields, ", ")
}
