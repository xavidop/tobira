package chroma

import (
	"fmt"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// record is one Chroma record as a reply carries it. Parts the request did
// not include are nil.
type record struct {
	id        string
	document  *string
	uri       *string
	embedding []float32
	metadata  map[string]any
	distance  *float64 // query replies
	query     int      // query replies: the index of the query it answers
}

// shapeOpts says which parts become columns.
type shapeOpts struct {
	ref        *driver.CollectionRef // Column.Source for editable columns
	documents  bool
	uris       bool
	embeddings bool
	metadatas  bool
	distances  bool
	queries    bool   // a _query column (several query embeddings)
	vecType    string // embedding NativeType, "" = "vector"
}

type metaCol struct {
	key, name string
	typ       driver.Type
	native    string
}

// shape turns records into rows. The columns are fixed by the records it is
// built from (the first page): the id column is Int when every id is a
// canonical integer, else String; a metadata column has its values' type
// (Int and Float together make Float, other mixes Unknown). Keys first seen
// later are left out and reported by warnings.
type shape struct {
	o       shapeOpts
	idInt   bool
	metas   []metaCol
	index   map[string]int
	renamed []string
	late    map[string]bool
	textIDs bool // a later id was not an integer in an Int id column
}

func newShape(recs []record, o shapeOpts) *shape {
	s := &shape{o: o, index: map[string]int{}, late: map[string]bool{}, idInt: len(recs) > 0}
	for _, r := range recs {
		if _, ok := canonicalInt(r.id); !ok {
			s.idInt = false
		}
	}
	if !o.metadatas {
		return s
	}
	types := map[string]map[driver.Type]bool{}
	native := map[string]string{}
	for _, r := range recs {
		for k, x := range r.metadata {
			v := metaValue(x)
			if v.IsNull() {
				continue
			}
			if types[k] == nil {
				types[k] = map[driver.Type]bool{}
				native[k] = nativeType(v)
			} else if native[k] != nativeType(v) {
				native[k] = "mixed"
			}
			types[k][v.Type()] = true
		}
	}
	for k, ts := range types {
		c := metaCol{key: k, name: displayName(k), typ: driver.TypeUnknown, native: native[k]}
		switch {
		case len(ts) == 1:
			for t := range ts {
				c.typ = t
			}
		case len(ts) == 2 && ts[driver.TypeInt] && ts[driver.TypeFloat]:
			c.typ, c.native = driver.TypeFloat, "float"
		}
		s.metas = append(s.metas, c)
	}
	slices.SortFunc(s.metas, func(a, b metaCol) int { return strings.Compare(a.name, b.name) })
	for i, c := range s.metas {
		s.index[c.key] = i
		if c.name != c.key {
			s.renamed = append(s.renamed, fmt.Sprintf("metadata key %q is shown as column %q", c.key, c.name))
		}
	}
	return s
}

func (s *shape) columns() []driver.Column {
	var cols []driver.Column
	if s.o.queries {
		cols = append(cols, driver.Column{Name: queryColumn, NativeType: "query index", Type: driver.TypeInt})
	}
	idType := driver.TypeString
	if s.idInt {
		idType = driver.TypeInt
	}
	cols = append(cols, driver.Column{Name: idColumn, NativeType: "id", Type: idType, Source: s.o.ref, Key: true})
	if s.o.documents {
		cols = append(cols, driver.Column{Name: documentColumn, NativeType: "document", Type: driver.TypeString, Source: s.o.ref})
	}
	if s.o.uris {
		cols = append(cols, driver.Column{Name: uriColumn, NativeType: "uri", Type: driver.TypeString, Source: s.o.ref})
	}
	if s.o.embeddings {
		vt := s.o.vecType
		if vt == "" {
			vt = "vector"
		}
		cols = append(cols, driver.Column{Name: embeddingColumn, NativeType: vt, Type: driver.TypeVector, Source: s.o.ref})
	}
	for _, c := range s.metas {
		cols = append(cols, driver.Column{Name: c.name, NativeType: c.native, Type: c.typ, Source: s.o.ref})
	}
	if s.o.distances {
		cols = append(cols, driver.Column{Name: distanceColumn, NativeType: "distance", Type: driver.TypeFloat})
	}
	return cols
}

func strOrNull(p *string) driver.Value {
	if p == nil {
		return driver.Null()
	}
	return driver.String(*p)
}

func (s *shape) row(r record) driver.Row {
	row := make(driver.Row, 0, len(s.metas)+6)
	if s.o.queries {
		row = append(row, driver.Int(int64(r.query)))
	}
	id := idValue(r.id)
	if !s.idInt {
		id = driver.String(r.id)
	} else if id.Type() != driver.TypeInt {
		s.textIDs = true
	}
	row = append(row, id)
	if s.o.documents {
		row = append(row, strOrNull(r.document))
	}
	if s.o.uris {
		row = append(row, strOrNull(r.uri))
	}
	if s.o.embeddings {
		if r.embedding == nil {
			row = append(row, driver.Null())
		} else {
			row = append(row, driver.Vector(r.embedding))
		}
	}
	metaStart := len(row)
	for range s.metas {
		row = append(row, driver.Null())
	}
	if s.o.metadatas {
		for k, x := range r.metadata {
			i, ok := s.index[k]
			if !ok {
				if x != nil {
					s.late[k] = true
				}
				continue
			}
			v := metaValue(x)
			if s.metas[i].typ == driver.TypeFloat && v.Type() == driver.TypeInt {
				n, _ := v.AsInt()
				v = driver.Float(float64(n))
			}
			row[metaStart+i] = v
		}
	}
	if s.o.distances {
		if r.distance == nil {
			row = append(row, driver.Null())
		} else {
			row = append(row, driver.Float(*r.distance))
		}
	}
	return row
}

// warnings lists renamed keys and keys first seen after the columns were
// fixed.
func (s *shape) warnings() []string {
	out := slices.Clone(s.renamed)
	if len(s.late) > 0 {
		keys := make([]string, 0, len(s.late))
		for k := range s.late {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		const show = 10
		more := ""
		if len(keys) > show {
			more = fmt.Sprintf(" and %d more", len(keys)-show)
			keys = keys[:show]
		}
		out = append(out, fmt.Sprintf("metadata keys first seen after the first page are not shown: %s%s (browse the collection to see every key)", strings.Join(keys, ", "), more))
	}
	if s.textIDs {
		out = append(out, "some ids on later pages are not integers: they are shown as text in the integer id column")
	}
	return out
}

// getRecords converts a /get reply. A metadata object Chroma could not
// have sent is an error.
func getRecords(resp getResponse) ([]record, error) {
	recs := make([]record, len(resp.IDs))
	for i, id := range resp.IDs {
		r := record{id: id}
		if i < len(resp.Documents) {
			r.document = resp.Documents[i]
		}
		if i < len(resp.URIs) {
			r.uri = resp.URIs[i]
		}
		if i < len(resp.Embeddings) && resp.Embeddings[i] != nil {
			r.embedding = numbersToVector(resp.Embeddings[i])
		}
		if i < len(resp.Metadatas) {
			m, err := decodeMetadata(resp.Metadatas[i])
			if err != nil {
				return nil, err
			}
			r.metadata = m
		}
		recs[i] = r
	}
	return recs, nil
}

// queryRecords converts a /query reply, query by query, with Chroma's
// distances passed through conv.
func queryRecords(resp queryResponse, conv func(float64) float64) ([]record, error) {
	var recs []record
	for q, ids := range resp.IDs {
		at := func(n int) bool { return n > q }
		for i, id := range ids {
			r := record{id: id, query: q}
			if at(len(resp.Documents)) && i < len(resp.Documents[q]) {
				r.document = resp.Documents[q][i]
			}
			if at(len(resp.URIs)) && i < len(resp.URIs[q]) {
				r.uri = resp.URIs[q][i]
			}
			if at(len(resp.Embeddings)) && i < len(resp.Embeddings[q]) && resp.Embeddings[q][i] != nil {
				r.embedding = numbersToVector(resp.Embeddings[q][i])
			}
			if at(len(resp.Metadatas)) && i < len(resp.Metadatas[q]) {
				m, err := decodeMetadata(resp.Metadatas[q][i])
				if err != nil {
					return nil, err
				}
				r.metadata = m
			}
			if at(len(resp.Distances)) && i < len(resp.Distances[q]) && resp.Distances[q][i] != nil {
				d, err := resp.Distances[q][i].Float64()
				if err != nil {
					return nil, fmt.Errorf("unexpected distance from chroma: %s", resp.Distances[q][i])
				}
				d = conv(d)
				r.distance = &d
			}
			recs = append(recs, r)
		}
	}
	return recs, nil
}
