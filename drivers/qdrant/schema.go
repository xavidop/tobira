package qdrant

import (
	"context"
	"fmt"
	"slices"
	"strings"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
)

// Column names the driver adds to point rows.
const (
	idColumn       = "id"
	vectorColumn   = "vector" // the unnamed default vector
	scoreColumn    = "_score"
	distanceColumn = "_distance"
	// payloadPrefix renames a payload key that clashes with one of the
	// columns above: payload key "id" is column "payload.id".
	payloadPrefix = "payload."
	// metadataKey is the collection metadata entry tobira reads:
	// {"tobira": {"types": {"<payload key>": "<type>"}}}.
	metadataKey = "tobira"
)

// vectorInfo describes one vector of a collection.
type vectorInfo struct {
	name     string // "" = the unnamed default vector
	column   string // result column: name, or "vector" for the default
	size     uint64
	distance pb.Distance
	sparse   bool
	multi    bool
}

// nativeType spells the vector's type for Column.NativeType.
func (v vectorInfo) nativeType() string {
	switch {
	case v.sparse:
		return "sparse vector"
	case v.multi:
		return fmt.Sprintf("multivector(%d, %s)", v.size, v.distance)
	}
	return fmt.Sprintf("vector(%d, %s)", v.size, v.distance)
}

// collSchema is what the driver needs to know about a collection to show
// its points: the vectors and the payload field types.
type collSchema struct {
	ref     driver.CollectionRef
	vectors []vectorInfo
	types   map[string]fieldType            // declared in metadata, or from a datetime index
	indexes map[string]pb.PayloadSchemaType // payload indexes by key
	info    *pb.CollectionInfo
}

// getSchema reads the collection's info. A missing collection is
// ErrNotFound.
func (c *conn) getSchema(ctx context.Context, op, name string) (*collSchema, error) {
	if name == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("no collection named"))
	}
	resp, err := c.client.GetCollectionsClient().Get(ctx, &pb.GetCollectionInfoRequest{CollectionName: name})
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	return schemaFromInfo(driver.CollectionRef{Namespace: defaultNamespace, Name: name}, resp.GetResult()), nil
}

// schemaFromInfo builds a collSchema. Payload field types come from, in
// order of precedence, the collection metadata's tobira.types entry and a
// datetime payload index. Unknown declared types are ignored.
func schemaFromInfo(ref driver.CollectionRef, info *pb.CollectionInfo) *collSchema {
	s := &collSchema{ref: ref, types: map[string]fieldType{}, indexes: map[string]pb.PayloadSchemaType{}, info: info}
	params := info.GetConfig().GetParams()
	switch vc := params.GetVectorsConfig().GetConfig().(type) {
	case *pb.VectorsConfig_Params:
		s.vectors = append(s.vectors, denseInfo("", vc.Params))
	case *pb.VectorsConfig_ParamsMap:
		for name, p := range vc.ParamsMap.GetMap() {
			s.vectors = append(s.vectors, denseInfo(name, p))
		}
	}
	for name := range params.GetSparseVectorsConfig().GetMap() {
		s.vectors = append(s.vectors, vectorInfo{name: name, column: name, sparse: true})
	}
	slices.SortFunc(s.vectors, func(a, b vectorInfo) int { return strings.Compare(a.column, b.column) })
	for key, ps := range info.GetPayloadSchema() {
		s.indexes[key] = ps.GetDataType()
		if ps.GetDataType() == pb.PayloadSchemaType_Datetime {
			s.types[key] = typeTime
		}
	}
	declared := info.GetConfig().GetMetadata()[metadataKey].GetStructValue().GetFields()["types"].GetStructValue().GetFields()
	for key, v := range declared {
		switch t := fieldType(strings.ToLower(v.GetStringValue())); t {
		case typeString, typeInt, typeFloat, typeBool, typeDecimal, typeTime, typeJSON:
			s.types[key] = t
		}
	}
	return s
}

func denseInfo(name string, p *pb.VectorParams) vectorInfo {
	col := name
	if name == "" {
		col = vectorColumn
	}
	return vectorInfo{name: name, column: col, size: p.GetSize(), distance: p.GetDistance(), multi: p.GetMultivectorConfig() != nil}
}

// reserved reports whether col is a column the driver adds.
func (s *collSchema) reserved(col string) bool {
	if col == idColumn || col == scoreColumn || col == distanceColumn {
		return true
	}
	_, ok := s.vectorByColumn(col)
	return ok
}

// column is the result column of payload key k. A key that clashes with a
// driver column, or that already starts with "payload.", is escaped with
// one more "payload." prefix, so payload keys "id" and "payload.id" are
// columns "payload.id" and "payload.payload.id": every key has its own
// column and payloadKey undoes it.
func (s *collSchema) column(k string) string {
	if s.reserved(k) || strings.HasPrefix(k, payloadPrefix) {
		return payloadPrefix + k
	}
	return k
}

// payloadKey is the inverse of column: the payload key behind column col.
func (s *collSchema) payloadKey(col string) string {
	if rest, ok := strings.CutPrefix(col, payloadPrefix); ok {
		return rest
	}
	return col
}

func (s *collSchema) vectorByColumn(col string) (vectorInfo, bool) {
	for _, v := range s.vectors {
		if v.column == col {
			return v, true
		}
	}
	return vectorInfo{}, false
}

// typeOf is the declared type of payload key k.
func (s *collSchema) typeOf(k string) fieldType { return s.types[k] }

// point is one result point, whatever RPC returned it.
type point struct {
	id      *pb.PointId
	payload map[string]*pb.Value
	vectors *pb.VectorsOutput
	score   float32
}

func retrievedPoints(ps []*pb.RetrievedPoint) []point {
	out := make([]point, len(ps))
	for i, p := range ps {
		out[i] = point{id: p.GetId(), payload: p.GetPayload(), vectors: p.GetVectors()}
	}
	return out
}

func scoredPoints(ps []*pb.ScoredPoint) []point {
	out := make([]point, len(ps))
	for i, p := range ps {
		out[i] = point{id: p.GetId(), payload: p.GetPayload(), vectors: p.GetVectors(), score: p.GetScore()}
	}
	return out
}

// shape fixes the columns of a point result: id, then an optional score
// column, the vectors (when requested) and the payload keys in key order.
type shape struct {
	schema      *collSchema
	score       string // scoreColumn, distanceColumn or ""
	scoreFn     func(float32) float64
	withVectors bool
	keys        []string // payload keys, in column order
	keyIndex    map[string]int
}

// newShape builds a shape from the schema's known fields plus every key in
// pts.
func newShape(s *collSchema, pts []point, withVectors bool, score string, scoreFn func(float32) float64) *shape {
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range s.types {
		add(k)
	}
	for k := range s.indexes {
		add(k)
	}
	for _, p := range pts {
		for k := range p.payload {
			add(k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return strings.Compare(s.column(a), s.column(b)) })
	idx := make(map[string]int, len(keys))
	for i, k := range keys {
		idx[k] = i
	}
	return &shape{schema: s, score: score, scoreFn: scoreFn, withVectors: withVectors, keys: keys, keyIndex: idx}
}

// columns lists the result columns; pts types the payload columns that
// have no declared type.
func (sh *shape) columns(pts []point) []driver.Column {
	src := &sh.schema.ref
	idT, idNative := idType(pts)
	cols := []driver.Column{{Name: idColumn, NativeType: idNative, Type: idT, Source: src, Key: true}}
	if sh.score != "" {
		cols = append(cols, driver.Column{Name: sh.score, NativeType: "float", Type: driver.TypeFloat})
	}
	if sh.withVectors {
		for _, v := range sh.schema.vectors {
			t := driver.TypeVector
			if v.sparse {
				t = driver.TypeObject
			} else if v.multi {
				t = driver.TypeList
			}
			cols = append(cols, driver.Column{Name: v.column, NativeType: v.nativeType(), Type: t, Source: src})
		}
	}
	for _, k := range sh.keys {
		decl := sh.schema.typeOf(k)
		t, native := sampleType(pts, k, decl)
		if t == driver.TypeNull && decl != typeNatural {
			t, native = decl.driverType(), string(decl)
		}
		cols = append(cols, driver.Column{Name: sh.schema.column(k), NativeType: native, Type: t, Source: src})
	}
	return cols
}

// sampleType is the type the non-null values of key k share in pts, each
// shown as declared type decl would show it: the declared type when every
// value fits it, or TypeUnknown with a union such as "string|int" when they
// differ, so a column never claims a type its cells do not have.
func sampleType(pts []point, k string, decl fieldType) (driver.Type, string) {
	vals := make([]driver.Value, 0, len(pts))
	for _, p := range pts {
		if v, ok := p.payload[k]; ok {
			vals = append(vals, typedValue(v, decl))
		}
	}
	t, native := inferValues(vals)
	if decl != typeNatural && t == decl.driverType() {
		native = string(decl)
	}
	return t, native
}

// idType is the type of the point ids in pts: Int, String (UUIDs) or a
// union when both appear; Int when there are none.
func idType(pts []point) (driver.Type, string) {
	vals := make([]driver.Value, len(pts))
	for i, p := range pts {
		vals[i] = idValue(p.id)
	}
	t, native := inferValues(vals)
	if t == driver.TypeNull {
		return driver.TypeInt, "point id"
	}
	return t, "point id (" + native + ")"
}

// row builds the row of p. dropped lists payload keys the shape has no
// column for.
func (sh *shape) row(p point) (driver.Row, []string) {
	n := 1 + len(sh.keys)
	if sh.score != "" {
		n++
	}
	if sh.withVectors {
		n += len(sh.schema.vectors)
	}
	r := make(driver.Row, 0, n)
	r = append(r, idValue(p.id))
	if sh.score != "" {
		r = append(r, driver.Float(sh.scoreFn(p.score)))
	}
	if sh.withVectors {
		for _, v := range sh.schema.vectors {
			r = append(r, vectorValue(pointVector(p.vectors, v.name)))
		}
	}
	base := len(r)
	r = r[:base+len(sh.keys)]
	for i, k := range sh.keys {
		if v, ok := p.payload[k]; ok {
			r[base+i] = typedValue(v, sh.schema.typeOf(k))
		}
	}
	var dropped []string
	for k := range p.payload {
		if _, ok := sh.keyIndex[k]; !ok {
			dropped = append(dropped, k)
		}
	}
	return r, dropped
}

// pointVector is the stored vector called name ("" = the unnamed one).
func pointVector(vs *pb.VectorsOutput, name string) *pb.VectorOutput {
	switch o := vs.GetVectorsOptions().(type) {
	case *pb.VectorsOutput_Vector:
		if name == "" {
			return o.Vector
		}
	case *pb.VectorsOutput_Vectors:
		return o.Vectors.GetVectors()[name]
	}
	return nil
}

// inferValues is the type vals share, ignoring NULLs: TypeNull for none,
// TypeUnknown with a union such as "int|string" when they differ.
func inferValues(vals []driver.Value) (driver.Type, string) {
	var types []driver.Type
	for _, v := range vals {
		if !v.IsNull() && !slices.Contains(types, v.Type()) {
			types = append(types, v.Type())
		}
	}
	switch len(types) {
	case 0:
		return driver.TypeNull, ""
	case 1:
		return types[0], types[0].String()
	}
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = t.String()
	}
	return driver.TypeUnknown, strings.Join(names, "|")
}
