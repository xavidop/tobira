package firestore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	fs "cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Mutator = (*conn)(nil)

// errReadOnlyConn refuses a mutation on a read-only connection.
func errReadOnlyConn(op string) error {
	return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the connection is read-only"))
}

// checkDocID refuses an id Firestore cannot hold.
func checkDocID(op, id string) error {
	if id == "" || strings.Contains(id, "/") || id == "." || id == ".." || strings.HasPrefix(id, "__") && strings.HasSuffix(id, "__") {
		return syntaxErr(op, fmt.Errorf("invalid document id %q", id))
	}
	return nil
}

// Insert creates one document and returns its id as the key {_id: id}. A
// String _id member names the document; without one Firestore picks a
// random id. Every other member is a field. The write is a create, so an
// id in use is ErrConflict and an existing document is never replaced.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if c.readOnly {
		return nil, errReadOnlyConn(op)
	}
	coll, err := c.target(op, ref)
	if err != nil {
		return nil, err
	}
	doc := coll.NewDoc()
	fields := driver.Object{}
	for _, m := range values {
		if m.Key != idColumn {
			fields = append(fields, driver.Member{Key: storedName(m.Key), Value: m.Value})
			continue
		}
		id, ok := m.Value.AsText()
		if !ok || m.Value.Type() != driver.TypeString {
			return nil, syntaxErr(op, errors.New("_id names the document: give a string"))
		}
		if err := checkDocID(op, id); err != nil {
			return nil, err
		}
		doc = coll.Doc(id)
	}
	data, err := mapOf(fields, 1)
	if err != nil {
		return nil, withOp(err, op)
	}
	if _, err := doc.Create(ctx, data); err != nil {
		return nil, c.fail(ctx, op, err)
	}
	return driver.Object{{Key: idColumn, Value: driver.String(doc.ID)}}, nil
}

// Update sets the changed fields of the one document key names. Each
// change is one top-level field (a name with dots is one field, not a
// path; _id_field is the stored field _id); NULL stores null. _id cannot change. A key of {_id: id} names the
// document and the write requires it to exist; any other key is matched
// with equality filters, and the write requires the matched document to be
// unchanged since it was read. No match is ErrNotFound, several are
// ErrConflict, and so is a document changed in between.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if c.readOnly {
		return errReadOnlyConn(op)
	}
	if len(changes) == 0 {
		return syntaxErr(op, errors.New("no changes"))
	}
	ups := make([]fs.Update, 0, len(changes))
	seen := map[string]bool{}
	for _, m := range changes {
		if m.Key == idColumn {
			return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("_id is the document id and cannot change"))
		}
		if m.Key == "" || seen[m.Key] {
			return syntaxErr(op, fmt.Errorf("invalid or repeated field %q", m.Key))
		}
		seen[m.Key] = true
		v, err := toFirestore(m.Value)
		if err != nil {
			return withOp(err, op)
		}
		ups = append(ups, fs.Update{FieldPath: fs.FieldPath{storedName(m.Key)}, Value: v})
	}
	doc, read, err := c.resolveKey(ctx, op, ref, key)
	if err != nil {
		return err
	}
	var pre []fs.Precondition // an update requires the document to exist anyway
	if read != nil {
		pre = append(pre, fs.LastUpdateTime(*read))
	}
	if _, err := doc.Update(ctx, ups, pre...); err != nil {
		return c.fail(ctx, op, err)
	}
	return nil
}

// Delete removes the one document key names, resolved as Update does. The
// delete requires the document to exist (and, for a field key, to be
// unchanged since it was read).
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if c.readOnly {
		return errReadOnlyConn(op)
	}
	doc, read, err := c.resolveKey(ctx, op, ref, key)
	if err != nil {
		return err
	}
	pre := fs.Exists
	if read != nil {
		pre = fs.LastUpdateTime(*read)
	}
	if _, err := doc.Delete(ctx, pre); err != nil {
		return c.fail(ctx, op, err)
	}
	return nil
}

// target returns the collection a mutation writes to.
func (c *conn) target(op string, ref driver.CollectionRef) (*fs.CollectionRef, error) {
	if err := c.checkNamespace(op, ref.Namespace); err != nil {
		return nil, err
	}
	return c.collection(op, ref.Name)
}

// resolveKey finds the document a key names. For {_id: id} that is the
// document with that id, unread (read is nil); for other keys it is the
// one document whose fields equal the key's (at most two are read), with
// the update time it was read at, which the write then requires.
func (c *conn) resolveKey(ctx context.Context, op string, ref driver.CollectionRef, key driver.Object) (doc *fs.DocumentRef, read *time.Time, err error) {
	coll, err := c.target(op, ref)
	if err != nil {
		return nil, nil, err
	}
	if len(key) == 0 {
		return nil, nil, syntaxErr(op, errors.New("empty key"))
	}
	if len(key) == 1 && key[0].Key == idColumn {
		id, ok := key[0].Value.AsText()
		if !ok || key[0].Value.Type() != driver.TypeString {
			return nil, nil, syntaxErr(op, errors.New("_id is a document id: give a string"))
		}
		if err := checkDocID(op, id); err != nil {
			return nil, nil, err
		}
		return coll.Doc(id), nil, nil
	}
	var where []whereClause
	for _, m := range key {
		cl, err := conditionClauses(driver.Condition{Field: m.Key, Op: driver.OpEq, Value: m.Value})
		if err != nil {
			return nil, nil, withOp(err, op)
		}
		if m.Value.IsNull() {
			cl[0].value = nil
		}
		where = append(where, cl...)
	}
	q, err := c.applyWhere(op, coll.Query, coll, where)
	if err != nil {
		return nil, nil, err
	}
	it := q.Limit(2).Documents(ctx)
	defer it.Stop()
	var snaps []*fs.DocumentSnapshot
	for {
		s, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, nil, c.fail(ctx, op, err)
		}
		snaps = append(snaps, s)
	}
	switch len(snaps) {
	case 0:
		return nil, nil, driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no document matches the key"))
	case 1:
		return snaps[0].Ref, &snaps[0].UpdateTime, nil
	}
	return nil, nil, driver.Wrap(driver.ErrConflict, Name, op, errors.New("the key matches more than one document"))
}
