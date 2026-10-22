package milvus

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"

	"github.com/xavidop/tobira/driver"
)

// Query runs a native request (see parseRequest). It never writes, and
// on a read-only request it never loads a collection.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	readOnly := c.readOnly || q.ReadOnly
	req, err := parseRequest(q.Text, readOnly)
	if err != nil {
		return nil, err
	}
	if err := req.addPositional(q.Params); err != nil {
		return nil, err
	}
	db := req.Database
	if db == "" {
		db = c.db(q.Namespace)
	}
	s, err := c.getSchema(ctx, "query", db, req.Collection)
	if err != nil {
		return nil, err
	}
	switch req.Op {
	case opCount:
		return c.count(ctx, s, req, readOnly, start)
	case opGet:
		return c.get(ctx, s, req, readOnly, q.Limit, start)
	case opSearch:
		return c.searchOp(ctx, s, req, readOnly, q.Limit, start)
	}
	return c.queryOp(ctx, s, req, readOnly, q.Limit, start)
}

// scanSpec is a keyset scan in primary-key order.
type scanSpec struct {
	s           *collSchema
	op          string
	filter      string
	params      exprParams
	output      []string
	partitions  []string
	consistency commonpb.ConsistencyLevel
	readOnly    bool
}

// outputNames are the output fields to request: the named ones, or every
// static field plus the dynamic field.
func outputNames(output []string) []string {
	if len(output) > 0 {
		return output
	}
	return []string{"*"}
}

// scanPage reads up to limit entities with a primary key above after (no
// bound when NULL), skipping offset, in primary-key order: Milvus sorts by
// key when asked as its query iterator does (iterator and
// reduce_stop_for_best).
func (c *conn) scanPage(ctx context.Context, sp scanSpec, after driver.Value, offset, limit int) ([]record, error) {
	params := maps.Clone(sp.params)
	if params == nil {
		params = exprParams{}
	}
	expr := sp.filter
	if !after.IsNull() {
		tv, err := templateValue(after)
		if err != nil {
			return nil, err
		}
		params["tobira_after"] = tv
		expr = andExpr(sp.filter, sp.s.pkField().name+" > {tobira_after}")
	}
	qp := []*commonpb.KeyValuePair{
		{Key: "limit", Value: strconv.Itoa(limit)},
		{Key: "iterator", Value: "true"},
		{Key: "reduce_stop_for_best", Value: "true"},
	}
	if offset > 0 {
		qp = append(qp, &commonpb.KeyValuePair{Key: "offset", Value: strconv.Itoa(offset)})
	}
	req := &milvuspb.QueryRequest{
		DbName:             sp.s.db,
		CollectionName:     sp.s.name,
		Expr:               expr,
		ExprTemplateValues: params,
		OutputFields:       outputNames(sp.output),
		PartitionNames:     sp.partitions,
		QueryParams:        qp,
		ConsistencyLevel:   sp.consistency,
	}
	return c.runQuery(ctx, sp.s, sp.op, sp.readOnly, req)
}

// runQuery sends one Query RPC, loading the collection first when a
// writable request finds it unloaded.
func (c *conn) runQuery(ctx context.Context, s *collSchema, op string, readOnly bool, req *milvuspb.QueryRequest) ([]record, error) {
	var recs []record
	err := c.withLoad(ctx, op, s, readOnly, func() error {
		resp, err := c.svc.Query(ctx, req)
		if err := checkRPC(resp, err); err != nil {
			return err
		}
		recs, err = decodeFields(resp.GetFieldsData(), -1)
		return err
	})
	return recs, err
}

// queryOp streams a query in primary-key order, in pages of pageSize: the
// request's limit, when given, caps the total and its offset skips the
// first entities. Columns are fixed by the first page.
func (c *conn) queryOp(ctx context.Context, s *collSchema, r *request, readOnly bool, limit int, start time.Time) (driver.ResultSet, error) {
	sp := scanSpec{s: s, op: "query", filter: r.Filter, params: r.params, output: r.OutputFields, partitions: r.Partitions, consistency: r.consistency, readOnly: readOnly}
	remaining := int64(1<<63 - 1)
	if r.Limit != nil && *r.Limit > 0 {
		remaining = *r.Limit
	}
	offset := 0
	if r.Offset != nil {
		offset = int(*r.Offset)
	}
	after := driver.Null()
	pk := s.pkField().name
	page := func(ctx context.Context) ([]record, bool, error) {
		n := min(remaining, pageSize)
		if limit > 0 {
			n = min(n, int64(limit)+1) // one extra row proves truncation
		}
		recs, err := c.scanPage(ctx, sp, after, offset, int(n))
		if err != nil {
			return nil, false, err
		}
		offset = 0
		remaining -= int64(len(recs))
		if len(recs) > 0 {
			after = recs[len(recs)-1].values[pk]
		}
		done := len(recs) < int(n) || remaining <= 0 || after.IsNull()
		return recs, done, nil
	}
	first, done, err := page(ctx)
	if err != nil {
		return nil, err
	}
	sh := newShape(s, staticFields(s, r.OutputFields), "", first)
	dropped := droppedKeys{}
	var fetch fetchFunc
	if !done {
		fetch = func(ctx context.Context) ([]driver.Row, bool, error) {
			recs, done, err := page(ctx)
			if err != nil {
				return nil, false, err
			}
			return sh.rows(recs, dropped), done, nil
		}
	}
	rs := newPagedResultSet(sh.columns(first), sh.rows(first, dropped), fetch, limit, start)
	rs.warn = dropped.warnings
	return rs, nil
}

// get reads entities by primary key.
func (c *conn) get(ctx context.Context, s *collSchema, r *request, readOnly bool, limit int, start time.Time) (driver.ResultSet, error) {
	pk := s.pkField()
	ids := make([]driver.Value, len(r.ids))
	for i, v := range r.ids {
		k, err := pkValue(pk, v, "query")
		if err != nil {
			return nil, err
		}
		ids[i] = k
	}
	tv, err := templateValue(driver.List(ids...))
	if err != nil {
		return nil, err
	}
	params := maps.Clone(r.params)
	params["tobira_ids"] = tv
	req := &milvuspb.QueryRequest{
		DbName:             s.db,
		CollectionName:     s.name,
		Expr:               pk.name + " in {tobira_ids}",
		ExprTemplateValues: params,
		OutputFields:       outputNames(r.OutputFields),
		PartitionNames:     r.Partitions,
		QueryParams:        []*commonpb.KeyValuePair{{Key: "limit", Value: strconv.Itoa(len(ids))}},
		ConsistencyLevel:   r.consistency,
	}
	recs, err := c.runQuery(ctx, s, "query", readOnly, req)
	if err != nil {
		return nil, err
	}
	sh := newShape(s, staticFields(s, r.OutputFields), "", recs)
	dropped := droppedKeys{}
	rs := newPagedResultSet(sh.columns(recs), sh.rows(recs, dropped), nil, limit, start)
	rs.warn = dropped.warnings
	return rs, nil
}

// count counts the entities matching the filter.
func (c *conn) count(ctx context.Context, s *collSchema, r *request, readOnly bool, start time.Time) (driver.ResultSet, error) {
	req := &milvuspb.QueryRequest{
		DbName:             s.db,
		CollectionName:     s.name,
		Expr:               r.Filter,
		ExprTemplateValues: r.params,
		OutputFields:       []string{"count(*)"},
		PartitionNames:     r.Partitions,
		ConsistencyLevel:   r.consistency,
	}
	recs, err := c.runQuery(ctx, s, "query", readOnly, req)
	if err != nil {
		return nil, err
	}
	n := driver.Int(0)
	if len(recs) == 1 {
		if v, ok := recs[0].values["count(*)"]; ok {
			n = v
		}
	}
	cols := []driver.Column{{Name: "count", NativeType: "Int64", Type: driver.TypeInt}}
	return driver.NewSliceResultSet(cols, []driver.Row{{n}}, driver.Stats{RowsAffected: -1, Duration: time.Since(start)}), nil
}

// pkValue converts a key value to the primary key's type: an Int64 key
// takes Int, or Decimal or text holding an integer; a VarChar key takes
// text.
func pkValue(pk *fieldInfo, v driver.Value, op string) (driver.Value, error) {
	bad := func() (driver.Value, error) {
		return driver.Null(), driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("primary key %q is %s, got %s %s", pk.name, pk.dataType, v.Type(), v))
	}
	if v.IsNull() {
		return bad()
	}
	if pk.dataType == schemapb.DataType_Int64 {
		i, err := wholeNumber(v)
		if err != nil || v.Type() == driver.TypeFloat {
			return bad()
		}
		return driver.Int(i), nil
	}
	if v.Type() != driver.TypeString {
		return bad()
	}
	return v, nil
}
