package weaviate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/weaviate/weaviate/entities/models"

	"github.com/xavidop/tobira/driver"
)

// idColumn is the column holding the object id.
const idColumn = "id"

// The columns of search results: the distance of a vector search and the
// score of a keyword or hybrid search.
const (
	distanceColumn = "_distance"
	scoreColumn    = "_score"
)

// vectorInfo is one vector of a collection: the legacy unnamed vector
// (name "") or a named vector.
type vectorInfo struct {
	name       string
	column     string
	distance   string
	vectorizer string
	indexType  string
	multi      bool
	// inferenceURL: the module is configured with an inference URL of
	// its own, so even a "local" module may send text anywhere.
	inferenceURL bool
}

// collSchema is what the driver needs of a collection's definition.
type collSchema struct {
	class   string
	props   []propInfo
	vectors []vectorInfo
	raw     *models.Class
}

var namePattern = regexp.MustCompile(`^[_A-Za-z][_0-9A-Za-z]*$`)

// checkName refuses a collection or property name that is not a GraphQL
// name: names are interpolated into GraphQL and REST paths.
func checkName(op, name string) error {
	if !namePattern.MatchString(name) {
		return driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("%q is not a valid Weaviate name (letters, digits and _, not starting with a digit)", name))
	}
	return nil
}

// getSchema reads a collection's definition. Weaviate finds a class by its
// name with the first letter in either case; the canonical name is kept.
func (c *conn) getSchema(ctx context.Context, op, name string) (*collSchema, error) {
	if err := checkName(op, name); err != nil {
		return nil, err
	}
	class, err := c.schema.ClassGetter().WithClassName(name).Do(ctx)
	if err != nil {
		err = mapError(ctx, op, err)
		if errors.Is(err, driver.ErrNotFound) {
			return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("collection %q does not exist", name))
		}
		return nil, err
	}
	return schemaFromClass(class), nil
}

// schemaFromClass reads a class definition. The legacy vector is the
// column "vector" and a named vector the column of its name; a column that
// would clash with a property or the id gets a suffix ("_vector" for the
// legacy one, "<name>_vector" for a named one).
func schemaFromClass(class *models.Class) *collSchema {
	s := &collSchema{class: class.Class, raw: class}
	taken := map[string]bool{idColumn: true}
	for _, p := range class.Properties {
		s.props = append(s.props, propInfo{
			name:       p.Name,
			dataType:   p.DataType,
			nested:     p.NestedProperties,
			desc:       p.Description,
			searchable: p.IndexSearchable == nil && typeOf(p.DataType) == driver.TypeString || p.IndexSearchable != nil && *p.IndexSearchable,
			rangeable:  p.IndexRangeFilters != nil && *p.IndexRangeFilters,
		})
		taken[p.Name] = true
	}
	column := func(name, alt string) string {
		if taken[name] {
			name = alt
		}
		for taken[name] {
			name = "_" + name
		}
		taken[name] = true
		return name
	}
	if class.VectorIndexType != "" {
		v := vectorInfo{vectorizer: class.Vectorizer, indexType: class.VectorIndexType, inferenceURL: moduleHasInferenceURL(class.ModuleConfig, class.Vectorizer)}
		v.distance, v.multi = indexConfig(class.VectorIndexConfig)
		v.column = column("vector", "_vector")
		s.vectors = append(s.vectors, v)
	}
	names := make([]string, 0, len(class.VectorConfig))
	for n := range class.VectorConfig {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		vc := class.VectorConfig[n]
		v := vectorInfo{name: n, vectorizer: vectorizerName(vc.Vectorizer), indexType: vc.VectorIndexType}
		v.inferenceURL = moduleHasInferenceURL(vc.Vectorizer, v.vectorizer) || moduleHasInferenceURL(class.ModuleConfig, v.vectorizer)
		v.distance, v.multi = indexConfig(vc.VectorIndexConfig)
		v.column = column(n, n+"_vector")
		s.vectors = append(s.vectors, v)
	}
	return s
}

// indexConfig reads the distance (cosine when unset) and the multi-vector
// flag of a vector index configuration.
func indexConfig(cfg any) (string, bool) {
	m, _ := cfg.(map[string]any)
	distance, _ := m["distance"].(string)
	if distance == "" {
		distance = "cosine"
	}
	multi := false
	if mv, ok := m["multivector"].(map[string]any); ok {
		multi, _ = mv["enabled"].(bool)
	}
	return distance, multi
}

// vectorizerName reads a named vector's vectorizer: {"text2vec-openai":
// {…}} gives "text2vec-openai"; anything else "" (unknown).
func vectorizerName(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if len(x) == 1 {
			for k := range x {
				return k
			}
		}
	}
	return ""
}

// vectorizerNames lists every vectorizer the collection declares, for the
// read-only guard: the class vectorizer (even without a legacy vector
// index), then each vector's. A module configured with its own inference
// URL is listed as "<module> with an inferenceUrl", which is never local.
func (s *collSchema) vectorizerNames() []string {
	var out []string
	name := func(module string, url bool) string {
		if url {
			return module + " with an inferenceUrl"
		}
		return module
	}
	if s.raw != nil && s.raw.VectorIndexType == "" && s.raw.Vectorizer != "" {
		out = append(out, name(s.raw.Vectorizer, moduleHasInferenceURL(s.raw.ModuleConfig, s.raw.Vectorizer)))
	}
	for _, v := range s.vectors {
		out = append(out, name(v.vectorizer, v.inferenceURL))
	}
	return out
}

// moduleHasInferenceURL reports whether cfg[module] (a module
// configuration) sets any inference URL: inferenceUrl, queryInferenceUrl,
// passageInferenceUrl, imageInferenceUrl and the like.
func moduleHasInferenceURL(cfg any, module string) bool {
	m, _ := cfg.(map[string]any)
	mc, _ := m[module].(map[string]any)
	for k, v := range mc {
		if s, ok := v.(string); ok && s != "" && strings.Contains(strings.ToLower(k), "inferenceurl") {
			return true
		}
	}
	return false
}

func (s *collSchema) prop(name string) (propInfo, bool) {
	i := slices.IndexFunc(s.props, func(p propInfo) bool { return p.name == name })
	if i < 0 {
		return propInfo{}, false
	}
	return s.props[i], true
}

func (s *collSchema) vectorByColumn(col string) (vectorInfo, bool) {
	i := slices.IndexFunc(s.vectors, func(v vectorInfo) bool { return v.column == col })
	if i < 0 {
		return vectorInfo{}, false
	}
	return s.vectors[i], true
}

// readable lists the properties a query returns by default: all but the
// cross-references, which need a fragment per target class.
func (s *collSchema) readable() []propInfo {
	var out []propInfo
	for _, p := range s.props {
		// An object without declared members cannot be selected.
		if !isReference(p.dataType) && (!isObjectType(p.dataType) || len(p.nested) > 0) {
			out = append(out, p)
		}
	}
	return out
}

// isObjectType reports whether a data type is object or object[].
func isObjectType(dt []string) bool {
	t, _ := baseType(dt)
	return t == "object"
}

// fieldProps resolves the requested property names (nil: every readable
// property).
func (s *collSchema) fieldProps(op string, fields []string) ([]propInfo, error) {
	if fields == nil {
		return s.readable(), nil
	}
	out := make([]propInfo, 0, len(fields))
	for _, f := range fields {
		p, ok := s.prop(f)
		if !ok {
			return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("collection %s has no property %q", s.class, f))
		}
		if isReference(p.dataType) {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("property %q is a cross-reference: select it with raw GraphQL ({ Get { %s { %s { ... on Class { … } } } } })", f, s.class, f))
		}
		out = append(out, p)
	}
	return out, nil
}

// selection is the GraphQL selection of the given properties (nil: every
// readable one), the id and, with vectors, every vector.
func (s *collSchema) selection(fields []string, vectors bool) string {
	props, err := s.fieldProps("query", fields)
	if err != nil {
		return ""
	}
	return s.selectionOf(props, vectors, nil)
}

// selectionOf selects props and _additional { id, extra..., vectors }.
func (s *collSchema) selectionOf(props []propInfo, vectors bool, extra []string) string {
	var b strings.Builder
	for _, p := range props {
		b.WriteString(propSelection(p.name, p.dataType, p.nested))
		b.WriteByte(' ')
	}
	b.WriteString("_additional { id")
	for _, e := range extra {
		b.WriteString(" " + e)
	}
	if vectors {
		var named []string
		for _, v := range s.vectors {
			if v.name == "" {
				b.WriteString(" vector")
			} else {
				named = append(named, v.name)
			}
		}
		if len(named) > 0 {
			b.WriteString(" vectors { " + strings.Join(named, " ") + " }")
		}
	}
	b.WriteString(" }")
	return b.String()
}

// propSelection selects one property: object types need their members
// spelled out.
func propSelection(name string, dt []string, nested []*models.NestedProperty) string {
	t, _ := baseType(dt)
	switch t {
	case "geoCoordinates":
		return name + " { latitude longitude }"
	case "phoneNumber":
		return name + " { input internationalFormatted countryCode national nationalFormatted valid }"
	case "object":
		parts := make([]string, 0, len(nested))
		for _, np := range nested {
			parts = append(parts, propSelection(np.Name, np.DataType, np.NestedProperties))
		}
		if len(parts) == 0 {
			return name
		}
		return name + " { " + strings.Join(parts, " ") + " }"
	}
	return name
}

// columnSpec is the shape of a result built from the schema.
type columnSpec struct {
	props   []propInfo
	vectors bool
	extra   string // distanceColumn, scoreColumn or ""
	// distance converts a raw distance to _distance (nil: kept).
	distance func(float64) float64
}

// columns lays out a result: id, the search column, the properties, then
// the vectors.
func (s *collSchema) columns(spec columnSpec) []driver.Column {
	ref := &driver.CollectionRef{Namespace: defaultNamespace, Name: s.class}
	cols := []driver.Column{{Name: idColumn, NativeType: "uuid", Type: driver.TypeString, Source: ref, Key: true}}
	if spec.extra != "" {
		cols = append(cols, driver.Column{Name: spec.extra, NativeType: "number", Type: driver.TypeFloat})
	}
	for _, p := range spec.props {
		cols = append(cols, driver.Column{Name: p.name, NativeType: nativeType(p.dataType), Type: typeOf(p.dataType), Source: ref})
	}
	if spec.vectors {
		for _, v := range s.vectors {
			cols = append(cols, driver.Column{Name: v.column, NativeType: "vector", Type: driver.TypeVector, Source: ref})
		}
	}
	return cols
}

// row converts one object of a Get result to a row laid out by columns.
func (s *collSchema) row(spec columnSpec, obj map[string]any) (driver.Row, string) {
	add, _ := obj["_additional"].(map[string]any)
	uuid, _ := add["id"].(string)
	r := driver.Row{idValue(uuid)}
	switch spec.extra {
	case distanceColumn:
		r = append(r, numberValue(add["distance"], spec.distance))
	case scoreColumn:
		r = append(r, numberValue(add["score"], nil))
	}
	for _, p := range spec.props {
		r = append(r, decodeProp(p.dataType, p.nested, obj[p.name]))
	}
	if spec.vectors {
		named, _ := add["vectors"].(map[string]any)
		for _, v := range s.vectors {
			if v.name == "" {
				r = append(r, vectorValue(add["vector"]))
			} else {
				r = append(r, vectorValue(named[v.name]))
			}
		}
	}
	return r, uuid
}

// numberValue reads a number Weaviate returns as a JSON number or, for
// scores, a string.
func numberValue(v any, conv func(float64) float64) driver.Value {
	var f float64
	switch x := v.(type) {
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return driver.Null()
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(x, 64); err != nil {
			return driver.String(x)
		}
	default:
		return driver.Null()
	}
	if conv != nil {
		f = conv(f)
	}
	return driver.Float(f)
}

// idType is the type of the ids in rows (column 0): Int when every id is
// an integer, String when none is, Unknown when they mix.
func idType(rows []driver.Row) driver.Type {
	ints, strs := 0, 0
	for _, r := range rows {
		switch r[0].Type() {
		case driver.TypeInt:
			ints++
		default:
			strs++
		}
	}
	switch {
	case ints > 0 && strs == 0:
		return driver.TypeInt
	case ints > 0:
		return driver.TypeUnknown
	}
	return driver.TypeString
}
