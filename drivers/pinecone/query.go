package pinecone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/xavidop/tobira/driver"
)

// The query ops: the only requests Query sends. Each one is a read.
const (
	opQuery = "query" // nearest records to a vector, a record id or a sparse vector
	opFetch = "fetch" // records by id
	opList  = "list"  // every record of a namespace (ids by prefix, then fetched)
	opStats = "stats" // the index statistics, one row per namespace
)

// Bounds on the work one call may ask for.
const (
	defaultK      = 10   // query/Similar results when none is given
	maxK          = 1000 // Pinecone's own cap on topK with metadata or values; also the fetch cap
	listPage      = 100  // ids per list call (Pinecone's maximum)
	defaultBrowse = 100  // Browse page when Limit is 0
	maxBrowse     = 1000 // cap on a Browse page
)

// opKeys are the members each op accepts besides op, index and namespace.
var opKeys = map[string][]string{
	opQuery: {"vector", "id", "sparseVector", "topK", "filter", "includeMetadata", "includeValues"},
	opFetch: {"ids", "includeMetadata", "includeValues"},
	opList:  {"prefix", "limit", "includeMetadata", "includeValues"},
	opStats: {},
}

// request is a parsed native query.
type request struct {
	op              string
	index           string // "" = the query's namespace, or the DSN's index host
	namespace       string // Pinecone namespace; "" = the default one
	vector          []float32
	id              string
	sparse          *pinecone.SparseValues
	topK            uint32
	filter          *structpb.Struct
	includeMetadata bool
	includeValues   bool
	ids             []string
	prefix          string
	limit           int // list: total cap, 0 = none
	warnings        []string
}

// parseRequest parses a native query: a JSON object with an "op" (query,
// the default, fetch, list or stats), an optional "index" and "namespace",
// and the op's own members in Pinecone's REST spelling (topK,
// includeMetadata, sparseVector...). Any other member, a duplicate key or a
// trailing text is refused before a byte is sent. With readOnly every
// refusal is ErrReadOnly, so a text the guard cannot read is never let
// through (fail closed); otherwise a disallowed op is ErrUnsupported (Query
// never writes: edits go through the Mutator) and a malformed text
// ErrSyntax.
func parseRequest(text string, readOnly bool) (request, error) {
	refuse := func(kind error, format string, args ...any) (request, error) {
		msg := fmt.Sprintf(format, args...)
		if readOnly && kind != driver.ErrReadOnly {
			kind, msg = driver.ErrReadOnly, "read-only: "+msg
		}
		return request{}, driver.Wrap(kind, Name, "query", errors.New(msg))
	}
	fields, err := topLevelObject(text)
	if err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	r := request{op: opQuery, includeMetadata: true}
	if raw, ok := fields["op"]; ok {
		if err := decodeString(raw, &r.op); err != nil {
			return refuse(driver.ErrSyntax, `"op" must be a string`)
		}
		delete(fields, "op")
	}
	allowed, ok := opKeys[r.op]
	if !ok {
		if readOnly {
			return refuse(driver.ErrReadOnly, "read-only: op %q is refused; only query, fetch, list and stats run", r.op)
		}
		return refuse(driver.ErrUnsupported, "op %q is not a query op: only query, fetch, list and stats run (edit records with the editor)", r.op)
	}
	for k := range fields {
		if k != "index" && k != "namespace" && !slices.Contains(allowed, k) {
			return refuse(driver.ErrSyntax, "unknown member %q for op %s", k, r.op)
		}
	}
	str := func(k string, dst *string) error {
		if raw, ok := fields[k]; ok {
			if err := decodeString(raw, dst); err != nil {
				return fmt.Errorf("%q must be a string", k)
			}
		}
		return nil
	}
	boolean := func(k string, dst *bool) error {
		if raw, ok := fields[k]; ok {
			if err := strictDecode(raw, dst); err != nil {
				return fmt.Errorf("%q must be true or false", k)
			}
		}
		return nil
	}
	if r.op == opStats {
		if _, ok := fields["namespace"]; ok {
			return refuse(driver.ErrSyntax, "stats covers every namespace: give no namespace")
		}
	}
	for _, err := range []error{
		str("index", &r.index), str("namespace", &r.namespace), str("prefix", &r.prefix), str("id", &r.id),
		boolean("includeMetadata", &r.includeMetadata), boolean("includeValues", &r.includeValues),
	} {
		if err != nil {
			return refuse(driver.ErrSyntax, "%v", err)
		}
	}
	r.namespace = pineconeNamespace(r.namespace)
	switch r.op {
	case opQuery:
		err = r.parseQuery(fields)
	case opFetch:
		err = r.parseFetch(fields)
	case opList:
		if raw, ok := fields["limit"]; ok {
			if e := strictDecode(raw, &r.limit); e != nil || r.limit < 0 {
				err = errors.New(`"limit" must be a whole number, 0 or more`)
			}
		}
	}
	if err != nil {
		var de *driver.Error
		if errors.As(err, &de) {
			return refuse(de.Kind, "%v", de.Err)
		}
		return refuse(driver.ErrSyntax, "%v", err)
	}
	return r, nil
}

// parseQuery reads exactly one of vector, id and sparseVector, plus topK
// and filter.
func (r *request) parseQuery(fields map[string]json.RawMessage) error {
	given := 0
	if raw, ok := fields["vector"]; ok {
		given++
		if err := strictDecode(raw, &r.vector); err != nil || len(r.vector) == 0 {
			return errors.New(`"vector" must be a non-empty list of numbers`)
		}
		for _, f := range r.vector {
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				return errors.New(`"vector" must hold finite numbers`)
			}
		}
	}
	if _, ok := fields["id"]; ok {
		given++
		if r.id == "" {
			return errors.New(`"id" must not be empty`)
		}
	}
	if raw, ok := fields["sparseVector"]; ok {
		given++
		var s struct {
			Indices []uint32  `json:"indices"`
			Values  []float32 `json:"values"`
		}
		if err := strictDecode(raw, &s); err != nil || len(s.Indices) == 0 || len(s.Indices) != len(s.Values) {
			return errors.New(`"sparseVector" must be {"indices":[...],"values":[...]}, of equal non-zero length`)
		}
		r.sparse = &pinecone.SparseValues{Indices: s.Indices, Values: s.Values}
	}
	if given != 1 {
		return errors.New(`a query needs exactly one of "vector", "id" or "sparseVector"`)
	}
	r.topK = defaultK
	if raw, ok := fields["topK"]; ok {
		var k int64
		if err := strictDecode(raw, &k); err != nil || k < 0 {
			return errors.New(`"topK" must be a whole number (0 means the default of 10)`)
		}
		switch {
		case k == 0:
		case k > maxK:
			r.topK = maxK
			r.warnings = append(r.warnings, fmt.Sprintf("topK %d is above the driver's cap: returning at most %d records", k, maxK))
		default:
			r.topK = uint32(k)
		}
	}
	if raw, ok := fields["filter"]; ok {
		f, err := rawFilter(string(raw))
		if err != nil {
			return errors.New(`"filter" must be a Pinecone filter object`)
		}
		r.filter = f
	}
	return nil
}

// parseFetch reads the ids, at least one and at most maxK.
func (r *request) parseFetch(fields map[string]json.RawMessage) error {
	raw, ok := fields["ids"]
	if !ok {
		return errors.New(`fetch needs "ids"`)
	}
	if err := strictDecode(raw, &r.ids); err != nil || len(r.ids) == 0 {
		return errors.New(`"ids" must be a non-empty list of strings`)
	}
	if len(r.ids) > maxK {
		return driver.Wrap(driver.ErrUnsupported, Name, "query", fmt.Errorf("fetch takes at most %d ids", maxK))
	}
	for _, id := range r.ids {
		if id == "" {
			return errors.New(`"ids" must not hold an empty id`)
		}
	}
	return nil
}

// decodeString decodes a JSON string (not null, not a number).
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
// one, so {"op":"list","op":"delete"} must not be read as either.
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

// Query runs a native request (see parseRequest). It never writes.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	req, err := parseRequest(q.Text, c.readOnly || q.ReadOnly)
	if err != nil {
		return nil, err
	}
	if len(q.Params) > 0 {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("pinecone requests take no parameters: put values in the JSON"))
	}
	index := req.index
	if index == "" {
		index = q.Namespace
	}
	h, err := c.index(ctx, "query", index)
	if err != nil {
		return nil, err
	}
	ic := h.ns(req.namespace)
	switch req.op {
	case opStats:
		return c.queryStats(ctx, h, start)
	case opList:
		return c.queryList(ctx, ic, req, q.Limit, start)
	}
	var recs []record
	opts := shapeOptions{values: req.includeValues}
	switch req.op {
	case opFetch:
		recs, err = c.fetch(ctx, "query", ic, req.ids)
	case opQuery:
		recs, err = c.runQuery(ctx, ic, req)
		opts.score = scoreColumn
	}
	if err != nil {
		return nil, err
	}
	if !req.includeMetadata {
		for i := range recs {
			recs[i].meta = nil
		}
	}
	sh := newShape(recs, opts)
	rows := make([]driver.Row, len(recs))
	for i, r := range recs {
		rows[i], _ = sh.row(r)
	}
	rs := newPagedResultSet(sh.columns(), rows, nil, q.Limit, start)
	rs.stats.Warnings = append(req.warnings, sh.warnings()...)
	return rs, nil
}

// runQuery runs a query op.
func (c *conn) runQuery(ctx context.Context, ic *pinecone.IndexConnection, req request) ([]record, error) {
	var (
		resp *pinecone.QueryVectorsResponse
		err  error
	)
	if req.id != "" {
		resp, err = ic.QueryByVectorId(ctx, &pinecone.QueryByVectorIdRequest{
			VectorId: req.id, TopK: req.topK, MetadataFilter: req.filter,
			IncludeValues: req.includeValues, IncludeMetadata: req.includeMetadata,
		})
	} else {
		resp, err = ic.QueryByVectorValues(ctx, &pinecone.QueryByVectorValuesRequest{
			Vector: req.vector, SparseValues: req.sparse, TopK: req.topK, MetadataFilter: req.filter,
			IncludeValues: req.includeValues, IncludeMetadata: req.includeMetadata,
		})
	}
	if err != nil {
		return nil, c.fail(ctx, "query", err)
	}
	return scored(resp), nil
}

// scored converts query matches.
func scored(resp *pinecone.QueryVectorsResponse) []record {
	out := make([]record, 0, len(resp.Matches))
	for _, m := range resp.Matches {
		if m == nil || m.Vector == nil {
			continue
		}
		r := newRecord(m.Vector)
		r.score = m.Score
		out = append(out, r)
	}
	return out
}

// queryStats returns one row per namespace: its collection name and record
// count, sorted by name.
func (c *conn) queryStats(ctx context.Context, h *indexHandle, start time.Time) (driver.ResultSet, error) {
	info, err := c.stats(ctx, "query", h)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(info.namespaces))
	for n := range info.namespaces {
		names = append(names, n)
	}
	slices.Sort(names)
	cols := []driver.Column{
		{Name: "namespace", NativeType: "string", Type: driver.TypeString},
		{Name: "vector_count", NativeType: "number", Type: driver.TypeInt},
	}
	rows := make([]driver.Row, len(names))
	for i, n := range names {
		rows[i] = driver.Row{driver.String(n), driver.Int(int64(info.namespaces[n]))}
	}
	return driver.NewSliceResultSet(cols, rows, driver.Stats{RowsAffected: -1, Duration: time.Since(start)}), nil
}

// queryList streams a namespace: ids in pages of up to 100 (by prefix),
// each page fetched. The request's limit caps the total. Columns are fixed
// by the first page; metadata keys first seen later are reported in a
// warning.
func (c *conn) queryList(ctx context.Context, ic *pinecone.IndexConnection, req request, limit int, start time.Time) (driver.ResultSet, error) {
	remaining := math.MaxInt
	if req.limit > 0 {
		remaining = req.limit
	}
	token := ""
	page := func(ctx context.Context) ([]record, bool, error) {
		n := min(remaining, listPage)
		if limit > 0 {
			n = min(n, limit+1) // one extra row proves truncation
		}
		ids, next, err := c.listPage(ctx, "query", ic, req.prefix, n, token)
		if err != nil {
			return nil, false, err
		}
		recs, err := c.fetch(ctx, "query", ic, ids)
		if err != nil {
			return nil, false, err
		}
		remaining -= len(ids)
		token = next
		return recs, next == "" || remaining <= 0 || len(ids) == 0, nil
	}
	opts := shapeOptions{values: req.includeValues}
	if remaining == 0 {
		return newPagedResultSet(newShape(nil, opts).columns(), nil, nil, limit, start), nil
	}
	first, done, err := page(ctx)
	if err != nil {
		return nil, err
	}
	strip := func(recs []record) {
		if !req.includeMetadata {
			for i := range recs {
				recs[i].meta = nil
			}
		}
	}
	strip(first)
	sh := newShape(first, opts)
	dropped := droppedKeys{}
	toRows := func(recs []record) []driver.Row {
		rows := make([]driver.Row, len(recs))
		for i, r := range recs {
			var d []string
			rows[i], d = sh.row(r)
			dropped.add(d)
		}
		return rows
	}
	var fetch fetchFunc
	if !done {
		fetch = func(ctx context.Context) ([]driver.Row, bool, error) {
			recs, done, err := page(ctx)
			if err != nil {
				return nil, false, err
			}
			strip(recs)
			return toRows(recs), done, nil
		}
	}
	rs := newPagedResultSet(sh.columns(), toRows(first), fetch, limit, start)
	rs.stats.Warnings = sh.warnings()
	rs.warn = dropped.warnings
	return rs, nil
}

// listPage lists up to n ids (1..100) after token ("" = the start).
func (c *conn) listPage(ctx context.Context, op string, ic *pinecone.IndexConnection, prefix string, n int, token string) ([]string, string, error) {
	limit := uint32(max(1, min(n, listPage)))
	in := &pinecone.ListVectorsRequest{Limit: &limit}
	if prefix != "" {
		in.Prefix = &prefix
	}
	if token != "" {
		in.PaginationToken = &token
	}
	resp, err := ic.ListVectors(ctx, in)
	if err != nil {
		return nil, "", c.fail(ctx, op, err)
	}
	ids := make([]string, 0, len(resp.VectorIds))
	for _, id := range resp.VectorIds {
		if id != nil {
			ids = append(ids, *id)
		}
	}
	next := ""
	if resp.NextPaginationToken != nil {
		next = *resp.NextPaginationToken
	}
	return ids, next, nil
}

// fetch reads records by id, in the order of ids; ids that do not exist
// (any more) are skipped.
func (c *conn) fetch(ctx context.Context, op string, ic *pinecone.IndexConnection, ids []string) ([]record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	resp, err := ic.FetchVectors(ctx, ids)
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	out := make([]record, 0, len(ids))
	for _, id := range ids {
		if v, ok := resp.Vectors[id]; ok && v != nil {
			out = append(out, newRecord(v))
		}
	}
	return out, nil
}
