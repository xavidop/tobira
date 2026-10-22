package weaviate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/weaviate/weaviate/entities/models"

	"github.com/xavidop/tobira/driver"
)

// The Mutator edits one object, named by its id: the "id" member of a key.
// Members named like a vector column set that vector; every other member
// is a property, which must exist (the driver never lets Weaviate's
// auto-schema add one).

// refuseReadOnly fails a write on a read-only connection before any
// request.
func (c *conn) refuseReadOnly(ctx context.Context, op string) error {
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if c.readOnly {
		return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the connection is read-only"))
	}
	return nil
}

// target checks the collection reference before any request.
func target(op string, ref driver.CollectionRef) error {
	if err := checkNamespace(op, ref.Namespace); err != nil {
		return err
	}
	return checkName(op, ref.Name)
}

// Insert adds one object. Its id is the "id" member (an integer or a
// UUID), or one Weaviate assigns when there is none. An id in use is
// ErrConflict: Weaviate refuses to create an object over an existing one.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return nil, err
	}
	if err := target(op, ref); err != nil {
		return nil, err
	}
	var uuid string
	if v, ok := values.Get(idColumn); ok && !v.IsNull() {
		var err error
		if uuid, err = toUUID(v, op); err != nil {
			return nil, err
		}
	}
	s, err := c.getSchema(ctx, op, ref.Name)
	if err != nil {
		return nil, err
	}
	props, legacy, named, err := splitMembers(s, values, op)
	if err != nil {
		return nil, err
	}
	cr := c.data.Creator().WithClassName(s.class).WithProperties(props)
	if uuid != "" {
		cr = cr.WithID(uuid)
	}
	if legacy != nil {
		cr = cr.WithVector(legacy)
	}
	if len(named) > 0 {
		cr = cr.WithVectors(named)
	}
	res, err := cr.Do(ctx)
	if err != nil {
		err = mapError(ctx, op, err)
		if errors.Is(err, driver.ErrConflict) && uuid != "" {
			return nil, driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("object %s already exists", idValue(uuid)))
		}
		return nil, err
	}
	id := uuid
	if res != nil && res.Object != nil && res.Object.ID != "" {
		id = res.Object.ID.String()
	}
	return driver.Object{{Key: idColumn, Value: idValue(id)}}, nil
}

// Update sets properties and vectors of one object (a merge: members not
// named keep their values). A NULL clears a property. A missing object is
// ErrNotFound.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return err
	}
	if err := target(op, ref); err != nil {
		return err
	}
	uuid, err := keyID(key, op)
	if err != nil {
		return err
	}
	if _, ok := changes.Get(idColumn); ok {
		return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("an object id cannot change: insert a new object instead"))
	}
	if len(changes) == 0 {
		return nil
	}
	s, err := c.getSchema(ctx, op, ref.Name)
	if err != nil {
		return err
	}
	props, legacy, named, err := splitMembers(s, changes, op)
	if err != nil {
		return err
	}
	up := c.data.Updater().WithMerge().WithClassName(s.class).WithID(uuid).WithProperties(props)
	if legacy != nil {
		up = up.WithVector(legacy)
	}
	if len(named) > 0 {
		up = up.WithVectors(named)
	}
	if err := up.Do(ctx); err != nil {
		err = mapError(ctx, op, err)
		if errors.Is(err, driver.ErrNotFound) {
			return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("object %s does not exist", idValue(uuid)))
		}
		return err
	}
	return nil
}

// Delete removes one object. A missing object is ErrNotFound.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return err
	}
	if err := target(op, ref); err != nil {
		return err
	}
	uuid, err := keyID(key, op)
	if err != nil {
		return err
	}
	s, err := c.getSchema(ctx, op, ref.Name)
	if err != nil {
		return err
	}
	// Weaviate deletes a missing object without complaint: check first.
	// An object deleted by another client in between is gone either way.
	if err := c.mustExist(ctx, op, s, uuid); err != nil {
		return err
	}
	if err := c.data.Deleter().WithClassName(s.class).WithID(uuid).Do(ctx); err != nil {
		err = mapError(ctx, op, err)
		if errors.Is(err, driver.ErrNotFound) {
			return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("object %s does not exist", idValue(uuid)))
		}
		return err
	}
	return nil
}

// mustExist fails with ErrNotFound when the object does not exist.
func (c *conn) mustExist(ctx context.Context, op string, s *collSchema, uuid string) error {
	resp, err := c.con.RunREST(ctx, "/objects/"+s.class+"/"+uuid, http.MethodHead, nil)
	if err != nil {
		return mapError(ctx, op, err)
	}
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("object %s does not exist", idValue(uuid)))
	}
	return statusError(op, resp.StatusCode, resp.Body)
}

// keyID reads a key that names exactly one object: {"id": value}.
func keyID(key driver.Object, op string) (string, error) {
	if len(key) != 1 || key[0].Key != idColumn {
		return "", driver.Wrap(driver.ErrUnsupported, Name, op, errors.New(`an object is named by its "id" alone`))
	}
	return toUUID(key[0].Value, op)
}

// splitMembers sorts members into properties, the legacy vector and named
// vectors. The id member is skipped.
func splitMembers(s *collSchema, members driver.Object, op string) (map[string]any, []float32, models.Vectors, error) {
	props := map[string]any{}
	var legacy []float32
	named := models.Vectors{}
	for _, m := range members {
		if m.Key == idColumn {
			continue
		}
		if v, ok := s.vectorByColumn(m.Key); ok {
			vec, err := toVector(v, m.Value, op)
			if err != nil {
				return nil, nil, nil, err
			}
			switch {
			case vec == nil:
			case v.name == "":
				legacy = vec
			default:
				named[v.name] = vec
			}
			continue
		}
		p, ok := s.prop(m.Key)
		if !ok {
			return nil, nil, nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("collection %s has no property %q (tobira does not add properties)", s.class, m.Key))
		}
		x, err := encodeProp(p, m.Value, op)
		if err != nil {
			return nil, nil, nil, err
		}
		props[p.name] = x
	}
	return props, legacy, named, nil
}

// toVector converts a value for vector v: a Vector, or a List of numbers.
// NULL leaves the vector unchanged.
func toVector(v vectorInfo, val driver.Value, op string) ([]float32, error) {
	if val.IsNull() {
		return nil, nil
	}
	if v.multi {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("vector %q is a multi-vector: edit it with a native client", v.column))
	}
	data, ok := val.AsVector()
	if !ok {
		items, isList := val.AsList()
		if !isList {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("vector %q needs a list of numbers, got %s", v.column, val.Type()))
		}
		data = make([]float32, len(items))
		for i, it := range items {
			f, ok := it.AsFloat()
			if !ok {
				return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("vector %q needs a list of numbers", v.column))
			}
			data[i] = float32(f)
		}
	}
	if len(data) == 0 {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("vector %q is empty", v.column))
	}
	for _, f := range data {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("vector %q has a non-finite component", v.column))
		}
	}
	return data, nil
}
