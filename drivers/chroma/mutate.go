package chroma

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/xavidop/tobira/driver"
)

// recordWrite is a Mutator row split into a record's parts.
type recordWrite struct {
	id        *string
	idValue   driver.Value
	embedding []float32
	document  *string
	uri       *string
	metadata  map[string]any // nil = no metadata change
}

// splitValues splits a row: id, embedding, document and uri are the
// record's own parts; every other member is a metadata key (a renamed
// column such as meta_id writes the key id). On insert a NULL member is
// left out; on update it removes the key. A NULL document, uri or
// embedding cannot be written: Chroma keeps the old one.
func splitValues(values driver.Object, update bool) (recordWrite, error) {
	var w recordWrite
	for _, m := range values {
		switch m.Key {
		case idColumn:
			if m.Value.IsNull() && !update {
				continue // a generated id
			}
			id, err := idText(m.Value)
			if err != nil {
				return w, err
			}
			w.id, w.idValue = &id, idValue(id)
		case embeddingColumn:
			if m.Value.IsNull() {
				if update {
					return w, unsupported("update", "chroma cannot remove an embedding: every record has one")
				}
				continue
			}
			e, err := embeddingOf(m.Value)
			if err != nil {
				return w, err
			}
			w.embedding = e
		case documentColumn, uriColumn:
			dst := &w.document
			if m.Key == uriColumn {
				dst = &w.uri
			}
			if m.Value.IsNull() {
				if update {
					return w, unsupported("update", "chroma cannot clear a record's %s (an update keeps the old one)", m.Key)
				}
				continue
			}
			s, ok := m.Value.AsText()
			if !ok || m.Value.Type() != driver.TypeString {
				return w, driver.Wrap(driver.ErrSyntax, Name, "encode", fmt.Errorf("%s must be a string, got %s", m.Key, m.Value.Type()))
			}
			*dst = &s
		case distanceColumn, queryColumn:
			return w, unsupported("encode", "%s is computed by a search and cannot be written", m.Key)
		default:
			x, err := encodeMeta(m.Value)
			if err != nil {
				return w, err
			}
			if x == nil && !update {
				continue
			}
			if w.metadata == nil {
				w.metadata = map[string]any{}
			}
			w.metadata[metaKey(m.Key)] = x
		}
	}
	return w, nil
}

// keyID reads a Mutator key: Chroma names records by id alone.
func keyID(key driver.Object) (string, error) {
	if len(key) != 1 || key[0].Key != idColumn {
		return "", unsupported("key", "chroma records are keyed by %q alone", idColumn)
	}
	return idText(key[0].Value)
}

func (c *conn) refuseReadOnly(op string) error {
	if c.readOnly {
		return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("the connection is read-only"))
	}
	return nil
}

// exists reports whether the collection holds a record with this id.
func (c *conn) exists(ctx context.Context, op string, info *collInfo, id string) (bool, error) {
	resp, err := c.api.get(ctx, c.tenant, info.ref.Namespace, info.id, getRequest{IDs: []string{id}, Include: []string{}})
	if err != nil {
		return false, c.fail(ctx, op, err)
	}
	for _, got := range resp.IDs {
		if got == id {
			return true, nil
		}
	}
	return false, nil
}

// Insert adds one record. It needs an embedding (Chroma's REST API never
// embeds, and tobira does not embed on a write). Without an id it
// generates a UUIDv4. Chroma's add silently keeps an existing record, so
// Insert checks the id first and returns ErrConflict. The check and the
// add are two requests: a record added by someone else in between is kept
// and this insert is silently lost.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	if err := c.check(ctx, "insert"); err != nil {
		return nil, err
	}
	if err := c.refuseReadOnly("insert"); err != nil {
		return nil, err
	}
	w, err := splitValues(values, false)
	if err != nil {
		return nil, err
	}
	if w.embedding == nil {
		return nil, driver.Wrap(driver.ErrSyntax, Name, "insert", errors.New("a chroma record needs an embedding: set the embedding column"))
	}
	if w.id == nil {
		id := newUUID()
		w.id, w.idValue = &id, driver.String(id)
	}
	info, err := c.collection(ctx, "insert", ref)
	if err != nil {
		return nil, err
	}
	if err := checkDim(info, w.embedding, "insert"); err != nil {
		return nil, err
	}
	found, err := c.exists(ctx, "insert", info, *w.id)
	if err != nil {
		return nil, err
	}
	if found {
		return nil, driver.Wrap(driver.ErrConflict, Name, "insert", fmt.Errorf("a record with id %q already exists", *w.id))
	}
	req := addRequest{IDs: []string{*w.id}, Embeddings: [][]float32{w.embedding}}
	if w.document != nil {
		req.Documents = []*string{w.document}
	}
	if w.uri != nil {
		req.URIs = []*string{w.uri}
	}
	if len(w.metadata) > 0 {
		req.Metadatas = []map[string]any{w.metadata}
	}
	if err := c.api.add(ctx, c.tenant, info.ref.Namespace, info.id, req); err != nil {
		return nil, c.fail(ctx, "insert", err)
	}
	return driver.Object{{Key: idColumn, Value: w.idValue}}, nil
}

// Update changes one record: its embedding, document, uri and metadata
// keys (merged; NULL removes a key). The id cannot change. Chroma's update
// of a missing id is a silent no-op, so Update checks the id first and
// returns ErrNotFound; a record deleted between the check and the update
// is not reported.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	if err := c.check(ctx, "update"); err != nil {
		return err
	}
	if err := c.refuseReadOnly("update"); err != nil {
		return err
	}
	id, err := keyID(key)
	if err != nil {
		return err
	}
	w, err := splitValues(changes, true)
	if err != nil {
		return err
	}
	if w.id != nil && *w.id != id {
		return unsupported("update", "chroma cannot change a record's id: insert a new record and delete this one")
	}
	if w.embedding == nil && w.document == nil && w.uri == nil && w.metadata == nil {
		if w.id != nil {
			return nil // only the unchanged id
		}
		return driver.Wrap(driver.ErrSyntax, Name, "update", errors.New("nothing to update"))
	}
	info, err := c.collection(ctx, "update", ref)
	if err != nil {
		return err
	}
	if w.embedding != nil {
		if err := checkDim(info, w.embedding, "update"); err != nil {
			return err
		}
	}
	found, err := c.exists(ctx, "update", info, id)
	if err != nil {
		return err
	}
	if !found {
		return driver.Wrap(driver.ErrNotFound, Name, "update", fmt.Errorf("no record with id %q", id))
	}
	req := updateRequest{IDs: []string{id}}
	if w.embedding != nil {
		req.Embeddings = [][]float32{w.embedding}
	}
	if w.document != nil {
		req.Documents = []*string{w.document}
	}
	if w.uri != nil {
		req.URIs = []*string{w.uri}
	}
	if w.metadata != nil {
		req.Metadatas = []map[string]any{w.metadata}
	}
	return c.fail(ctx, "update", c.api.update(ctx, c.tenant, info.ref.Namespace, info.id, req))
}

// Delete removes one record. Chroma reports a deleted count even for a
// missing id, so Delete checks the id first and returns ErrNotFound.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	if err := c.check(ctx, "delete"); err != nil {
		return err
	}
	if err := c.refuseReadOnly("delete"); err != nil {
		return err
	}
	id, err := keyID(key)
	if err != nil {
		return err
	}
	info, err := c.collection(ctx, "delete", ref)
	if err != nil {
		return err
	}
	found, err := c.exists(ctx, "delete", info, id)
	if err != nil {
		return err
	}
	if !found {
		return driver.Wrap(driver.ErrNotFound, Name, "delete", fmt.Errorf("no record with id %q", id))
	}
	return c.fail(ctx, "delete", c.api.delete(ctx, c.tenant, info.ref.Namespace, info.id, []string{id}))
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
