package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// The Mutator edits exactly one key per call. A key is given as
// {_key: "user:1"}, or, in a prefix collection, as one member whose value
// is the key's suffix ({id: 7} in "user" is "user:7"). Every call runs
// WATCH + MULTI/EXEC on its key, so a concurrent change makes it fail with
// ErrConflict instead of half-applying.
//
// Values:
//   - value (with an optional type) sets the whole value: a string, an
//     Object for a hash, a List for a list or set, a List of {member,
//     score} Objects for a zset, or JSON for a RedisJSON document (type
//     ReJSON-RL). Without a type, the value's own shape decides.
//   - ttl sets the expiry in seconds; NULL or -1 removes it.
//   - any other member is a hash field (a record collection's column); a
//     NULL field is deleted on update and skipped on insert.

var errNotHash = errors.New("field changes need a hash")

func (c *conn) mutateCheck(ctx context.Context, op string) error {
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if c.readOnly {
		return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the connection is read-only"))
	}
	return nil
}

func mutErr(kind error, op, format string, a ...any) error {
	return driver.Wrap(kind, Name, op, fmt.Errorf(format, a...))
}

// resolveKey turns a Mutator key into the Redis key, checking it belongs to
// the collection.
func resolveKey(op string, ref driver.CollectionRef, key driver.Object) (string, error) {
	var k string
	if v, ok := key.Get(colKey); ok {
		s, err := keyText(v)
		if err != nil {
			return "", mutErr(driver.ErrSyntax, op, "_key: %v", err)
		}
		k = s
	} else {
		if len(key) != 1 {
			return "", mutErr(driver.ErrSyntax, op, "a key is _key or one id field, got %d members", len(key))
		}
		if ref.Name == allKeys {
			return "", mutErr(driver.ErrSyntax, op, "in collection %q a key must be given as _key", allKeys)
		}
		s, err := keyText(key[0].Value)
		if err != nil {
			return "", mutErr(driver.ErrSyntax, op, "%s: %v", key[0].Key, err)
		}
		k = ref.Name + ":" + s
	}
	if !inCollection(ref.Name, k) {
		return "", mutErr(driver.ErrSyntax, op, "key %q is not in collection %q", k, ref.Name)
	}
	return k, nil
}

func keyText(v driver.Value) (string, error) {
	switch v.Type() {
	case driver.TypeNull, driver.TypeList, driver.TypeObject, driver.TypeVector, driver.TypeUnknown:
		return "", fmt.Errorf("a key cannot be %s", v.Type())
	}
	s, err := encodeValue(v)
	if err == nil && s == "" {
		err = errors.New("a key cannot be empty")
	}
	return s, err
}

// change is a Mutator input split into its parts.
type change struct {
	value    driver.Value
	hasValue bool
	typ      string
	ttl      driver.Value
	hasTTL   bool
	fields   driver.Object // hash fields, NULLs included
}

func splitChange(op string, o driver.Object) (change, error) {
	var ch change
	for _, m := range o {
		switch m.Key {
		case colKey, colTruncated: // _truncated is read-only row metadata
		case colValue:
			ch.value, ch.hasValue = m.Value, !m.Value.IsNull()
		case colType:
			if !m.Value.IsNull() {
				s, ok := m.Value.AsText()
				if !ok {
					return ch, mutErr(driver.ErrSyntax, op, "type must be text")
				}
				ch.typ = canonicalType(s)
			}
		case colTTL:
			ch.ttl, ch.hasTTL = m.Value, true
		default:
			ch.fields = append(ch.fields, m)
		}
	}
	return ch, nil
}

// canonicalType spells a type as TYPE reports it: lower case, with json
// and any casing of ReJSON-RL as ReJSON-RL.
func canonicalType(t string) string {
	l := strings.ToLower(t)
	if l == "json" || l == strings.ToLower(redisJSONType) {
		return redisJSONType
	}
	return l
}

func (ch change) nonNullFields() driver.Object {
	var out driver.Object
	for _, m := range ch.fields {
		if !m.Value.IsNull() {
			out = append(out, m)
		}
	}
	return out
}

// Insert creates one key. Without _key, a record collection's id field
// names it ("<collection>:<id>"). An existing key is ErrConstraint.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	if err := c.mutateCheck(ctx, op); err != nil {
		return nil, err
	}
	cl, _, release, err := c.client(op, ref.Namespace)
	if err != nil {
		return nil, err
	}
	defer release()
	ch, err := splitChange(op, values)
	if err != nil {
		return nil, err
	}
	fields := ch.nonNullFields()
	if len(fields) > 0 && ch.hasValue {
		return nil, mutErr(driver.ErrSyntax, op, "give either value or hash fields, not both")
	}
	keyObj := driver.Object{}
	if v, ok := values.Get(colKey); ok && !v.IsNull() {
		keyObj = driver.Object{{Key: colKey, Value: v}}
	} else if ref.Name != allKeys {
		rec, _, _, err := sampleRecords(ctx, cl, ref.Name)
		if err != nil {
			return nil, mapError(ctx, op, err)
		}
		if rec != nil && rec.idField != "" {
			if v, ok := values.Get(rec.idField); ok && !v.IsNull() {
				keyObj = driver.Object{{Key: rec.idField, Value: v}}
			}
		}
	}
	if len(keyObj) == 0 {
		return nil, mutErr(driver.ErrSyntax, op, "insert needs _key (or the id field of a record collection)")
	}
	key, err := resolveKey(op, ref, keyObj)
	if err != nil {
		return nil, err
	}
	var queue func(p goredis.Pipeliner) error
	switch {
	case len(fields) > 0:
		if ch.typ != "" && ch.typ != "hash" {
			return nil, mutErr(driver.ErrSyntax, op, "hash fields need type hash, got %q", ch.typ)
		}
		args, err := hashArgs(op, fields)
		if err != nil {
			return nil, err
		}
		queue = func(p goredis.Pipeliner) error { p.HSet(ctx, key, args...); return nil }
	case ch.hasValue:
		queue = func(p goredis.Pipeliner) error { return writeValue(ctx, p, op, key, ch.typ, ch.value) }
	default:
		return nil, mutErr(driver.ErrSyntax, op, "nothing to insert: give value or hash fields")
	}
	err = cl.Watch(ctx, func(tx *goredis.Tx) error {
		n, err := tx.Exists(ctx, key).Result()
		if err != nil {
			return err
		}
		if n > 0 {
			return mutErr(driver.ErrConstraint, op, "key %q already exists", key)
		}
		_, err = tx.TxPipelined(ctx, func(p goredis.Pipeliner) error {
			if err := queue(p); err != nil {
				return err
			}
			if ch.hasTTL {
				return queueTTL(ctx, p, op, key, ch.ttl)
			}
			return nil
		})
		return err
	}, key)
	if err != nil {
		return nil, txError(ctx, op, err)
	}
	return driver.Object{{Key: colKey, Value: textValue(key)}}, nil
}

// Update changes one existing key: its value, its ttl or its hash fields.
// A missing key is ErrNotFound. Replacing the value keeps the expiry unless
// ttl is also given.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	if err := c.mutateCheck(ctx, op); err != nil {
		return err
	}
	cl, _, release, err := c.client(op, ref.Namespace)
	if err != nil {
		return err
	}
	defer release()
	k, err := resolveKey(op, ref, key)
	if err != nil {
		return err
	}
	ch, err := splitChange(op, changes)
	if err != nil {
		return err
	}
	if v, ok := changes.Get(colKey); ok {
		if s, err := keyText(v); err != nil || s != k {
			return mutErr(driver.ErrUnsupported, op, "renaming a key is not supported")
		}
	}
	err = cl.Watch(ctx, func(tx *goredis.Tx) error {
		typ, err := tx.Type(ctx, k).Result()
		if err != nil {
			return err
		}
		if typ == "none" {
			return mutErr(driver.ErrNotFound, op, "key %q does not exist", k)
		}
		pttl, err := tx.PTTL(ctx, k).Result()
		if err != nil {
			return err
		}
		newTyp := typ
		if ch.typ != "" {
			newTyp = ch.typ
		}
		switch {
		case !ch.hasValue && ch.typ != "" && ch.typ != typ:
			return mutErr(driver.ErrSyntax, op, "changing the type to %q needs a value", ch.typ)
		case ch.hasValue && len(ch.nonNullFields()) > 0:
			return mutErr(driver.ErrSyntax, op, "give either value or hash fields, not both")
		case len(ch.fields) > 0 && newTyp != "hash":
			return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("%w: %q is a %s", errNotHash, k, newTyp))
		}
		if ch.hasValue || len(ch.fields) > 0 {
			if err := refuseLargeValue(ctx, tx, op, k, typ); err != nil {
				return err
			}
		}
		var set []any
		var del []string
		for _, m := range ch.fields {
			if m.Value.IsNull() {
				del = append(del, m.Key)
				continue
			}
			s, err := encodeValue(m.Value)
			if err != nil {
				return mutErr(driver.ErrSyntax, op, "field %s: %v", m.Key, err)
			}
			set = append(set, m.Key, s)
		}
		_, err = tx.TxPipelined(ctx, func(p goredis.Pipeliner) error {
			if ch.hasValue {
				if ch.typ == "" && typ != "string" && typ != "hash" && typ != "list" && typ != "set" && typ != "zset" && typ != redisJSONType {
					return mutErr(driver.ErrUnsupported, op, "replacing a %s value is not supported", typ)
				}
				p.Del(ctx, k)
				if err := writeValue(ctx, p, op, k, newTyp, ch.value); err != nil {
					return err
				}
				if !ch.hasTTL && pttl > 0 {
					p.PExpire(ctx, k, pttl)
				}
			}
			if len(set) > 0 {
				p.HSet(ctx, k, set...)
			}
			if len(del) > 0 {
				p.HDel(ctx, k, del...)
			}
			if ch.hasTTL {
				return queueTTL(ctx, p, op, k, ch.ttl)
			}
			return nil
		})
		return err
	}, k)
	return txError(ctx, op, err)
}

// errTooLarge is the message of an inline edit of a value larger than its
// preview.
const errTooLarge = "value is too large to edit inline; use a query command"

// refuseLargeValue fails with ErrConflict when key's current value is larger
// than a preview shows (over previewBytes of a string, previewItems
// elements, maxJSONPreview bytes of JSON): any inline edit of it may be
// derived from a cut preview. It runs inside the WATCH, so it sizes the
// value being replaced.
func refuseLargeValue(ctx context.Context, tx *goredis.Tx, op, key, typ string) error {
	var build func() (driver.Value, bool)
	if _, err := tx.Pipelined(ctx, func(p goredis.Pipeliner) error {
		build = queuePreview(ctx, p, key, typ)
		return nil
	}); err != nil && !errors.Is(err, goredis.Nil) {
		return err
	}
	if _, cut := build(); cut {
		return driver.Wrap(driver.ErrConflict, Name, op, errors.New(errTooLarge))
	}
	return nil
}

// Delete removes one key. A missing key is ErrNotFound.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	if err := c.mutateCheck(ctx, op); err != nil {
		return err
	}
	cl, _, release, err := c.client(op, ref.Namespace)
	if err != nil {
		return err
	}
	defer release()
	k, err := resolveKey(op, ref, key)
	if err != nil {
		return err
	}
	n, err := cl.Del(ctx, k).Result()
	if err != nil {
		return mapError(ctx, op, err)
	}
	if n == 0 {
		return mutErr(driver.ErrNotFound, op, "key %q does not exist", k)
	}
	return nil
}

// txError maps a WATCH transaction's error: a conflicting change is
// ErrConflict.
func txError(ctx context.Context, op string, err error) error {
	if errors.Is(err, goredis.TxFailedErr) {
		return mutErr(driver.ErrConflict, op, "the key changed during the edit; try again")
	}
	return mapError(ctx, op, err)
}

func hashArgs(op string, fields driver.Object) ([]any, error) {
	args := make([]any, 0, 2*len(fields))
	for _, m := range fields {
		s, err := encodeValue(m.Value)
		if err != nil {
			return nil, mutErr(driver.ErrSyntax, op, "field %s: %v", m.Key, err)
		}
		args = append(args, m.Key, s)
	}
	return args, nil
}

// queueTTL queues the expiry change for ttl: seconds > 0 sets it, NULL or
// -1 removes it.
func queueTTL(ctx context.Context, p goredis.Pipeliner, op, key string, ttl driver.Value) error {
	if ttl.IsNull() {
		p.Persist(ctx, key)
		return nil
	}
	n, ok := ttl.AsInt()
	if !ok {
		if s, isText := ttl.AsText(); isText && isCanonicalInt(s) {
			n, ok = typedValue(s, driver.TypeInt).AsInt()
		}
	}
	switch {
	case !ok:
		return mutErr(driver.ErrSyntax, op, "ttl must be a number of seconds")
	case n == -1:
		p.Persist(ctx, key)
	case n > 0:
		p.Expire(ctx, key, time.Duration(n)*time.Second)
	default:
		return mutErr(driver.ErrSyntax, op, "ttl must be positive, or -1 for no expiry")
	}
	return nil
}

// writeValue queues the commands that write v as a value of type typ ("" =
// v's own shape) to an absent (or just deleted) key.
func writeValue(ctx context.Context, p goredis.Pipeliner, op, key, typ string, v driver.Value) error {
	typ = canonicalType(typ)
	if typ == "" {
		switch v.Type() {
		case driver.TypeObject:
			typ = "hash"
		case driver.TypeList:
			typ = "list"
		default:
			typ = "string"
		}
	}
	empty := mutErr(driver.ErrSyntax, op, "Redis cannot store an empty %s", typ)
	switch typ {
	case "string":
		s, err := encodeValue(v)
		if err != nil {
			return mutErr(driver.ErrSyntax, op, "value: %v", err)
		}
		p.Set(ctx, key, s, 0)
	case "hash":
		obj, ok := v.AsObject()
		if !ok {
			return mutErr(driver.ErrSyntax, op, "a hash value is an object")
		}
		var fields driver.Object
		for _, m := range obj {
			if !m.Value.IsNull() {
				fields = append(fields, m)
			}
		}
		if len(fields) == 0 {
			return empty
		}
		args, err := hashArgs(op, fields)
		if err != nil {
			return err
		}
		p.HSet(ctx, key, args...)
	case "list", "set":
		items, err := listItems(op, v)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return empty
		}
		if typ == "list" {
			p.RPush(ctx, key, items...)
		} else {
			p.SAdd(ctx, key, items...)
		}
	case "zset":
		zs, err := zsetItems(op, v)
		if err != nil {
			return err
		}
		if len(zs) == 0 {
			return empty
		}
		p.ZAdd(ctx, key, zs...)
	case redisJSONType:
		var doc string
		if raw, ok := v.AsJSON(); ok {
			doc = string(raw)
		} else if s, ok := v.AsText(); ok && v.Type() == driver.TypeString {
			doc = s
		} else {
			raw, err := v.MarshalJSON()
			if err != nil {
				return mutErr(driver.ErrSyntax, op, "value: %v", err)
			}
			doc = string(raw)
		}
		p.Do(ctx, "JSON.SET", key, "$", doc)
	default:
		return mutErr(driver.ErrUnsupported, op, "writing a %s value is not supported", typ)
	}
	return nil
}

func listItems(op string, v driver.Value) ([]any, error) {
	list, ok := v.AsList()
	if !ok {
		return nil, mutErr(driver.ErrSyntax, op, "a list or set value is a list")
	}
	items := make([]any, 0, len(list))
	for _, e := range list {
		s, err := encodeValue(e)
		if err != nil {
			return nil, mutErr(driver.ErrSyntax, op, "element: %v", err)
		}
		items = append(items, s)
	}
	return items, nil
}

// zsetItems reads a List of {member, score} Objects, or an Object of
// member -> score.
func zsetItems(op string, v driver.Value) ([]goredis.Z, error) {
	bad := mutErr(driver.ErrSyntax, op, "a zset value is a list of {member, score} objects")
	score := func(x driver.Value) (float64, bool) {
		if f, ok := x.AsFloat(); ok {
			return f, true
		}
		if r, ok := ratOf(x); ok {
			f, _ := r.Float64()
			return f, true
		}
		return 0, false
	}
	if obj, ok := v.AsObject(); ok {
		zs := make([]goredis.Z, 0, len(obj))
		for _, m := range obj {
			f, ok := score(m.Value)
			if !ok {
				return nil, bad
			}
			zs = append(zs, goredis.Z{Member: m.Key, Score: f})
		}
		return zs, nil
	}
	list, ok := v.AsList()
	if !ok {
		return nil, bad
	}
	zs := make([]goredis.Z, 0, len(list))
	for _, e := range list {
		o, ok := e.AsObject()
		if !ok {
			return nil, bad
		}
		mv, okM := o.Get("member")
		sv, okS := o.Get("score")
		if !okM || !okS {
			return nil, bad
		}
		m, err := encodeValue(mv)
		f, okF := score(sv)
		if err != nil || !okF {
			return nil, bad
		}
		zs = append(zs, goredis.Z{Member: m, Score: f})
	}
	return zs, nil
}
