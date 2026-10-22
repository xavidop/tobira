package cosmos

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

var errReadOnlyConn = errors.New("the connection is read-only")

// target resolves a mutation's container and its properties, refusing on
// a read-only connection before any request.
func (c *conn) target(ctx context.Context, op string, ref driver.CollectionRef) (*azcosmos.ContainerClient, *azcosmos.ContainerProperties, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, nil, err
	}
	if c.readOnly {
		return nil, nil, driver.Wrap(driver.ErrReadOnly, Name, op, errReadOnlyConn)
	}
	db, err := c.database(op, ref.Namespace)
	if err != nil {
		return nil, nil, err
	}
	cc, err := c.container(op, db, ref.Name)
	if err != nil {
		return nil, nil, err
	}
	props, err := c.containerProps(ctx, db, ref.Name, cc)
	if err != nil {
		return nil, nil, mapError(ctx, op, err)
	}
	return cc, props, nil
}

// Insert creates an item from values. Its id must be a string; without one
// a random UUID is used. The partition key comes from the item's own
// fields. An existing id in the same partition is ErrConstraint. The
// returned key holds the partition key fields and id.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	cc, props, err := c.target(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	doc := slices.Clone(values)
	if v, ok := doc.Get("id"); !ok || v.IsNull() {
		doc = slices.DeleteFunc(doc, func(m driver.Member) bool { return m.Key == "id" })
		doc = append(driver.Object{{Key: "id", Value: driver.String(newID())}}, doc...)
	} else if s, _ := v.AsText(); v.Type() != driver.TypeString || s == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("id must be a non-empty string: Cosmos DB ids are strings"))
	}
	if slices.ContainsFunc(doc, func(m driver.Member) bool { return isSystemProp(m.Key) }) {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("system properties (_rid, _etag, _ts…) are set by Cosmos DB, not written"))
	}
	var pkVals []driver.Value
	for _, p := range props.PartitionKeyDefinition.Paths {
		v, ok := pkValueAt(doc, p)
		if !ok {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("the item lacks partition key field %q", pkFieldName(p)))
		}
		pkVals = append(pkVals, v)
	}
	pk, err := partitionKeyOf(pkVals)
	if err != nil {
		return nil, err
	}
	body, err := encodeValue(driver.ObjectValue(doc...))
	if err != nil {
		return nil, err
	}
	if _, err := cc.CreateItem(ctx, pk, body, nil); err != nil {
		return nil, mapError(ctx, op, err)
	}
	var key driver.Object
	for _, k := range keyPaths(props) {
		v, _ := pkValueAt(doc, k.path)
		key = append(key, driver.Member{Key: k.name, Value: v})
	}
	return key, nil
}

// Update reads the item, applies changes (each a top-level property,
// replaced or added) and replaces it with an ETag match, so a change made
// in between is ErrConflict. The id, the partition key and the system
// properties cannot change. A missing item is ErrNotFound.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	cc, props, err := c.target(ctx, op, ref)
	if err != nil {
		return err
	}
	id, pk, err := itemKey(op, key, props)
	if err != nil {
		return err
	}
	fixed := keyFields(props)
	for _, m := range changes {
		if slices.Contains(fixed, m.Key) || slices.ContainsFunc(fixed, func(f string) bool { return strings.HasPrefix(f, m.Key+".") }) || isSystemProp(m.Key) {
			return driver.Wrap(driver.ErrUnsupported, Name, op,
				fmt.Errorf("%q cannot change: the id, the partition key and system properties are fixed (insert a copy and delete the original)", m.Key))
		}
	}
	resp, err := cc.ReadItem(ctx, pk, id, nil)
	if err != nil {
		return mapError(ctx, op, err)
	}
	v, err := decodeValue(resp.Value)
	if err != nil {
		return driver.Wrap(nil, Name, op, fmt.Errorf("cannot decode the item: %w", err))
	}
	doc, _ := v.AsObject()
	doc = slices.DeleteFunc(slices.Clone(doc), func(m driver.Member) bool { return isSystemProp(m.Key) })
	for _, ch := range changes {
		if i := slices.IndexFunc(doc, func(m driver.Member) bool { return m.Key == ch.Key }); i >= 0 {
			doc[i].Value = ch.Value
		} else {
			doc = append(doc, ch)
		}
	}
	body, err := encodeValue(driver.ObjectValue(doc...))
	if err != nil {
		return err
	}
	etag := resp.ETag
	if _, err := cc.ReplaceItem(ctx, pk, id, body, &azcosmos.ItemOptions{IfMatchEtag: &etag}); err != nil {
		err = mapError(ctx, op, err)
		if errors.Is(err, driver.ErrConflict) {
			return driver.Wrap(driver.ErrConflict, Name, op, errors.New("the item changed since it was read: reload and try again"))
		}
		return err
	}
	return nil
}

// Delete removes the item with key's id and partition key. A missing item
// is ErrNotFound.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	cc, props, err := c.target(ctx, op, ref)
	if err != nil {
		return err
	}
	id, pk, err := itemKey(op, key, props)
	if err != nil {
		return err
	}
	_, err = cc.DeleteItem(ctx, pk, id, nil)
	return mapError(ctx, op, err)
}

// itemKey reads an item's id and partition key from a key object holding
// exactly the container's key fields (keyFields).
func itemKey(op string, key driver.Object, props *azcosmos.ContainerProperties) (string, azcosmos.PartitionKey, error) {
	fields := keyFields(props)
	for _, m := range key {
		if !slices.Contains(fields, m.Key) {
			return "", azcosmos.PartitionKey{}, driver.Wrap(driver.ErrSyntax, Name, op,
				fmt.Errorf("key field %q is not part of the item key (%s)", m.Key, strings.Join(fields, ", ")))
		}
	}
	idv, ok := key.Get("id")
	id, _ := idv.AsText()
	if !ok || idv.Type() != driver.TypeString || id == "" {
		return "", azcosmos.PartitionKey{}, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("the key needs id, a non-empty string"))
	}
	var vals []driver.Value
	for _, p := range props.PartitionKeyDefinition.Paths {
		v, ok := key.Get(pkFieldName(p))
		if !ok {
			return "", azcosmos.PartitionKey{}, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("the key needs partition key field %q", pkFieldName(p)))
		}
		vals = append(vals, v)
	}
	pk, err := partitionKeyOf(vals)
	return id, pk, err
}

// newID returns a random UUID (version 4) for an item inserted without id.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
