package milvus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/client/v2/column"

	"github.com/xavidop/tobira/driver"
)

// The Mutator edits one entity, named by its primary key. Members named
// like a static field set that field; any other member is a dynamic key
// (a column "$meta.k" names key k). Milvus has no conditional write, so
// each edit checks first and then writes; an entity created or deleted in
// between is not detected.

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

// keyValue reads a key that names exactly one entity: {pk: value}.
func keyValue(s *collSchema, key driver.Object, op string) (driver.Value, error) {
	pk := s.pkField()
	if len(key) != 1 || key[0].Key != pk.name {
		return driver.Null(), driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("an entity is named by its primary key %q alone", pk.name))
	}
	return pkValue(pk, key[0].Value, op)
}

// exists reports whether the entity with primary key k exists, and
// returns its dynamic keys when withDynamic.
func (c *conn) exists(ctx context.Context, s *collSchema, op string, k driver.Value, withDynamic bool) (bool, []driver.Member, error) {
	pk := s.pkField()
	tv, err := templateValue(k)
	if err != nil {
		return false, nil, err
	}
	out := []string{pk.name}
	if withDynamic {
		out = append(out, dynamicField)
	}
	req := &milvuspb.QueryRequest{
		DbName:             s.db,
		CollectionName:     s.name,
		Expr:               pk.name + " == {tobira_k}",
		ExprTemplateValues: exprParams{"tobira_k": tv},
		OutputFields:       out,
		QueryParams:        []*commonpb.KeyValuePair{{Key: "limit", Value: "1"}},
		ConsistencyLevel:   commonpb.ConsistencyLevel_Strong,
	}
	recs, err := c.runQuery(ctx, s, op, false, req)
	if err != nil || len(recs) == 0 {
		return false, nil, err
	}
	return true, recs[0].dyn, nil
}

// entityColumns turns members into one-row columns: static fields, plus
// the dynamic keys merged over base into one $meta object. With fill (an
// insert), every nullable or defaulted field the members leave out gets a
// NULL.
func entityColumns(s *collSchema, members driver.Object, base []driver.Member, op string, fill bool) ([]*schemapb.FieldData, error) {
	var fields []*schemapb.FieldData
	seen := map[string]bool{}
	dyn := map[string]any{}
	for _, m := range base {
		x, err := goValue(m.Value)
		if err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("dynamic key %q: %v", m.Key, err))
		}
		dyn[m.Key] = x
	}
	hasDyn := len(base) > 0
	for _, m := range members {
		if f, ok := s.field(m.Key); ok {
			if f.output != "" {
				return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("field %q is computed by function %s: write its input instead", f.name, f.output))
			}
			col, err := columnFor(f, m.Value, op)
			if err != nil {
				return nil, err
			}
			fields = append(fields, fieldData(col))
			seen[f.name] = true
			continue
		}
		key, _ := s.dynamicKey(m.Key)
		if !s.dynamic {
			return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("collection %s has no field %q (and no dynamic field)", s.name, m.Key))
		}
		if !identifier.MatchString(key) {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("dynamic key %q: tobira writes keys of letters, digits and underscores only", key))
		}
		x, err := goValue(m.Value)
		if err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("dynamic key %q: %v", key, err))
		}
		dyn[key] = x
		hasDyn = true
	}
	if fill {
		// An insert names every field: one the values leave out is NULL
		// (Milvus then applies the field's default, or refuses a field
		// that has neither).
		for i := range s.fields {
			f := &s.fields[i]
			if seen[f.name] || f.output != "" || (f.pk && f.autoID) || (!f.nullable && !f.hasDefault) {
				continue
			}
			col, err := columnFor(f, driver.Null(), op)
			if err != nil {
				return nil, err
			}
			fields = append(fields, fieldData(col))
		}
	}
	if s.dynamic && (hasDyn || fill) {
		raw, err := json.Marshal(dyn)
		if err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, err)
		}
		fields = append(fields, &schemapb.FieldData{
			Type:      schemapb.DataType_JSON,
			FieldName: dynamicField,
			IsDynamic: true,
			Field:     &schemapb.FieldData_Scalars{Scalars: &schemapb.ScalarField{Data: &schemapb.ScalarField_JsonData{JsonData: &schemapb.JSONArray{Data: [][]byte{raw}}}}},
		})
	}
	return fields, nil
}

// fieldData is a column's wire form, nulls compacted as Milvus expects.
func fieldData(col column.Column) *schemapb.FieldData {
	col.CompactNullableValues()
	return col.FieldData()
}

// Insert adds one entity. Its key is the primary key member, or, for an
// auto-id key, the one Milvus assigns (a key given for it is refused). A
// key already in use is ErrConflict: Milvus itself would accept the insert
// and keep the newer entity, so the driver checks first. Fields left out
// are NULL, or their default.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return nil, err
	}
	s, err := c.getSchema(ctx, op, c.db(ref.Namespace), ref.Name)
	if err != nil {
		return nil, err
	}
	pk := s.pkField()
	kv, given := values.Get(pk.name)
	given = given && !kv.IsNull()
	members := values
	switch {
	case pk.autoID && given:
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("primary key %q is assigned by Milvus (auto id): leave it out", pk.name))
	case !pk.autoID && !given:
		return nil, driver.Wrap(driver.ErrConstraint, Name, op, fmt.Errorf("primary key %q is required", pk.name))
	}
	var key driver.Value
	if given {
		if key, err = pkValue(pk, kv, op); err != nil {
			return nil, err
		}
		members = withMember(values, pk.name, key)
	}
	fields, err := entityColumns(s, members, nil, op, true)
	if err != nil {
		return nil, err
	}
	if given {
		found, _, err := c.exists(ctx, s, op, key, false)
		if err != nil {
			return nil, err
		}
		if found {
			return nil, driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("an entity with %s %s already exists", pk.name, key))
		}
	}
	resp, err := c.svc.Insert(ctx, &milvuspb.InsertRequest{DbName: s.db, CollectionName: s.name, FieldsData: fields, NumRows: 1, SchemaTimestamp: s.updateTS})
	if err := checkRPC(resp, err); err != nil {
		return nil, mapError(ctx, op, err)
	}
	if !given {
		switch ids := resp.GetIDs(); {
		case len(ids.GetIntId().GetData()) == 1:
			key = driver.Int(ids.GetIntId().GetData()[0])
		case len(ids.GetStrId().GetData()) == 1:
			key = driver.String(ids.GetStrId().GetData()[0])
		default:
			return nil, driver.Wrap(nil, Name, op, errors.New("milvus returned no key for the inserted entity"))
		}
	}
	return driver.Object{{Key: pk.name, Value: key}}, nil
}

// withMember returns obj with member name set to v.
func withMember(obj driver.Object, name string, v driver.Value) driver.Object {
	out := make(driver.Object, len(obj))
	copy(out, obj)
	for i := range out {
		if out[i].Key == name {
			out[i].Value = v
		}
	}
	return out
}

// Update sets fields and dynamic keys of one entity with a partial upsert
// (Milvus 2.6). A NULL sets a nullable field to null and a dynamic key to
// JSON null. A missing entity is ErrNotFound; the key cannot change. An
// auto-id collection is refused: Milvus would give the entity a new key.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return err
	}
	s, err := c.getSchema(ctx, op, c.db(ref.Namespace), ref.Name)
	if err != nil {
		return err
	}
	k, err := keyValue(s, key, op)
	if err != nil {
		return err
	}
	pk := s.pkField()
	if pk.autoID {
		// Milvus gives an upserted entity of an auto-id collection a new
		// key, even a partial upsert (checked against 2.6.25), so an
		// update would silently change the entity's key.
		return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("collection %s assigns its primary key %q (auto id), and Milvus gives an updated entity a new key: edit it with a native client, or delete and insert", s.name, pk.name))
	}
	if _, ok := changes.Get(pk.name); ok {
		return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("primary key %q cannot change: insert a new entity instead", pk.name))
	}
	if len(changes) == 0 {
		return nil
	}
	dynChange := false
	for _, m := range changes {
		if _, static := s.field(m.Key); !static {
			dynChange = true
		}
	}
	// Validate before reading, so a bad value costs no round trip.
	if _, err := entityColumns(s, changes, nil, op, false); err != nil {
		return err
	}
	found, dyn, err := c.exists(ctx, s, op, k, dynChange && s.dynamic)
	if err != nil {
		return err
	}
	if !found {
		return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no entity with %s %s", pk.name, k))
	}
	// The dynamic field is written whole: the stored keys, with the
	// changes over them.
	fields, err := entityColumns(s, withMemberAdded(changes, pk.name, k), dyn, op, false)
	if err != nil {
		return err
	}
	resp, err := c.svc.Upsert(ctx, &milvuspb.UpsertRequest{DbName: s.db, CollectionName: s.name, FieldsData: fields, NumRows: 1, SchemaTimestamp: s.updateTS, PartialUpdate: true})
	return mapError(ctx, op, checkRPC(resp, err))
}

// withMemberAdded returns obj plus member name = v, first.
func withMemberAdded(obj driver.Object, name string, v driver.Value) driver.Object {
	return append(driver.Object{{Key: name, Value: v}}, obj...)
}

// Delete removes one entity. A missing entity is ErrNotFound.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return err
	}
	s, err := c.getSchema(ctx, op, c.db(ref.Namespace), ref.Name)
	if err != nil {
		return err
	}
	k, err := keyValue(s, key, op)
	if err != nil {
		return err
	}
	found, _, err := c.exists(ctx, s, op, k, false)
	if err != nil {
		return err
	}
	pk := s.pkField()
	if !found {
		return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("no entity with %s %s", pk.name, k))
	}
	tv, err := templateValue(k)
	if err != nil {
		return err
	}
	resp, err := c.svc.Delete(ctx, &milvuspb.DeleteRequest{
		DbName:             s.db,
		CollectionName:     s.name,
		Expr:               pk.name + " == {tobira_k}",
		ExprTemplateValues: exprParams{"tobira_k": tv},
		ConsistencyLevel:   commonpb.ConsistencyLevel_Strong,
	})
	return mapError(ctx, op, checkRPC(resp, err))
}
