package mongodb

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Mutator = (*conn)(nil)

// errReadOnlyConn refuses a mutation on a read-only connection.
func errReadOnlyConn(op string) error {
	return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the connection is read-only"))
}

// Insert inserts one document (insertOne) and returns its _id as the key.
// Without an _id in values, the client assigns an ObjectId.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if c.readOnly {
		return nil, errReadOnlyConn(op)
	}
	doc, err := objectToBSON(values)
	if err != nil {
		return nil, err
	}
	res, err := c.database(ref.Namespace).Collection(ref.Name).InsertOne(ctx, doc)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	t, b, err := bson.MarshalValue(res.InsertedID)
	if err != nil {
		return nil, driver.Wrap(nil, Name, op, err)
	}
	return driver.Object{{Key: "_id", Value: valueOf(bson.RawValue{Type: t, Value: b})}}, nil
}

// Update changes the one document key matches: updateOne with $set for each
// change; a change to NULL stores null, as NULL means everywhere in tobira
// (removing a field is not supported). A field name containing "." is
// ErrUnsupported, since $set would read it as a nested path; _id cannot
// change. The write filters by the resolved _id and the whole key again. No match is ErrNotFound, several are ErrConflict.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if c.readOnly {
		return errReadOnlyConn(op)
	}
	upd, err := updateDocument(changes)
	if err != nil {
		return err
	}
	coll := c.database(ref.Namespace).Collection(ref.Name)
	filter, err := keyFilter(key)
	if err != nil {
		return err
	}
	id, err := resolveKey(ctx, op, coll, filter)
	if err != nil {
		return err
	}
	res, err := coll.UpdateOne(ctx, writeFilter(id, filter), upd)
	if err != nil {
		return mapError(ctx, op, err)
	}
	if res.MatchedCount == 0 {
		return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no document matches the key"))
	}
	return nil
}

// Delete removes the one document key matches (deleteOne). No match is
// ErrNotFound, several are ErrConflict.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if c.readOnly {
		return errReadOnlyConn(op)
	}
	coll := c.database(ref.Namespace).Collection(ref.Name)
	filter, err := keyFilter(key)
	if err != nil {
		return err
	}
	id, err := resolveKey(ctx, op, coll, filter)
	if err != nil {
		return err
	}
	res, err := coll.DeleteOne(ctx, writeFilter(id, filter))
	if err != nil {
		return mapError(ctx, op, err)
	}
	if res.DeletedCount == 0 {
		return driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no document matches the key"))
	}
	return nil
}

// resolveKey finds the _id of the one document the key filter matches: it
// reads at most two matching _ids. The key is normally {_id: …}, but any
// fields work.
func resolveKey(ctx context.Context, op string, coll *mongo.Collection, filter bson.D) (bson.RawValue, error) {
	cur, err := coll.Find(ctx, filter, options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}).SetLimit(2))
	if err != nil {
		return bson.RawValue{}, mapError(ctx, op, err)
	}
	defer closeCursor(cur)
	var ids []bson.RawValue
	for cur.Next(ctx) {
		ids = append(ids, bson.RawValue{Type: cur.Current.Lookup("_id").Type, Value: append([]byte(nil), cur.Current.Lookup("_id").Value...)})
	}
	if err := cur.Err(); err != nil {
		return bson.RawValue{}, mapError(ctx, op, err)
	}
	switch len(ids) {
	case 0:
		return bson.RawValue{}, driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no document matches the key"))
	case 1:
		return ids[0], nil
	}
	return bson.RawValue{}, driver.Wrap(driver.ErrConflict, Name, op, errors.New("the key matches more than one document"))
}

// writeFilter is the filter of the write itself: the resolved _id and the
// whole key again, so a key field that changed since resolveKey matches
// nothing (ErrNotFound) instead of writing.
func writeFilter(id bson.RawValue, key bson.D) bson.D {
	return bson.D{{Key: "$and", Value: bson.A{bson.D{{Key: "_id", Value: id}}, key}}}
}

// keyFilter builds the filter for a key: each field equal to its value (a
// 24-hex-digit String also matches the ObjectId it spells). A value holding
// a "$"-key at any depth (an object, a list element or JSON) is ErrSyntax:
// it would act as a query operator, not a value.
func keyFilter(key driver.Object) (bson.D, error) {
	if len(key) == 0 {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "key", errors.New("empty key"))
	}
	out := make(bson.D, 0, len(key))
	for _, m := range key {
		if err := checkField(m.Key); err != nil {
			return nil, err
		}
		if alts, ok := hexAlternatives(m.Value); ok {
			out = append(out, bson.E{Key: m.Key, Value: bson.D{{Key: "$in", Value: alts}}})
			continue
		}
		v, err := toBSON(m.Value)
		if err != nil {
			return nil, err
		}
		if hasOperatorKey(v) {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "key", fmt.Errorf("the value of key field %q holds a $-operator", m.Key))
		}
		out = append(out, bson.E{Key: m.Key, Value: v})
	}
	return out, nil
}

// hasOperatorKey reports whether a value holds a key starting with "$" at
// any depth.
func hasOperatorKey(v any) bool {
	switch x := v.(type) {
	case bson.D:
		for _, e := range x {
			if strings.HasPrefix(e.Key, "$") || hasOperatorKey(e.Value) {
				return true
			}
		}
	case bson.A:
		for _, e := range x {
			if hasOperatorKey(e) {
				return true
			}
		}
	}
	return false
}

// updateDocument turns changes into {$set: …}; a NULL change sets null.
func updateDocument(changes driver.Object) (bson.D, error) {
	if len(changes) == 0 {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "update", errors.New("no changes"))
	}
	var set bson.D
	for _, m := range changes {
		if err := checkField(m.Key); err != nil {
			return nil, err
		}
		if m.Key == "_id" {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "update", fmt.Errorf("_id cannot change"))
		}
		if strings.Contains(m.Key, ".") {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "update", fmt.Errorf(
				"field %q: names containing \".\" are not supported in edits (MongoDB would read it as a nested path); set the whole parent document instead", m.Key))
		}
		v, err := toBSON(m.Value)
		if err != nil {
			return nil, err
		}
		set = append(set, bson.E{Key: m.Key, Value: v})
	}
	return bson.D{{Key: "$set", Value: set}}, nil
}
