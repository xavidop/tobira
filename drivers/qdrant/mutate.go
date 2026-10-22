package qdrant

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
)

// The Mutator edits one point, named by its id: the "id" member of a key.
// Members named like a vector column set that vector; every other member
// is a payload key. Writes wait for Qdrant to apply them.

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

// insertMarkerKey is the payload key Insert writes with a random value (a
// nonce), so it can tell its write from another writer's; it is removed
// once the point is read back.
const insertMarkerKey = "_tobira_insert"

// Insert adds one point. Its id is the "id" member, or a new random UUID
// when there is none. An id already in use is ErrConflict. The write is an
// upsert in insert-only mode, so even a point created between the check
// and the write is never replaced. Qdrant skips such a point without an
// error, so every insert also writes a one-off marker (insertMarkerKey, a
// random nonce) and reads only the marker back: if it is not ours, the
// insert lost the race and is ErrConflict too. Payload values cannot tell
// two writers apart (the other point may hold the same ones, and another
// client may set payload on ours meanwhile), and neither can vectors, which
// Qdrant normalises and may quantise. The marker is removed on every path
// after the write (removeMarker); it stays only when that removal fails
// too, for example when the server is unreachable.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return nil, err
	}
	if err := checkNamespace(op, ref.Namespace); err != nil {
		return nil, err
	}
	schema, err := c.getSchema(ctx, op, ref.Name)
	if err != nil {
		return nil, err
	}
	var id *pb.PointId
	if v, ok := values.Get(idColumn); ok && !v.IsNull() {
		if id, err = pointID(v); err != nil {
			return nil, err
		}
		cur, err := c.current(ctx, op, ref.Name, id, nil)
		if err != nil {
			return nil, err
		}
		if cur != nil {
			return nil, errTaken(op, id)
		}
	} else {
		id = pb.NewIDUUID(newUUID())
	}
	payload, vectors, err := splitMembers(schema, values, nil, op)
	if err != nil {
		return nil, err
	}
	marker := newUUID()
	payload[insertMarkerKey] = pb.NewValueString(marker)
	pt := &pb.PointStruct{Id: id, Payload: payload, Vectors: pointVectors(vectors)}
	if c.beforeInsertWrite != nil {
		c.beforeInsertWrite()
	}
	wait := true
	mode := pb.UpdateMode_InsertOnly
	if _, err := c.client.GetPointsClient().Upsert(ctx, &pb.UpsertPoints{CollectionName: ref.Name, Wait: &wait, Points: []*pb.PointStruct{pt}, UpdateMode: &mode}); err != nil {
		if errors.Is(mapError(ctx, op, err), driver.ErrConflict) {
			return nil, errTaken(op, id)
		}
		// The write may still have landed (a timeout, a lost reply).
		_ = c.removeMarker(ctx, ref.Name, id, marker, false)
		return nil, mapError(ctx, op, err)
	}
	if c.afterInsertWrite != nil {
		c.afterInsertWrite()
	}
	stored, err := c.point(ctx, op, ref.Name, id, pb.NewWithPayloadInclude(insertMarkerKey))
	if err != nil {
		_ = c.removeMarker(ctx, ref.Name, id, marker, false)
		return nil, err
	}
	if stored == nil || stored[insertMarkerKey].GetStringValue() != marker {
		return nil, errTaken(op, id) // our write was skipped: nothing of ours to remove
	}
	if cleanup := c.removeMarker(ctx, ref.Name, id, marker, true); cleanup != nil {
		return nil, mapError(ctx, op, fmt.Errorf("point %s was inserted, but removing its %s marker failed: %w", idValue(id), insertMarkerKey, cleanup))
	}
	return driver.Object{{Key: idColumn, Value: idValue(id)}}, nil
}

// markerCleanupTimeout bounds removeMarker, which runs even after the
// caller's ctx has ended.
const markerCleanupTimeout = 5 * time.Second

// removeMarker deletes Insert's marker from point id. confirmed says the
// read-back saw this insert's marker, so the point is ours and the id
// alone selects it. Otherwise (the write or the read-back failed) the
// selector also matches the marker's value, so a point another insert won,
// holding its own marker, is left alone; a collection whose strict mode
// refuses filters on unindexed keys refuses that, and the marker stays. It
// runs with the caller's values but not its cancellation, and a short
// timeout of its own.
func (c *conn) removeMarker(ctx context.Context, coll string, id *pb.PointId, marker string, confirmed bool) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markerCleanupTimeout)
	defer cancel()
	sel := pb.NewPointsSelector(id)
	if !confirmed {
		sel = pb.NewPointsSelectorFilter(&pb.Filter{Must: []*pb.Condition{
			pb.NewHasID(id), pb.NewMatchKeyword(insertMarkerKey, marker),
		}})
	}
	wait := true
	_, err := c.client.GetPointsClient().DeletePayload(cctx, &pb.DeletePayloadPoints{
		CollectionName: coll, Wait: &wait, Keys: []string{insertMarkerKey}, PointsSelector: sel,
	})
	return err
}

func errTaken(op string, id *pb.PointId) error {
	return driver.Wrap(driver.ErrConflict, Name, op, fmt.Errorf("point %s already exists", idValue(id)))
}

// Update sets payload keys and vectors of one point, in one UpdateBatch
// request (update_vectors, then set_payload). A NULL sets the key to
// null. A Decimal over a stored JSON number stays a number (encodeLike). A
// missing point is ErrNotFound.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return err
	}
	if err := checkNamespace(op, ref.Namespace); err != nil {
		return err
	}
	id, err := keyID(key, op)
	if err != nil {
		return err
	}
	if _, ok := changes.Get(idColumn); ok {
		return driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("a point id cannot change: insert a new point instead"))
	}
	if len(changes) == 0 {
		return nil
	}
	schema, err := c.getSchema(ctx, op, ref.Name)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(changes))
	for _, m := range changes {
		if _, isVec := schema.vectorByColumn(m.Key); !isVec {
			keys = append(keys, schema.payloadKey(m.Key))
		}
	}
	cur, err := c.current(ctx, op, ref.Name, id, keys)
	if err != nil {
		return err
	}
	if cur == nil {
		return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("point %s does not exist", idValue(id)))
	}
	payload, vectors, err := splitMembers(schema, changes, cur, op)
	if err != nil {
		return err
	}
	// Qdrant applies a batch's operations in order and stops at the first
	// failure without undoing the earlier ones. Vectors, the operation the
	// server can still refuse (their sizes are checked here first), go
	// first, so a refused vector leaves the payload untouched.
	var ops []*pb.PointsUpdateOperation
	if len(vectors) > 0 {
		pv := &pb.PointVectors{Id: id, Vectors: pointVectors(vectors)}
		ops = append(ops, pb.NewPointsUpdateUpdateVectors(&pb.PointsUpdateOperation_UpdateVectors{Points: []*pb.PointVectors{pv}}))
	}
	if len(payload) > 0 {
		ops = append(ops, pb.NewPointsUpdateSetPayload(&pb.PointsUpdateOperation_SetPayload{Payload: payload, PointsSelector: pb.NewPointsSelector(id)}))
	}
	wait := true
	if _, err := c.client.GetPointsClient().UpdateBatch(ctx, &pb.UpdateBatchPoints{CollectionName: ref.Name, Wait: &wait, Operations: ops}); err != nil {
		return mapError(ctx, op, err)
	}
	return nil
}

// Delete removes one point. A missing point is ErrNotFound.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return err
	}
	if err := checkNamespace(op, ref.Namespace); err != nil {
		return err
	}
	id, err := keyID(key, op)
	if err != nil {
		return err
	}
	if err := c.mustExist(ctx, op, ref.Name, id); err != nil {
		return err
	}
	wait := true
	_, err = c.client.GetPointsClient().Delete(ctx, &pb.DeletePoints{CollectionName: ref.Name, Wait: &wait, Points: pb.NewPointsSelector(id)})
	return mapError(ctx, op, err)
}

// keyID reads a key that names exactly one point: {"id": value}.
func keyID(key driver.Object, op string) (*pb.PointId, error) {
	if len(key) != 1 || key[0].Key != idColumn {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New(`a point is named by its "id" alone`))
	}
	return pointID(key[0].Value)
}

// current returns the stored payload of point id (only keys, when given;
// none when keys is empty but non-nil), or nil when the point does not
// exist.
func (c *conn) current(ctx context.Context, op, coll string, id *pb.PointId, keys []string) (map[string]*pb.Value, error) {
	with := pb.NewWithPayload(false)
	if len(keys) > 0 {
		with = pb.NewWithPayloadInclude(keys...)
	}
	return c.point(ctx, op, coll, id, with)
}

// point returns the payload of point id selected by with, or nil when the
// point does not exist.
func (c *conn) point(ctx context.Context, op, coll string, id *pb.PointId, with *pb.WithPayloadSelector) (map[string]*pb.Value, error) {
	resp, err := c.client.GetPointsClient().Get(ctx, &pb.GetPoints{
		CollectionName: coll,
		Ids:            []*pb.PointId{id},
		WithPayload:    with,
		WithVectors:    pb.NewWithVectors(false),
	})
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	if len(resp.GetResult()) == 0 {
		return nil, nil
	}
	if p := resp.GetResult()[0].GetPayload(); p != nil {
		return p, nil
	}
	return map[string]*pb.Value{}, nil
}

func (c *conn) mustExist(ctx context.Context, op, coll string, id *pb.PointId) error {
	cur, err := c.current(ctx, op, coll, id, nil)
	if err != nil {
		return err
	}
	if cur == nil {
		return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("point %s does not exist", idValue(id)))
	}
	return nil
}

// splitMembers sorts members into payload values and vectors. The id
// member is skipped (it is the point id). cur is the stored payload, for
// encodeLike; nil on insert.
func splitMembers(s *collSchema, members driver.Object, cur map[string]*pb.Value, op string) (map[string]*pb.Value, map[string]*pb.Vector, error) {
	payload := map[string]*pb.Value{}
	vectors := map[string]*pb.Vector{}
	for _, m := range members {
		if m.Key == idColumn {
			continue
		}
		if v, ok := s.vectorByColumn(m.Key); ok {
			vec, err := toVector(v, m.Value, op)
			if err != nil {
				return nil, nil, err
			}
			if vec != nil {
				vectors[v.name] = vec
			}
			continue
		}
		k := s.payloadKey(m.Key)
		pv, err := encodeLike(m.Value, cur[k])
		if err != nil {
			return nil, nil, err
		}
		payload[k] = pv
	}
	return payload, vectors, nil
}

// toVector converts a value for vector v: a Vector, or a List of numbers.
// NULL leaves the vector out.
func toVector(v vectorInfo, val driver.Value, op string) (*pb.Vector, error) {
	if val.IsNull() {
		return nil, nil
	}
	if v.sparse || v.multi {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("vector %q is not a dense vector: edit it with a native client", v.column))
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
	if uint64(len(data)) != v.size {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("vector %q has %d dimensions, got %d", v.column, v.size, len(data)))
	}
	return pb.NewVectorDense(data), nil
}

// pointVectors builds the vectors of a point: the unnamed vector alone, or
// a map of named ones (an empty map for a collection without vectors).
func pointVectors(vectors map[string]*pb.Vector) *pb.Vectors {
	if v, ok := vectors[""]; ok && len(vectors) == 1 {
		return &pb.Vectors{VectorsOptions: &pb.Vectors_Vector{Vector: v}}
	}
	return pb.NewVectorsMap(vectors)
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
