package chroma

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/xavidop/tobira/driver"
)

// rawOrNil passes a raw JSON clause on, or a nil interface when there is
// none (a typed nil would be sent as null).
func rawOrNil(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return r
}

// Query runs a native request (see parseRequest). It never writes, and
// queryTexts are embedded only by the host's embedder, never by Chroma.
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
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("chroma requests take no parameters: put values in the JSON"))
	}
	info, err := c.collection(ctx, "query", driver.CollectionRef{Namespace: q.Namespace, Name: req.collection})
	if err != nil {
		return nil, err
	}
	switch req.op {
	case opCount:
		n, err := c.api.count(ctx, c.tenant, info.ref.Namespace, info.id)
		if err != nil {
			return nil, c.fail(ctx, "query", err)
		}
		cols := []driver.Column{{Name: "count", NativeType: "count", Type: driver.TypeInt}}
		return driver.NewSliceResultSet(cols, []driver.Row{{driver.Int(n)}}, driver.Stats{RowsAffected: -1, Duration: time.Since(start)}), nil
	case opQuery:
		return c.runQuery(ctx, info, req, q.Limit, start)
	}
	return c.runGet(ctx, info, req, q.Limit, start)
}

// optsFor maps an include list to the columns it produces.
func optsFor(info *collInfo, include []string) shapeOpts {
	ref := info.ref
	return shapeOpts{
		ref:        &ref,
		documents:  slices.Contains(include, "documents"),
		uris:       slices.Contains(include, "uris"),
		embeddings: slices.Contains(include, "embeddings"),
		metadatas:  slices.Contains(include, "metadatas"),
		distances:  slices.Contains(include, "distances"),
		vecType:    info.vecType,
	}
}

// runGet streams a get in pages of getPage records. The request's limit,
// when given, caps the total; Query.Limit asks for one extra record to
// prove truncation. Columns are fixed by the first page.
func (c *conn) runGet(ctx context.Context, info *collInfo, req request, limit int, start time.Time) (driver.ResultSet, error) {
	include := req.include
	if include == nil {
		include = []string{"documents", "metadatas"}
	}
	remaining := -1 // the request's own cap; -1 = none
	if req.limit != nil {
		remaining = *req.limit
	}
	pageSize := getPage
	if limit > 0 {
		pageSize = min(pageSize, limit+1)
	}
	offset := req.offset
	page := func(ctx context.Context) ([]record, bool, error) {
		n := pageSize
		if remaining >= 0 {
			n = min(n, remaining)
		}
		off := offset
		resp, err := c.api.get(ctx, c.tenant, info.ref.Namespace, info.id, getRequest{
			IDs: req.ids, Where: rawOrNil(req.where), WhereDocument: rawOrNil(req.whereDocument),
			Limit: &n, Offset: &off, Include: include,
		})
		if err != nil {
			return nil, false, c.fail(ctx, "query", err)
		}
		recs, err := getRecords(resp)
		if err != nil {
			return nil, false, driver.Wrap(nil, Name, "query", err)
		}
		offset += len(recs)
		if remaining >= 0 {
			remaining -= len(recs)
		}
		done := len(recs) < n || remaining == 0
		return recs, done, nil
	}
	o := optsFor(info, include)
	if remaining == 0 {
		sh := newShape(nil, o)
		return newPagedResultSet(sh.columns(), nil, nil, limit, start), nil
	}
	first, done, err := page(ctx)
	if err != nil {
		return nil, err
	}
	sh := newShape(first, o)
	toRows := func(recs []record) []driver.Row {
		rows := make([]driver.Row, len(recs))
		for i, r := range recs {
			rows[i] = sh.row(r)
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
			return toRows(recs), done, nil
		}
	}
	rs := newPagedResultSet(sh.columns(), toRows(first), fetch, limit, start)
	rs.warn = sh.warnings
	return rs, nil
}

// runQuery runs a nearest-neighbour query. queryTexts go through the
// host embedder; Chroma's distances are shown as Chroma returns them.
func (c *conn) runQuery(ctx context.Context, info *collInfo, req request, limit int, start time.Time) (driver.ResultSet, error) {
	embs := req.queryEmbeddings
	if len(req.queryTexts) > 0 {
		var err error
		if embs, err = c.embed(ctx, "query", req.queryTexts); err != nil {
			return nil, err
		}
	}
	for _, e := range embs {
		if err := checkDim(info, e, "query"); err != nil {
			return nil, err
		}
	}
	include := req.include
	if include == nil {
		include = []string{"documents", "metadatas", "distances"}
	}
	resp, err := c.api.query(ctx, c.tenant, info.ref.Namespace, info.id, queryRequest{
		QueryEmbeddings: embs, IDs: req.ids, Where: rawOrNil(req.where), WhereDocument: rawOrNil(req.whereDocument),
		NResults: req.nResults, Include: include,
	})
	if err != nil {
		return nil, c.fail(ctx, "query", err)
	}
	recs, err := queryRecords(resp, func(d float64) float64 { return d })
	if err != nil {
		return nil, driver.Wrap(nil, Name, "query", err)
	}
	o := optsFor(info, include)
	o.queries = len(embs) > 1
	sh := newShape(recs, o)
	rows := make([]driver.Row, len(recs))
	for i, r := range recs {
		rows[i] = sh.row(r)
	}
	rs := newPagedResultSet(sh.columns(), rows, nil, limit, start)
	rs.stats.Warnings = append(slices.Clone(req.warnings), sh.warnings()...)
	return rs, nil
}

// checkDim refuses a query embedding whose size differs from the
// collection's.
func checkDim(info *collInfo, v []float32, op string) error {
	if info.dim > 0 && len(v) != info.dim {
		return driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("collection %q holds %d-dimensional embeddings, the query has %d", info.ref.Name, info.dim, len(v)))
	}
	return finite(v)
}
