package neo4j

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// systemDB is Neo4j's own database, which holds users, roles and the
// catalog; it is never listed as a namespace.
const systemDB = "system"

// maxCountedLabels bounds the per-label counts Collections reads: each is
// one count-store lookup, cheap, but a graph may have thousands of labels.
const maxCountedLabels = 200

// Namespaces lists the databases from SHOW DATABASES, without the system
// one. The DSN's database is the default, or else the server's. A user
// who may not list databases gets the one the connection is on.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	const op = "namespaces"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	rows, err := c.all(ctx, op, systemDB, "SHOW DATABASES YIELD name, type, default RETURN DISTINCT name, type, default ORDER BY name", nil)
	if err != nil {
		if !errors.Is(err, driver.ErrPermission) {
			return nil, err
		}
		vals, err := c.one(ctx, op, "", "CALL db.info() YIELD name RETURN name", nil)
		if err != nil {
			return nil, err
		}
		name, _ := vals[0].(string)
		return []driver.Namespace{{Name: name, Label: "database", Default: true}}, nil
	}
	var out []driver.Namespace
	for _, r := range rows {
		name, _ := r[0].(string)
		typ, _ := r[1].(string)
		def, _ := r[2].(bool)
		if typ == systemDB || name == systemDB {
			continue
		}
		if c.set.database != "" {
			def = name == c.set.database
		}
		out = append(out, driver.Namespace{Name: name, Label: "database", Default: def})
	}
	return out, nil
}

// Collections lists a database's node labels (CALL db.labels), each with
// its node count from the count store.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	labels, err := c.labels(ctx, op, ns.Name)
	if err != nil {
		return nil, err
	}
	out := make([]driver.Collection, 0, len(labels))
	for _, l := range labels {
		col := driver.Collection{Ref: driver.CollectionRef{Namespace: ns.Name, Name: l}, Type: driver.CollectionCollection, RowEstimate: -1, Comment: "node label"}
		if len(labels) <= maxCountedLabels {
			if n, err := c.countLabel(ctx, op, ns.Name, l); err == nil {
				col.RowEstimate = n
			}
		}
		out = append(out, col)
	}
	return out, nil
}

// labels lists the node labels of database db, sorted.
func (c *conn) labels(ctx context.Context, op, db string) ([]string, error) {
	rows, err := c.all(ctx, op, db, "CALL db.labels() YIELD label RETURN label", nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if l, ok := r[0].(string); ok {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out, nil
}

// countLabel counts the nodes with a label: one count-store lookup.
func (c *conn) countLabel(ctx context.Context, op, db, label string) (int64, error) {
	vals, err := c.one(ctx, op, db, "MATCH (n:"+quote(label)+") RETURN count(n)", nil)
	if err != nil {
		return -1, err
	}
	n, _ := vals[0].(int64)
	return n, nil
}

// Describe reports a label's properties, as db.schema.nodeTypeProperties
// sees them across every node that carries the label: each property with
// the Cypher types it holds and whether every such node has it. The
// element id is the key, under the _id column that Browse returns.
// Answers are cached for a short while, since Browse asks on every page.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if sch, ok := c.schemas.get(ref); ok {
		return sch, nil
	}
	labels, err := c.labels(ctx, op, ref.Namespace)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(labels, ref.Name) {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, errors.New("no node has the label "+ref.Name))
	}
	rows, err := c.all(ctx, op, ref.Namespace,
		"CALL db.schema.nodeTypeProperties() YIELD nodeLabels, propertyName, propertyTypes, mandatory "+
			"WHERE $1 IN nodeLabels AND propertyName IS NOT NULL RETURN propertyName, propertyTypes, mandatory",
		map[string]any{"1": ref.Name})
	if err != nil {
		return nil, err
	}
	// A property seen under several label combinations is merged: the
	// union of its types, mandatory only when it always is.
	type prop struct {
		types     []string
		mandatory bool
		seen      bool
	}
	props := map[string]*prop{}
	for _, r := range rows {
		name, _ := r[0].(string)
		p := props[name]
		if p == nil {
			p = &prop{mandatory: true}
			props[name] = p
		}
		if ts, ok := r[1].([]any); ok {
			for _, t := range ts {
				if s, ok := t.(string); ok && !slices.Contains(p.types, s) {
					p.types = append(p.types, s)
				}
			}
		}
		m, _ := r[2].(bool)
		p.mandatory = p.mandatory && m
		p.seen = true
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range props {
			if !yield(k) {
				return
			}
		}
	})
	fields := []driver.Field{{Name: idColumn, NativeType: "ElementId", Type: driver.TypeString, Generated: true, Comment: "element id"}}
	for _, n := range names {
		p := props[n]
		slices.Sort(p.types)
		fields = append(fields, driver.Field{
			Name: n, NativeType: strings.Join(p.types, " | "), Type: typeOfTypes(p.types), Nullable: !p.mandatory,
		})
	}
	sch := &driver.CollectionSchema{
		Ref: ref, Type: driver.CollectionCollection, Fields: fields, PrimaryKey: []string{idColumn},
		RowEstimate: -1, SizeBytes: -1, Schemaless: true, Comment: "node label",
	}
	if n, err := c.countLabel(ctx, op, ref.Namespace, ref.Name); err == nil {
		sch.RowEstimate = n
	}
	sch.Indexes = c.indexes(ctx, ref)
	c.schemas.put(ref, sch)
	return sch, nil
}

// typeOfTypes is the driver.Type of a property that holds the given
// Cypher types: theirs when they agree, else unknown.
func typeOfTypes(types []string) driver.Type {
	if len(types) == 0 {
		return driver.TypeUnknown
	}
	t := typeOfNative(types[0])
	for _, s := range types[1:] {
		if typeOfNative(s) != t {
			return driver.TypeUnknown
		}
	}
	return t
}

// indexes lists the indexes on a label; a failure (an old server, a user
// who may not SHOW INDEXES) leaves them out.
func (c *conn) indexes(ctx context.Context, ref driver.CollectionRef) []driver.Index {
	rows, err := c.all(ctx, "describe", ref.Namespace,
		"SHOW INDEXES YIELD name, type, labelsOrTypes, properties, owningConstraint WHERE $1 IN labelsOrTypes RETURN name, type, properties, owningConstraint",
		map[string]any{"1": ref.Name})
	if err != nil {
		return nil
	}
	var out []driver.Index
	for _, r := range rows {
		name, _ := r[0].(string)
		typ, _ := r[1].(string)
		var fields []string
		if ps, ok := r[2].([]any); ok {
			for _, p := range ps {
				if s, ok := p.(string); ok {
					fields = append(fields, s)
				}
			}
		}
		out = append(out, driver.Index{Name: name, Fields: fields, Method: strings.ToLower(typ), Unique: r[3] != nil})
	}
	return out
}
