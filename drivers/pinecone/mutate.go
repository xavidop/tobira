package pinecone

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/xavidop/tobira/driver"
)

// The Mutator edits one record of a namespace. A key names it by "_id",
// or by metadata equality ({"sku": "A1"}), which must match exactly one
// record. Members named "values" set the dense vector; every other member
// is a metadata key.

// refuseReadOnly fails a write on a read-only connection before any RPC.
func (c *conn) refuseReadOnly(ctx context.Context, op string) error {
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if c.readOnly {
		return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the connection is read-only"))
	}
	return nil
}

// Insert adds one record. Its id is the "_id" member, or a new random UUID
// when there is none; "values" (the dense vector) is required. An id
// already in use is ErrConflict. Pinecone has no insert-only write, so the
// check is a fetch before the upsert: a record created by another client
// between the two is overwritten. NULL members are left
// out, since Pinecone metadata has no null.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return nil, err
	}
	vec, meta, err := splitMembers(values, op)
	if err != nil {
		return nil, err
	}
	if len(vec) == 0 {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New(`a Pinecone record needs its dense vector: give "values"`))
	}
	id := ""
	explicit := false
	if v, ok := values.Get(idColumn); ok && !v.IsNull() {
		if id, err = recordID(v, op); err != nil {
			return nil, err
		}
		explicit = true
	} else {
		id = newUUID()
	}
	h, _, err := c.collection(ctx, op, ref, false)
	if err != nil {
		return nil, err
	}
	ic := h.ns(pineconeNamespace(ref.Name))
	if explicit {
		recs, err := c.fetch(ctx, op, ic, []string{id})
		if err != nil {
			return nil, err
		}
		if len(recs) > 0 {
			return nil, driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("record %q already exists", id))
		}
	}
	if _, err := ic.UpsertVectors(ctx, []*pinecone.Vector{{Id: id, Values: &vec, Metadata: meta}}); err != nil {
		return nil, c.fail(ctx, op, err)
	}
	return driver.Object{{Key: idColumn, Value: driver.String(id)}}, nil
}

// Update sets metadata keys and the dense vector of one record; other keys
// keep their values. Pinecone cannot remove a metadata key in an update,
// so a NULL is ErrUnsupported. A missing record is ErrNotFound.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return err
	}
	for _, m := range changes {
		switch {
		case m.Key == idColumn:
			return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("a record id cannot change: insert a new record instead"))
		case m.Value.IsNull():
			return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%q: Pinecone cannot remove a metadata key in an update (re-insert the record without it)", m.Key))
		}
	}
	vec, meta, err := splitMembers(changes, op)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}
	if err := checkKey(key, op); err != nil {
		return err
	}
	ic, id, err := c.resolve(ctx, op, ref, key)
	if err != nil {
		return err
	}
	req := &pinecone.UpdateVectorRequest{Id: id, Values: vec}
	if len(meta.GetFields()) > 0 {
		req.Metadata = meta
	}
	return c.fail(ctx, op, ic.UpdateVector(ctx, req))
}

// Delete removes one record. A missing record is ErrNotFound.
//
// Update and Delete check that the record exists, then act: Pinecone has
// no conditional update or delete, and acting on a missing id succeeds
// silently, so a record deleted by another client in between is not
// reported.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return err
	}
	if err := checkKey(key, op); err != nil {
		return err
	}
	ic, id, err := c.resolve(ctx, op, ref, key)
	if err != nil {
		return err
	}
	return c.fail(ctx, op, ic.DeleteVectorsById(ctx, []string{id}))
}

// checkKey refuses a key that cannot name a record, before any RPC.
func checkKey(key driver.Object, op string) error {
	if len(key) == 0 {
		return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New(`a record is named by its "_id" or by metadata values`))
	}
	for _, m := range key {
		switch {
		case m.Key == idColumn && len(key) > 1:
			return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New(`a key with "_id" names the record by its id alone`))
		case m.Key == valuesColumn || m.Key == sparseColumn || m.Key == scoreColumn || m.Key == distanceColumn:
			return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%q cannot name a record", m.Key))
		case m.Value.IsNull():
			return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("key %q is NULL", m.Key))
		}
	}
	return nil
}

// resolve finds the one record key names in ref and returns its namespace
// connection and id. An "_id" key is checked with a fetch. A metadata key
// is looked up with a filtered query (a vector search restricted to the
// records whose metadata equal the key, top 2), which needs a dense index:
// none matching is ErrNotFound, two or more ErrConflict.
func (c *conn) resolve(ctx context.Context, op string, ref driver.CollectionRef, key driver.Object) (*pinecone.IndexConnection, string, error) {
	h, _, err := c.collection(ctx, op, ref, false)
	if err != nil {
		return nil, "", err
	}
	ic := h.ns(pineconeNamespace(ref.Name))
	if key[0].Key == idColumn {
		id, err := recordID(key[0].Value, op)
		if err != nil {
			return nil, "", err
		}
		recs, err := c.fetch(ctx, op, ic, []string{id})
		if err != nil {
			return nil, "", err
		}
		if len(recs) == 0 {
			return nil, "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("record %q does not exist", id))
		}
		return ic, id, nil
	}
	conds := make([]driver.Condition, len(key))
	for i, m := range key {
		conds[i] = driver.Condition{Field: m.Key, Op: driver.OpEq, Value: m.Value}
	}
	filter, err := pineconeFilter(driver.Filter{Conditions: conds})
	if err != nil {
		return nil, "", withOp(err, op)
	}
	info, err := c.stats(ctx, op, h)
	if err != nil {
		return nil, "", err
	}
	if info.sparse || info.dim <= 0 {
		return nil, "", driver.Wrap(driver.ErrUnsupported, Name, op, errors.New(`a sparse index finds records by "_id" only`))
	}
	probe := make([]float32, info.dim)
	probe[0] = 1 // any non-zero vector: the filter picks the records
	resp, err := ic.QueryByVectorValues(ctx, &pinecone.QueryByVectorValuesRequest{Vector: probe, TopK: 2, MetadataFilter: filter})
	if err != nil {
		return nil, "", c.fail(ctx, op, err)
	}
	matches := scored(resp)
	switch len(matches) {
	case 0:
		return nil, "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no record matches %s", describeKey(key)))
	case 1:
		return ic, matches[0].id, nil
	}
	return nil, "", driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("several records match %s: name one by its _id", describeKey(key)))
}

// describeKey renders a key for an error message.
func describeKey(key driver.Object) string {
	parts := make([]string, len(key))
	for i, m := range key {
		parts[i] = m.Key + " = " + m.Value.String()
	}
	return strings.Join(parts, ", ")
}

// recordID reads a record id: a String, or an Int as its decimal text.
// Pinecone ids are 1 to 512 bytes, without NUL.
func recordID(v driver.Value, op string) (string, error) {
	var id string
	switch v.Type() {
	case driver.TypeString:
		id, _ = v.AsText()
	case driver.TypeInt:
		n, _ := v.AsInt()
		id = strconv.FormatInt(n, 10)
	default:
		return "", driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("a record id is text, got %s", v.Type()))
	}
	if id == "" || len(id) > 512 || strings.ContainsRune(id, 0) {
		return "", driver.Wrap(driver.ErrSyntax, Name, op, errors.New("a record id is 1 to 512 bytes, without NUL"))
	}
	return id, nil
}

// splitMembers sorts members into the dense vector and the metadata. The
// _id member is skipped; NULL members are left out.
func splitMembers(members driver.Object, op string) ([]float32, *structpb.Struct, error) {
	var vec []float32
	meta := &structpb.Struct{Fields: map[string]*structpb.Value{}}
	for _, m := range members {
		switch m.Key {
		case idColumn:
			continue
		case valuesColumn:
			v, err := toVector(m.Value, op)
			if err != nil {
				return nil, nil, err
			}
			vec = v
			continue
		case sparseColumn:
			return nil, nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("sparse values cannot be edited here: write them with a native client"))
		case scoreColumn, distanceColumn:
			return nil, nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s is computed by a search, not stored", m.Key))
		}
		key := metaKey(m.Key) // meta_values is the metadata key "values"
		pv, err := toMeta(key, m.Value)
		if err != nil {
			return nil, nil, withOp(err, op)
		}
		if pv != nil {
			meta.Fields[key] = pv
		}
	}
	return vec, meta, nil
}

// toVector converts the dense vector: a Vector, or a List of numbers. NULL
// is no vector.
func toVector(val driver.Value, op string) ([]float32, error) {
	if val.IsNull() {
		return nil, nil
	}
	data, ok := val.AsVector()
	if !ok {
		items, isList := val.AsList()
		if !isList {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("values needs a list of numbers, got %s", val.Type()))
		}
		data = make([]float32, len(items))
		for i, it := range items {
			f, ok := it.AsFloat()
			if !ok {
				return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("values needs a list of numbers"))
			}
			data[i] = float32(f)
		}
	}
	if len(data) == 0 {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("values must not be empty"))
	}
	return data, nil
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// withOp returns a driver error with its Op replaced.
func withOp(err error, op string) error {
	var de *driver.Error
	if !errors.As(err, &de) {
		return err
	}
	out := *de
	out.Op = op
	return &out
}
