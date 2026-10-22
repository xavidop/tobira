package tools

import (
	"context"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/session"
)

// Connection is one entry of list_connections.
type Connection struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Kind     string `json:"kind"`
	Dialect  string `json:"dialect"`
	ReadOnly bool   `json:"read_only"`
	// Status is idle, connecting, ready, failed, broken or unavailable (its
	// driver plugin could not report what it is).
	Status string `json:"status"`
	// Error is a redacted one-line reason for a failed, broken or
	// unavailable connection.
	Error string `json:"error,omitempty"`
}

// ListNamespacesInput is the input of list_namespaces.
type ListNamespacesInput struct {
	Connection string `json:"connection" jsonschema:"description=Connection name from list_connections"`
}

// ListCollectionsInput is the input of list_collections.
type ListCollectionsInput struct {
	Connection string `json:"connection" jsonschema:"description=Connection name from list_connections"`
	Namespace  string `json:"namespace,omitempty" jsonschema:"description=Namespace; empty means the connection default"`
}

// DescribeInput is the input of describe.
type DescribeInput struct {
	Connection string `json:"connection" jsonschema:"description=Connection name from list_connections"`
	Namespace  string `json:"namespace,omitempty" jsonschema:"description=Namespace; empty means the connection default"`
	Collection string `json:"collection" jsonschema:"description=Collection (table or view) name"`
}

// NamespacesOutput is the result of list_namespaces.
type NamespacesOutput struct {
	outcome
	Namespaces []Namespace `json:"namespaces,omitempty"`
}

// Namespace is one namespace.
type Namespace struct {
	Name    string `json:"name"`
	Label   string `json:"label,omitempty"`
	Default bool   `json:"default,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// CollectionsOutput is the result of list_collections.
type CollectionsOutput struct {
	outcome
	Collections []Collection `json:"collections,omitempty"`
}

// Collection is one collection of a namespace.
type Collection struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	RowEstimate *int64 `json:"row_estimate,omitempty"`
	Comment     string `json:"comment,omitempty"`
}

// DescribeOutput is the result of describe. It never carries the native
// DDL, which can embed secrets in defaults or comments, nor sample rows.
type DescribeOutput struct {
	outcome
	Namespace  string   `json:"namespace,omitempty"`
	Collection string   `json:"collection,omitempty"`
	Type       string   `json:"type,omitempty"`
	Fields     []Field  `json:"fields,omitempty"`
	PrimaryKey []string `json:"primary_key,omitempty"`
	// PartitionKey and ClusteringKey are a wide-column table's key: the
	// partition columns, then the columns that order rows inside a
	// partition, each with its clustering order.
	PartitionKey  []string     `json:"partition_key,omitempty"`
	ClusteringKey []KeyColumn  `json:"clustering_key,omitempty"`
	Indexes       []Index      `json:"indexes,omitempty"`
	ForeignKeys   []ForeignKey `json:"foreign_keys,omitempty"`
	RowEstimate   *int64       `json:"row_estimate,omitempty"`
	Comment       string       `json:"comment,omitempty"`
}

// Field is one field of a collection.
type Field struct {
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Nullable      bool    `json:"nullable"`
	Default       string  `json:"default,omitempty"`
	Generated     bool    `json:"generated,omitempty"`
	AutoIncrement bool    `json:"auto_increment,omitempty"`
	Comment       string  `json:"comment,omitempty"`
	Fields        []Field `json:"fields,omitempty"`
}

// KeyColumn is one clustering column and its order.
type KeyColumn struct {
	Name string `json:"name"`
	Desc bool   `json:"desc,omitempty"`
}

// Index is one index.
type Index struct {
	Name    string   `json:"name"`
	Fields  []string `json:"fields"`
	Unique  bool     `json:"unique,omitempty"`
	Primary bool     `json:"primary,omitempty"`
}

// ForeignKey is one foreign key; References is "namespace.collection".
type ForeignKey struct {
	Name       string   `json:"name,omitempty"`
	Fields     []string `json:"fields"`
	References string   `json:"references"`
	RefFields  []string `json:"ref_fields,omitempty"`
}

// ListConnections lists the connections the turn may use, in configuration
// order.
func (t *Toolset) ListConnections(ctx context.Context) ([]Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := []Connection{}
	for _, name := range t.deps.Manager.Names() {
		if !t.allowed(ctx, name) {
			continue
		}
		st := t.deps.Manager.State(name)
		c := Connection{
			Name: t.redact(name, name), Driver: st.Info.Name, Kind: string(st.Info.Kind), Dialect: st.Info.Dialect,
			ReadOnly: st.ReadOnly, Status: st.Status.String(),
		}
		if st.Broken {
			c.Status = "broken"
		}
		if st.Broken || st.Status == session.Failed {
			c.Error = firstLine(t.redact(name, st.Err))
		}
		if why, ok := t.deps.Unavailable[name]; ok {
			c.Status, c.Error = "unavailable", firstLine(t.redact(name, why))
		}
		out = append(out, c)
	}
	t.report(ctx, NameListConnections, "", struct{}{}, -1, "")
	return out, nil
}

// ListNamespaces lists a connection's namespaces.
func (t *Toolset) ListNamespaces(ctx context.Context, in ListNamespacesInput) (NamespacesOutput, error) {
	in.Connection = t.scopedConn(ctx, in.Connection)
	return run(t, ctx, NameListNamespaces, in.Connection, in, func(call, _ context.Context) (NamespacesOutput, error) {
		c, err := t.open(call, in.Connection)
		if err != nil {
			return NamespacesOutput{}, err
		}
		nss, err := c.Namespaces(call)
		if err != nil {
			return NamespacesOutput{}, err
		}
		out := NamespacesOutput{Namespaces: []Namespace{}}
		for _, n := range nss {
			out.Namespaces = append(out.Namespaces, Namespace{
				Name: t.text(in.Connection, n.Name), Label: t.text(in.Connection, n.Label), Default: n.Default, Comment: t.text(in.Connection, n.Comment),
			})
		}
		return out, nil
	})
}

// ListCollections lists the collections of a namespace.
func (t *Toolset) ListCollections(ctx context.Context, in ListCollectionsInput) (CollectionsOutput, error) {
	in.Connection = t.scopedConn(ctx, in.Connection)
	return run(t, ctx, NameListCollections, in.Connection, in, func(call, _ context.Context) (CollectionsOutput, error) {
		c, err := t.open(call, in.Connection)
		if err != nil {
			return CollectionsOutput{}, err
		}
		cols, err := c.Collections(call, driver.NamespaceRef{Name: in.Namespace})
		if err != nil {
			return CollectionsOutput{}, err
		}
		out := CollectionsOutput{Collections: []Collection{}}
		for _, col := range cols {
			out.Collections = append(out.Collections, Collection{
				Namespace: t.text(in.Connection, col.Ref.Namespace), Name: t.text(in.Connection, col.Ref.Name),
				Type: string(col.Type), RowEstimate: estimate(col.RowEstimate), Comment: t.text(in.Connection, col.Comment),
			})
		}
		return out, nil
	})
}

// Describe describes one collection, without its DDL or sample rows.
func (t *Toolset) Describe(ctx context.Context, in DescribeInput) (DescribeOutput, error) {
	in.Connection = t.scopedConn(ctx, in.Connection)
	return run(t, ctx, NameDescribe, in.Connection, in, func(call, _ context.Context) (DescribeOutput, error) {
		c, err := t.open(call, in.Connection)
		if err != nil {
			return DescribeOutput{}, err
		}
		s, err := c.Describe(call, driver.CollectionRef{Namespace: in.Namespace, Name: in.Collection})
		if err != nil {
			return DescribeOutput{}, err
		}
		conn := in.Connection
		out := DescribeOutput{
			Namespace: t.text(conn, s.Ref.Namespace), Collection: t.text(conn, s.Ref.Name), Type: string(s.Type),
			Fields: t.fields(conn, s.Fields), RowEstimate: estimate(s.RowEstimate), Comment: t.text(conn, s.Comment),
		}
		for _, k := range s.PrimaryKey {
			out.PrimaryKey = append(out.PrimaryKey, t.text(conn, k))
		}
		out.PartitionKey = t.texts(conn, s.PartitionKey)
		for _, k := range s.ClusteringKey {
			out.ClusteringKey = append(out.ClusteringKey, KeyColumn{Name: t.text(conn, k.Field), Desc: k.Desc})
		}
		for _, ix := range s.Indexes {
			out.Indexes = append(out.Indexes, Index{Name: t.text(conn, ix.Name), Fields: t.texts(conn, ix.Fields), Unique: ix.Unique, Primary: ix.Primary})
		}
		for _, fk := range s.ForeignKeys {
			ref := fk.References
			if ref.Namespace == "" {
				ref.Namespace = s.Ref.Namespace
			}
			out.ForeignKeys = append(out.ForeignKeys, ForeignKey{
				Name: t.text(conn, fk.Name), Fields: t.texts(conn, fk.Fields),
				References: t.text(conn, qualified(ref)), RefFields: t.texts(conn, fk.RefFields),
			})
		}
		return out, nil
	})
}

func (t *Toolset) fields(conn string, fs []driver.Field) []Field {
	var out []Field
	for _, f := range fs {
		typ := f.NativeType
		if typ == "" {
			typ = f.Type.String()
		}
		out = append(out, Field{
			Name: t.text(conn, f.Name), Type: t.text(conn, typ), Nullable: f.Nullable, Default: t.text(conn, f.Default),
			Generated: f.Generated, AutoIncrement: f.AutoIncrement, Comment: t.text(conn, f.Comment), Fields: t.fields(conn, f.Fields),
		})
	}
	return out
}

// text is a connection-derived string as the model sees it: redacted, then
// cut to MaxValueRunes.
func (t *Toolset) text(conn, s string) string {
	if s == "" {
		return ""
	}
	return truncate(t.redact(conn, s), MaxValueRunes)
}

func (t *Toolset) texts(conn string, ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = t.text(conn, s)
	}
	return out
}

// qualified is "namespace.name", or the name alone without a namespace.
func qualified(ref driver.CollectionRef) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "." + ref.Name
}

func estimate(n int64) *int64 {
	if n < 0 {
		return nil
	}
	return &n
}
