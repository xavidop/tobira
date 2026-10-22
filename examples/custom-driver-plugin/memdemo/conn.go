package memdemo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Conn is a connection to the driver's store. It is safe for concurrent
// use.
type Conn struct {
	s        *store
	readOnly bool
	closed   atomic.Bool
}

// The capabilities Info declares; drivertest checks they match.
var (
	_ driver.Browser    = (*Conn)(nil)
	_ driver.Mutator    = (*Conn)(nil)
	_ driver.Explainer  = (*Conn)(nil)
	_ driver.Transactor = (*Conn)(nil)
	_ driver.Completer  = (*Conn)(nil)
)

// check is the guard every call starts with: honour ctx, refuse a closed
// Conn.
func (c *Conn) check(ctx context.Context, op string) error {
	if err := ctx.Err(); err != nil {
		return driver.Wrap(nil, Name, op, err) // errors.Is(err, context.Canceled) still holds
	}
	if c.closed.Load() {
		return driver.Wrap(driver.ErrConnection, Name, op, errors.New("connection is closed"))
	}
	return nil
}

// checkWrite is check plus the read-only guard.
func (c *Conn) checkWrite(ctx context.Context, op string) error {
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if c.readOnly {
		return errReadOnly(op)
	}
	return nil
}

func errReadOnly(op string) error {
	return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("writes are refused"))
}

// Ping implements driver.Conn.
func (c *Conn) Ping(ctx context.Context) error { return c.check(ctx, "ping") }

// Close implements driver.Conn. It is idempotent.
func (c *Conn) Close() error {
	c.closed.Store(true)
	return nil
}

// Namespaces implements driver.Conn.
func (c *Conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	return []driver.Namespace{{Name: Namespace, Label: "namespace", Default: true}}, nil
}

// Collections implements driver.Conn.
func (c *Conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	if ns.Name != Namespace {
		return nil, driver.Wrap(driver.ErrNotFound, Name, "collections", fmt.Errorf("namespace %q", ns.Name))
	}
	var out []driver.Collection
	err := c.s.read(func(d data) error {
		for _, name := range d.names() {
			out = append(out, driver.Collection{
				Ref:         driver.CollectionRef{Namespace: Namespace, Name: name},
				Type:        driver.CollectionTable,
				RowEstimate: int64(len(d[name].rows)),
			})
		}
		return nil
	})
	return out, err
}

// Describe implements driver.Conn.
func (c *Conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	var schema *driver.CollectionSchema
	err := c.s.read(func(d data) error {
		t, err := d.table("describe", ref)
		if err != nil {
			return err
		}
		fields := make([]driver.Field, len(t.cols))
		for i, col := range t.cols {
			fields[i] = driver.Field{Name: col.Name, NativeType: col.NativeType, Type: col.Type, Nullable: !col.Key}
		}
		schema = &driver.CollectionSchema{
			Ref:         driver.CollectionRef{Namespace: Namespace, Name: ref.Name},
			Type:        driver.CollectionTable,
			Fields:      fields,
			PrimaryKey:  []string{t.key},
			RowEstimate: int64(len(t.rows)),
			SizeBytes:   -1,
		}
		return nil
	})
	return schema, err
}

// prepare parses q and applies the read-only guard: readOnly is the
// connection's (or transaction's) setting, and Query.ReadOnly adds to it.
// The guard looks at the parsed statement, so no query text can get past
// it.
func prepare(q driver.Query, readOnly bool) (statement, error) {
	if q.Namespace != "" && q.Namespace != Namespace {
		return statement{}, driver.Wrap(driver.ErrNotFound, Name, "query", fmt.Errorf("namespace %q", q.Namespace))
	}
	st, err := parse(q.Text, q.Params)
	if err != nil {
		return st, err
	}
	if st.writes() && (readOnly || q.ReadOnly) {
		return st, errReadOnly("query")
	}
	return st, nil
}

// resultSet turns a result into a ResultSet, applying Query.Limit.
func (r result) resultSet(limit int, start time.Time) driver.ResultSet {
	rows, truncated := r.rows, false
	if limit > 0 && len(rows) > limit {
		rows, truncated = rows[:limit], true
	}
	return driver.NewSliceResultSet(r.cols, rows, driver.Stats{
		RowsAffected: r.affected, Duration: time.Since(start), Truncated: truncated,
	})
}

// Query implements driver.Querier.
func (c *Conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	st, err := prepare(q, c.readOnly)
	if err != nil {
		return nil, err
	}
	if st.verb == "sleep" {
		// Sleep holds no lock, so it never blocks other callers.
		if err := pause(ctx, st.sleep); err != nil {
			return nil, err
		}
		return result{affected: -1}.resultSet(0, start), nil
	}
	var res result
	run := func(d data) (err error) {
		res, err = exec(d, st)
		return err
	}
	if st.writes() {
		err = c.s.write(run)
	} else {
		err = c.s.read(run)
	}
	if err != nil {
		return nil, err
	}
	return res.resultSet(q.Limit, start), nil
}

// Browse implements driver.Browser.
func (c *Conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	if err := c.check(ctx, "browse"); err != nil {
		return nil, err
	}
	var rs driver.ResultSet
	err := c.s.read(func(d data) (err error) {
		rs, err = browse(d, ref, opts)
		return err
	})
	return rs, err
}

// Insert implements driver.Mutator.
func (c *Conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	if err := c.checkWrite(ctx, "insert"); err != nil {
		return nil, err
	}
	var key driver.Object
	err := c.s.write(func(d data) (err error) {
		key, err = insert(d, ref, values)
		return err
	})
	return key, err
}

// Update implements driver.Mutator.
func (c *Conn) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	if err := c.checkWrite(ctx, "update"); err != nil {
		return err
	}
	return c.s.write(func(d data) error { return update(d, ref, key, changes) })
}

// Delete implements driver.Mutator.
func (c *Conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	if err := c.checkWrite(ctx, "delete"); err != nil {
		return err
	}
	return c.s.write(func(d data) error { return remove(d, ref, key) })
}

// Explain implements driver.Explainer.
func (c *Conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	if err := c.check(ctx, "explain"); err != nil {
		return nil, err
	}
	var plan *driver.Plan
	err := c.s.read(func(d data) (err error) {
		plan, err = explain(d, q, opts, c.readOnly)
		return err
	})
	return plan, err
}

// Complete implements driver.Completer.
func (c *Conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	var out []driver.Completion
	err := c.s.read(func(d data) error {
		out = complete(d, req)
		return nil
	})
	return out, err
}

// Begin implements driver.Transactor. A transaction works on a snapshot
// of the store taken now; see Tx.
func (c *Conn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.check(ctx, "begin"); err != nil {
		return nil, err
	}
	if opts.Isolation != "" && !strings.EqualFold(opts.Isolation, "snapshot") {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "begin",
			fmt.Errorf("isolation %q: only snapshot is offered", opts.Isolation))
	}
	d, version := c.s.snapshot()
	return &Tx{s: c.s, d: d, version: version, readOnly: c.readOnly || opts.ReadOnly}, nil
}
