package weaviate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/weaviate/weaviate/entities/models"

	"github.com/xavidop/tobira/driver"
)

const (
	// maxCollections caps a collection listing.
	maxCollections = 10000
	// describeSample and describeTimeout bound Describe's sampling.
	describeSample  = 20
	describeTimeout = 5 * time.Second
)

// Namespaces returns the one namespace, "default".
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	return []driver.Namespace{{Name: defaultNamespace, Label: "instance", Default: true}}, nil
}

// Collections lists the collections, sorted by name, at most
// maxCollections of them.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	if err := checkNamespace("collections", ns.Name); err != nil {
		return nil, err
	}
	dump, err := c.schema.Getter().Do(ctx)
	if err != nil {
		return nil, mapError(ctx, "collections", err)
	}
	var out []driver.Collection
	for _, cl := range dump.Classes {
		if cl == nil {
			continue
		}
		out = append(out, driver.Collection{Ref: driver.CollectionRef{Namespace: defaultNamespace, Name: cl.Class}, Type: driver.CollectionCollection, RowEstimate: -1, Comment: cl.Description})
	}
	slices.SortFunc(out, func(a, b driver.Collection) int { return strings.Compare(a.Ref.Name, b.Ref.Name) })
	return out[:min(len(out), maxCollections)], nil
}

// Describe returns the properties, typed by the schema, and the vectors,
// sized from a sample of up to 20 objects read within 5 seconds. The id's
// type comes from the sample too (Int when every id is an integer UUID).
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := checkNamespace(op, ref.Namespace); err != nil {
		return nil, err
	}
	s, err := c.getSchema(ctx, op, ref.Name)
	if err != nil {
		return nil, err
	}
	spec := columnSpec{props: s.readable(), vectors: true}
	sctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	objs, err := c.getObjects(sctx, op, s, gqlArgs(arg("limit", fmt.Sprint(describeSample))), s.selectionOf(spec.props, true, nil))
	if err != nil {
		// Only our own 5 s sampling deadline is forgiven.
		if ctx.Err() != nil || !errors.Is(sctx.Err(), context.DeadlineExceeded) {
			return nil, err
		}
		objs = nil
	}
	sample := make([]driver.Row, len(objs))
	for i, o := range objs {
		sample[i], _ = s.row(spec, o)
	}
	out := &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: defaultNamespace, Name: s.class},
		Type:        driver.CollectionCollection,
		PrimaryKey:  []string{idColumn},
		RowEstimate: c.count(ctx, s),
		SizeBytes:   -1,
		Sample:      sample,
		Comment:     describeComment(s),
	}
	idT := idType(sample)
	out.Fields = append(out.Fields, driver.Field{Name: idColumn, NativeType: "uuid", Type: idT, Comment: "the object id"})
	for _, p := range s.props {
		f := driver.Field{Name: p.name, NativeType: nativeType(p.dataType), Type: typeOf(p.dataType), Nullable: true, Comment: p.desc, Fields: nestedFields(p.dataType, p.nested)}
		out.Fields = append(out.Fields, f)
		if p.searchable {
			out.Indexes = append(out.Indexes, driver.Index{Name: p.name + "_searchable", Fields: []string{p.name}, Method: "bm25"})
		}
		if p.rangeable {
			out.Indexes = append(out.Indexes, driver.Index{Name: p.name + "_range", Fields: []string{p.name}, Method: "range"})
		}
	}
	base := 1 + len(spec.props)
	for i, v := range s.vectors {
		native := "vector"
		for _, r := range sample {
			if vec, ok := r[base+i].AsVector(); ok {
				native = fmt.Sprintf("vector(%d)", len(vec))
				break
			}
		}
		if v.multi {
			native = "multivector"
		}
		comment := fmt.Sprintf("distance %s, vectorizer %s", v.distance, orUnknown(v.vectorizer))
		if v.name == "" {
			comment = "the default vector; " + comment
		}
		out.Fields = append(out.Fields, driver.Field{Name: v.column, NativeType: native, Type: driver.TypeVector, Nullable: true, Comment: comment})
		out.Indexes = append(out.Indexes, driver.Index{Name: v.column, Fields: []string{v.column}, Method: v.indexType + " " + v.distance})
	}
	if b, err := json.MarshalIndent(s.raw, "", "  "); err == nil {
		out.DDL = string(b)
	}
	return out, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func describeComment(s *collSchema) string {
	var parts []string
	if s.raw.Description != "" {
		parts = append(parts, s.raw.Description)
	}
	if mt := s.raw.MultiTenancyConfig; mt != nil && mt.Enabled {
		parts = append(parts, "multi-tenant (tobira reads no tenant: queries fail)")
	}
	if s.raw.Vectorizer != "" {
		parts = append(parts, "vectorizer "+s.raw.Vectorizer)
	}
	return strings.Join(parts, "; ")
}

// count reads the number of objects with an Aggregate; -1 when it fails.
func (c *conn) count(ctx context.Context, s *collSchema) int64 {
	data, err := c.runGraphQL(ctx, "describe", "{ Aggregate { "+s.class+" { meta { count } } } }")
	if err != nil {
		return -1
	}
	groups := resultObjects(data, "Aggregate", s.class)
	if len(groups) != 1 {
		return -1
	}
	meta, _ := groups[0]["meta"].(map[string]any)
	if n, ok := meta["count"].(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return -1
}

// nestedFields describes the members of object, geoCoordinates and
// phoneNumber properties.
func nestedFields(dt []string, nested []*models.NestedProperty) []driver.Field {
	t, _ := baseType(dt)
	f := func(name string, typ driver.Type, native string) driver.Field {
		return driver.Field{Name: name, Type: typ, NativeType: native, Nullable: true}
	}
	switch t {
	case "geoCoordinates":
		return []driver.Field{f("latitude", driver.TypeFloat, "number"), f("longitude", driver.TypeFloat, "number")}
	case "phoneNumber":
		return []driver.Field{
			f("input", driver.TypeString, "text"), f("internationalFormatted", driver.TypeString, "text"),
			f("countryCode", driver.TypeInt, "int"), f("national", driver.TypeInt, "int"),
			f("nationalFormatted", driver.TypeString, "text"), f("valid", driver.TypeBool, "boolean"),
		}
	case "object":
		out := make([]driver.Field, 0, len(nested))
		for _, np := range nested {
			nf := f(np.Name, typeOf(np.DataType), nativeType(np.DataType))
			nf.Comment = np.Description
			nf.Fields = nestedFields(np.DataType, np.NestedProperties)
			out = append(out, nf)
		}
		return out
	}
	return nil
}
