package chroma

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/xavidop/tobira/driver"
)

const (
	listPage       = 100   // databases or collections per list request
	maxListed      = 10000 // cap on listed databases or collections
	describeSample = 100   // records Describe samples
	sampleRows     = 20    // of which the schema keeps as Sample
)

// collectionName is Chroma's rule for collection names: 3 to 512 of
// [a-zA-Z0-9._-], starting and ending with a letter or digit.
var collectionName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{1,510}[a-zA-Z0-9]$`)

// collInfo is what the driver needs to know about a collection.
type collInfo struct {
	ref     driver.CollectionRef
	id      string
	space   string // l2, cosine or ip
	method  string // hnsw or spann
	dim     int    // 0 = unknown (no record yet)
	model   collectionModel
	vecType string
}

// database resolves a namespace to a database name: "" is the connection's.
func (c *conn) databaseFor(op, ns string) (string, error) {
	if ns == "" {
		return c.database, nil
	}
	if !validName(ns) {
		return "", driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("database %q does not exist", ns))
	}
	return ns, nil
}

// collection reads a collection's description. A missing one, or a name
// Chroma could not have created, is ErrNotFound.
func (c *conn) collection(ctx context.Context, op string, ref driver.CollectionRef) (*collInfo, error) {
	db, err := c.databaseFor(op, ref.Namespace)
	if err != nil {
		return nil, err
	}
	if !collectionName.MatchString(ref.Name) || strings.Contains(ref.Name, "..") {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("collection %q does not exist (not a valid chroma collection name)", ref.Name))
	}
	m, err := c.api.getCollection(ctx, c.tenant, db, ref.Name)
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	info := &collInfo{ref: driver.CollectionRef{Namespace: db, Name: m.Name}, id: m.ID, model: m}
	if m.Dimension != nil {
		info.dim = *m.Dimension
	}
	info.space, info.method = vectorSpace(m)
	info.vecType = fmt.Sprintf("vector(%s)", info.space)
	if info.dim > 0 {
		info.vecType = fmt.Sprintf("vector(%d, %s)", info.dim, info.space)
	}
	return info, nil
}

// vectorSpace reads the distance space and index method from the
// configuration (hnsw or spann), else from the schema's #embedding vector
// index; Chroma's default is l2 with hnsw.
func vectorSpace(m collectionModel) (space, method string) {
	var cfg struct {
		HNSW  *struct{ Space string } `json:"hnsw"`
		SPANN *struct{ Space string } `json:"spann"`
	}
	_ = json.Unmarshal(m.Configuration, &cfg)
	switch {
	case cfg.SPANN != nil && cfg.SPANN.Space != "":
		return cfg.SPANN.Space, "spann"
	case cfg.HNSW != nil && cfg.HNSW.Space != "":
		return cfg.HNSW.Space, "hnsw"
	}
	var sch struct {
		Keys map[string]struct {
			FloatList struct {
				VectorIndex struct {
					Config struct {
						Space string          `json:"space"`
						SPANN json.RawMessage `json:"spann"`
					} `json:"config"`
				} `json:"vector_index"`
			} `json:"float_list"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(m.Schema, &sch)
	if vi := sch.Keys["#embedding"].FloatList.VectorIndex.Config; vi.Space != "" {
		method = "hnsw"
		if len(vi.SPANN) > 0 && string(vi.SPANN) != "null" {
			method = "spann"
		}
		return vi.Space, method
	}
	return "l2", "hnsw"
}

// Namespaces lists the tenant's databases. A token that may not list them
// sees only its own database.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	var out []driver.Namespace
	seenDefault := false
	for offset := 0; offset < maxListed; offset += listPage {
		dbs, err := c.api.listDatabases(ctx, c.tenant, listPage, offset)
		if err != nil {
			err = c.fail(ctx, "namespaces", err)
			if errors.Is(err, driver.ErrPermission) || errors.Is(err, driver.ErrNotFound) {
				return []driver.Namespace{{Name: c.database, Label: "database", Default: true}}, nil
			}
			return nil, err
		}
		for _, d := range dbs {
			def := d.Name == c.database
			seenDefault = seenDefault || def
			out = append(out, driver.Namespace{Name: d.Name, Label: "database", Default: def})
		}
		if len(dbs) < listPage {
			break
		}
	}
	if !seenDefault {
		out = append([]driver.Namespace{{Name: c.database, Label: "database", Default: true}}, out...)
	}
	return out, nil
}

// Collections lists a database's collections (at most 10,000).
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	db, err := c.databaseFor("collections", ns.Name)
	if err != nil {
		return nil, err
	}
	var out []driver.Collection
	for offset := 0; offset < maxListed; offset += listPage {
		cs, err := c.api.listCollections(ctx, c.tenant, db, listPage, offset)
		if err != nil {
			return nil, c.fail(ctx, "collections", err)
		}
		for _, m := range cs {
			out = append(out, driver.Collection{Ref: driver.CollectionRef{Namespace: db, Name: m.Name}, Type: driver.CollectionCollection, RowEstimate: -1})
		}
		if len(cs) < listPage {
			break
		}
	}
	return out, nil
}

// Describe reports a collection's columns: id (the key), document, uri
// when the sample has one, the embedding, then the metadata keys typed
// from a sample of records. Chroma has no schema for metadata, so the
// schema is marked Schemaless.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	info, err := c.collection(ctx, "describe", ref)
	if err != nil {
		return nil, err
	}
	n, err := c.api.count(ctx, c.tenant, info.ref.Namespace, info.id)
	if err != nil {
		return nil, c.fail(ctx, "describe", err)
	}
	limit := describeSample
	resp, err := c.api.get(ctx, c.tenant, info.ref.Namespace, info.id, getRequest{Limit: &limit, Include: []string{"documents", "metadatas", "uris"}})
	if err != nil {
		return nil, c.fail(ctx, "describe", err)
	}
	recs, err := getRecords(resp)
	if err != nil {
		return nil, driver.Wrap(nil, Name, "describe", err)
	}
	hasURI := false
	for _, r := range recs {
		hasURI = hasURI || r.uri != nil
	}
	sh := newShape(recs, shapeOpts{documents: true, uris: hasURI, embeddings: true, metadatas: true, vecType: info.vecType})
	sch := &driver.CollectionSchema{
		Ref:         info.ref,
		Type:        driver.CollectionCollection,
		PrimaryKey:  []string{idColumn},
		RowEstimate: n,
		SizeBytes:   -1,
		Schemaless:  true,
		Indexes:     []driver.Index{{Name: "#embedding", Fields: []string{embeddingColumn}, Method: info.method + " " + info.space}},
		DDL:         indentJSON(info.model.Configuration),
	}
	for _, col := range sh.columns() {
		sch.Fields = append(sch.Fields, driver.Field{
			Name:       col.Name,
			NativeType: col.NativeType,
			Type:       col.Type,
			Nullable:   col.Name != idColumn && col.Name != embeddingColumn,
		})
	}
	for i, r := range recs {
		if i == sampleRows {
			break
		}
		sch.Sample = append(sch.Sample, sh.row(r))
	}
	if md := bytes.TrimSpace(info.model.Metadata); len(md) > 0 && string(md) != "null" {
		sch.Comment = "metadata: " + string(md)
	}
	return sch, nil
}

func indentJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") != nil {
		return string(raw)
	}
	return buf.String()
}
