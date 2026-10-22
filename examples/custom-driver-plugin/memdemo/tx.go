package memdemo

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

// Tx is a snapshot transaction. It reads and writes a private copy of the
// store taken at Begin, so it sees none of the writes made after Begin
// and nobody sees its writes before Commit. Commit installs the copy, or
// fails with driver.ErrConflict when the store changed since Begin (first
// committer wins). It is also a Mutator and an Explainer.
type Tx struct {
	s        *store
	readOnly bool

	mu      sync.Mutex
	d       data   // the snapshot; nil once finished
	version uint64 // the store version the snapshot is at
	dirty   bool   // the snapshot has writes
	state   txState
}

type txState int

const (
	txOpen txState = iota
	txCommitted
	txRolledBack
)

var (
	_ driver.Tx        = (*Tx)(nil)
	_ driver.Mutator   = (*Tx)(nil)
	_ driver.Explainer = (*Tx)(nil)
)

// do runs fn on the snapshot, with the same guards a Conn has. write marks
// the snapshot changed when fn succeeds.
func (t *Tx) do(ctx context.Context, op string, write bool, fn func(data) error) error {
	if err := ctx.Err(); err != nil {
		return driver.Wrap(nil, Name, op, err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != txOpen {
		return driver.Wrap(driver.ErrConflict, Name, op, errors.New("the transaction is finished"))
	}
	if write && t.readOnly {
		return errReadOnly(op)
	}
	if err := fn(t.d); err != nil {
		return err
	}
	t.dirty = t.dirty || write
	return nil
}

// Query implements driver.Querier.
func (t *Tx) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	st, err := prepare(q, t.readOnly)
	if err != nil {
		return nil, err
	}
	if st.verb == "sleep" {
		// Check the Tx is open, then sleep outside t.mu, so a sleeping
		// query never blocks the Tx.
		if err := t.do(ctx, "query", false, func(data) error { return nil }); err != nil {
			return nil, err
		}
		if err := pause(ctx, st.sleep); err != nil {
			return nil, err
		}
		return result{affected: -1}.resultSet(0, start), nil
	}
	var res result
	err = t.do(ctx, "query", st.writes(), func(d data) (err error) {
		res, err = exec(d, st)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res.resultSet(q.Limit, start), nil
}

// Insert implements driver.Mutator.
func (t *Tx) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	var key driver.Object
	err := t.do(ctx, "insert", true, func(d data) (err error) {
		key, err = insert(d, ref, values)
		return err
	})
	return key, err
}

// Update implements driver.Mutator.
func (t *Tx) Update(ctx context.Context, ref driver.CollectionRef, key, changes driver.Object) error {
	return t.do(ctx, "update", true, func(d data) error { return update(d, ref, key, changes) })
}

// Delete implements driver.Mutator.
func (t *Tx) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	return t.do(ctx, "delete", true, func(d data) error { return remove(d, ref, key) })
}

// Explain implements driver.Explainer.
func (t *Tx) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	var plan *driver.Plan
	err := t.do(ctx, "explain", false, func(d data) (err error) {
		plan, err = explain(d, q, opts, t.readOnly)
		return err
	})
	return plan, err
}

// Commit implements driver.Tx. A transaction without writes commits
// without touching the store. After a failed Commit the transaction stays
// open, so it can still be rolled back.
func (t *Tx) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return driver.Wrap(nil, Name, "commit", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != txOpen {
		return driver.Wrap(driver.ErrConflict, Name, "commit", errors.New("the transaction is finished"))
	}
	if t.dirty {
		if err := t.s.commit(t.d, t.version); err != nil {
			return err
		}
	}
	t.state, t.d = txCommitted, nil
	return nil
}

// Rollback implements driver.Tx. It is a no-op once the transaction is
// finished, after Commit included.
func (t *Tx) Rollback(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == txOpen {
		t.state, t.d = txRolledBack, nil
	}
	return nil
}
