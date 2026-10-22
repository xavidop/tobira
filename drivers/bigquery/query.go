package bigquery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	bq "cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	"github.com/xavidop/tobira/driver"
)

// Query runs one GoogleSQL statement as a query job. BigQuery keeps no
// session between calls, so each call is independent.
//
//   - A read-only call (conn opened read-only, or q.ReadOnly) must pass the
//     lexical allow-list (checkReadOnly) before anything is sent, then a
//     dry run of the same text and parameters, whose statementType must be
//     SELECT (checkStatementType). Only then is the job run.
//   - Every job carries maximumBytesBilled (1 GiB unless the DSN's
//     max_bytes_billed says otherwise), so a query that would bill more
//     fails instead of running.
//   - q.Namespace is the default dataset (the DSN's when empty).
//   - q.Params bind to ? placeholders.
//   - q.Limit reads one row past the limit to set Stats.Truncated.
//
// A writable call runs anything, DML and scripts included; DML reports
// its affected rows. Cancelling ctx returns at once and cancels the job on
// the server in the background, so it stops billing.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	return c.run(ctx, "query", q, c.readOnly || q.ReadOnly)
}

// guardRead runs both layers of the read-only guard on q.
func (c *conn) guardRead(ctx context.Context, op string, q driver.Query, params []bq.QueryParameter) error {
	if err := checkReadOnly(q.Text); err != nil {
		return withOp(err, op)
	}
	st, err := c.dryRun(ctx, op, q, params)
	if err != nil {
		return err
	}
	return checkStatementType(op, st.StatementType)
}

// newQuery builds the job for q: GoogleSQL, the default dataset, the
// parameters and the byte cap. It never sets a destination table or a
// session.
func (c *conn) newQuery(q driver.Query, params []bq.QueryParameter) *bq.Query {
	bqq := c.client.Query(q.Text)
	bqq.Parameters = params
	if ds := c.dataset(q.Namespace); ds != "" {
		bqq.DefaultProjectID = c.dsn.project
		bqq.DefaultDatasetID = ds
	}
	bqq.MaxBytesBilled = c.dsn.maxBytesBilled
	return bqq
}

// dryRun validates q on the server without running it, returning the
// statistics real BigQuery reports (statement type, bytes, schema).
func (c *conn) dryRun(ctx context.Context, op string, q driver.Query, params []bq.QueryParameter) (*bq.QueryStatistics, error) {
	bqq := c.newQuery(q, params)
	bqq.DryRun = true
	job, err := bqq.Run(ctx)
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	status := job.LastStatus()
	if status == nil {
		return nil, driver.Wrap(nil, Name, op, errors.New("the dry run returned no status"))
	}
	if err := status.Err(); err != nil {
		return nil, c.fail(ctx, op, err)
	}
	var st *bq.QueryStatistics
	if status.Statistics != nil {
		st, _ = status.Statistics.Details.(*bq.QueryStatistics)
	}
	if st == nil {
		st = &bq.QueryStatistics{}
	}
	return st, nil
}

// newJobID returns a job id that names tobira, so the job can be found
// (and cancelled) by id.
func newJobID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "tobira_" + hex.EncodeToString(b[:])
}

func (c *conn) run(ctx context.Context, op string, q driver.Query, ro bool) (driver.ResultSet, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	params, err := toParams(op, q.Params)
	if err != nil {
		return nil, err
	}
	if ro {
		if err := c.guardRead(ctx, op, q, params); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	bqq := c.newQuery(q, params)
	bqq.JobID = newJobID()

	// The iterator keeps the context it was created with; it gets one of
	// its own, cancelled by ctx while Query runs and by each Next's ctx
	// while that Next runs.
	ictx, icancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, icancel)
	job, err := bqq.Run(ictx)
	var it *bq.RowIterator
	if err == nil {
		it, err = job.Read(ictx)
	}
	stop()
	if err != nil {
		icancel()
		if ctx.Err() != nil {
			c.cancelJob(bqq.JobID, job)
		}
		return nil, c.fail(ctx, op, err)
	}
	if len(it.Schema) == 0 {
		// DML, DDL or a script: nothing to read.
		defer icancel()
		stats := driver.Stats{RowsAffected: -1}
		if status, err := job.Status(ictx); err == nil && status.Statistics != nil {
			if qs, ok := status.Statistics.Details.(*bq.QueryStatistics); ok && isDML(qs.StatementType) {
				stats.RowsAffected = qs.NumDMLAffectedRows
			}
		}
		stats.Duration = time.Since(start)
		return driver.NewSliceResultSet(nil, nil, stats), nil
	}
	limit := max(q.Limit, 0)
	if limit > 0 {
		it.PageInfo().MaxSize = min(limit+1, 10000)
	}
	r := &resultSet{c: c, op: op, it: it, schema: it.Schema, ictx: ictx, cancel: icancel, start: start, limit: limit}
	r.cols = make([]driver.Column, len(it.Schema))
	for i, fs := range it.Schema {
		r.cols[i] = driver.Column{Name: fs.Name, NativeType: nativeType(fs), Type: fieldType(fs)}
	}
	return r, nil
}

func isDML(st string) bool {
	switch st {
	case "INSERT", "UPDATE", "DELETE", "MERGE":
		return true
	}
	return false
}

// cancelJob asks the server, in the background, to cancel a job whose
// query was cancelled. Close waits for it. job is the inserted job when
// the insert answered: it carries the job's location, without which
// BigQuery cannot find a job outside the US and EU multi-regions. When the
// insert was cut off, the job is looked up by id in the DSN's location.
func (c *conn) cancelJob(id string, job *bq.Job) {
	c.cancelMu.Lock()
	if c.closed.Load() {
		c.cancelMu.Unlock()
		return
	}
	c.cancels.Add(1)
	c.cancelMu.Unlock()
	go func() {
		defer c.cancels.Done()
		ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
		defer cancel()
		if job == nil {
			var err error
			if job, err = c.client.JobFromIDLocation(ctx, id, c.client.Location); err != nil {
				return // never created, or already gone
			}
		}
		if err := job.Cancel(ctx); err != nil {
			c.cfg.Log().Debug("bigquery: cancelling a job failed", "job", id, "err", scrubMessage(err.Error(), c.cfg, c.secrets))
		}
	}()
}

// resultSet reads a query job's rows. It is for a single consumer.
type resultSet struct {
	c      *conn
	op     string
	it     *bq.RowIterator
	schema bq.Schema
	cols   []driver.Column
	ictx   context.Context
	cancel context.CancelFunc
	start  time.Time
	limit  int
	n      int
	stats  driver.Stats
	cut    bool // a value was cut at maxValueBytes (warned once)
	done   bool
}

func (r *resultSet) Columns() []driver.Column { return r.cols }

func (r *resultSet) Next(ctx context.Context) (driver.Row, bool, error) {
	if r.done {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		r.finish()
		return nil, false, driver.Wrap(nil, Name, r.op, err)
	}
	stop := context.AfterFunc(ctx, r.cancel)
	defer stop()
	var vals []bq.Value
	err := r.it.Next(&vals)
	if errors.Is(err, iterator.Done) {
		r.finish()
		return nil, false, nil
	}
	if err != nil {
		r.finish()
		return nil, false, r.c.fail(ctx, r.op, err)
	}
	if r.limit > 0 && r.n >= r.limit {
		r.stats.Truncated = true
		r.finish()
		return nil, false, nil
	}
	r.n++
	row := make(driver.Row, len(r.schema))
	for i, fs := range r.schema {
		var v bq.Value
		if i < len(vals) {
			v = vals[i]
		}
		var cut bool
		row[i], cut = toValueCut(fs, v)
		if cut && !r.cut {
			r.cut = true
			r.stats.Warnings = append(r.stats.Warnings, cutWarning)
		}
	}
	return row, true, nil
}

func (r *resultSet) finish() {
	if r.done {
		return
	}
	r.done = true
	r.cancel()
	r.stats.RowsAffected = -1
	r.stats.Duration = time.Since(r.start)
}

func (r *resultSet) Stats() driver.Stats { return r.stats }

func (r *resultSet) Close() error {
	r.finish()
	return nil
}

// toParams converts positional parameters. NULL binds as a NULL STRING
// (BigQuery needs a type); lists, objects and vectors are not supported.
func toParams(op string, params []driver.Value) ([]bq.QueryParameter, error) {
	out := make([]bq.QueryParameter, len(params))
	for i, p := range params {
		v, err := toParam(p)
		if err != nil {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("parameter %d: %w", i+1, err))
		}
		out[i] = bq.QueryParameter{Value: v}
	}
	return out, nil
}

func toParam(v driver.Value) (any, error) {
	switch v.Type() {
	case driver.TypeNull:
		return bq.NullString{}, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return b, nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		return n, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return f, nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			return nil, errors.New("not a decimal number")
		}
		return r, nil
	case driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return b, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t, nil
	case driver.TypeJSON:
		b, _ := v.AsJSON()
		return bq.NullJSON{JSONVal: string(b), Valid: true}, nil
	}
	return nil, fmt.Errorf("%s values cannot be bound", v.Type())
}
