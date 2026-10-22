package milvus

import (
	"fmt"
	"slices"
	"strings"

	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/client/v2/column"

	"github.com/xavidop/tobira/driver"
)

// record is one entity of a reply: its static field values by name, its
// dynamic keys and, for a search, its score.
type record struct {
	values map[string]driver.Value
	dyn    []driver.Member
	score  float64
}

// decodeFields turns a reply's columnar field data into records. n is the
// row count when no column says it (a search's topk); -1 means "from the
// columns".
func decodeFields(fields []*schemapb.FieldData, n int) ([]record, error) {
	type col struct {
		name    string
		c       column.Column
		dynamic bool
	}
	var cols []col
	for _, fd := range fields {
		c, err := column.FieldDataColumn(fd, 0, -1)
		if err != nil {
			return nil, fmt.Errorf("decoding field %q: %w", fd.GetFieldName(), err)
		}
		dyn := fd.GetIsDynamic() || fd.GetFieldName() == dynamicField
		cols = append(cols, col{name: fd.GetFieldName(), c: c, dynamic: dyn})
		if n < 0 || c.Len() < n {
			n = c.Len()
		}
	}
	n = max(n, 0)
	recs := make([]record, n)
	for i := range recs {
		recs[i].values = make(map[string]driver.Value, len(cols))
		for _, c := range cols {
			if !c.dynamic {
				recs[i].values[c.name] = cellValue(c.c, i)
				continue
			}
			v := cellValue(c.c, i)
			raw, _ := v.AsJSON()
			ms, err := dynamicMembers(raw)
			if err != nil {
				return nil, err
			}
			recs[i].dyn = ms
		}
	}
	return recs, nil
}

// shape fixes a result's columns: the primary key, the score column (if
// any), the other static fields in schema order, then the dynamic keys of
// the first page, in key order. Dynamic keys first seen on a later page
// have no column; they are reported in a warning.
type shape struct {
	s      *collSchema
	static []*fieldInfo
	score  string
	keys   []string
	keyIdx map[string]int
}

// newShape builds the shape of records whose static fields are static.
func newShape(s *collSchema, static []*fieldInfo, score string, first []record) *shape {
	pk := s.pkField()
	ordered := []*fieldInfo{}
	for _, f := range static {
		if f.name == pk.name {
			ordered = append([]*fieldInfo{f}, ordered...)
		} else {
			ordered = append(ordered, f)
		}
	}
	seen := map[string]bool{}
	var keys []string
	for _, r := range first {
		for _, m := range r.dyn {
			if !seen[m.Key] {
				seen[m.Key] = true
				keys = append(keys, m.Key)
			}
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return strings.Compare(s.columnName(a), s.columnName(b)) })
	idx := make(map[string]int, len(keys))
	for i, k := range keys {
		idx[k] = i
	}
	return &shape{s: s, static: ordered, score: score, keys: keys, keyIdx: idx}
}

// columns are the result columns. A dynamic key's type is the type its
// first page values agree on, TypeUnknown when they differ.
func (sh *shape) columns(first []record) []driver.Column {
	src := &driver.CollectionRef{Namespace: sh.s.db, Name: sh.s.name}
	var cols []driver.Column
	for i, f := range sh.static {
		if i == 1 && sh.score != "" {
			cols = append(cols, driver.Column{Name: sh.score, NativeType: "float", Type: driver.TypeFloat})
		}
		cols = append(cols, driver.Column{Name: f.name, NativeType: f.nativeType(), Type: f.driverType(), Source: src, Key: f.pk})
	}
	if len(sh.static) < 2 && sh.score != "" {
		cols = append(cols, driver.Column{Name: sh.score, NativeType: "float", Type: driver.TypeFloat})
	}
	for _, k := range sh.keys {
		t := driver.TypeNull
		for _, r := range first {
			for _, m := range r.dyn {
				if m.Key != k || m.Value.IsNull() {
					continue
				}
				switch {
				case t == driver.TypeNull:
					t = m.Value.Type()
				case t != m.Value.Type():
					t = driver.TypeUnknown
				}
			}
		}
		if t == driver.TypeNull {
			t = driver.TypeUnknown
		}
		cols = append(cols, driver.Column{Name: sh.s.columnName(k), NativeType: "dynamic", Type: t, Source: src})
	}
	return cols
}

// row lays out r in the shape's columns; dropped are its dynamic keys
// without a column.
func (sh *shape) row(r record) (driver.Row, []string) {
	n := len(sh.static) + len(sh.keys)
	if sh.score != "" {
		n++
	}
	row := make(driver.Row, 0, n)
	for i, f := range sh.static {
		if i == 1 && sh.score != "" {
			row = append(row, driver.Float(r.score))
		}
		v, ok := r.values[f.name]
		if !ok {
			v = driver.Null()
		}
		row = append(row, v)
	}
	if len(sh.static) < 2 && sh.score != "" {
		row = append(row, driver.Float(r.score))
	}
	dyn := make([]driver.Value, len(sh.keys))
	for i := range dyn {
		dyn[i] = driver.Null()
	}
	var dropped []string
	for _, m := range r.dyn {
		if i, ok := sh.keyIdx[m.Key]; ok {
			dyn[i] = m.Value
		} else {
			dropped = append(dropped, m.Key)
		}
	}
	return append(row, dyn...), dropped
}

// rows lays out records, collecting dropped keys.
func (sh *shape) rows(recs []record, dropped droppedKeys) []driver.Row {
	out := make([]driver.Row, len(recs))
	for i, r := range recs {
		var d []string
		out[i], d = sh.row(r)
		if dropped != nil {
			dropped.add(d)
		}
	}
	return out
}

// staticFields are the static fields an output-field list selects: every
// field for none or "*", else the named ones (plus the primary key, which
// Milvus always returns). Names that are not static fields are dynamic
// keys and select nothing here.
func staticFields(s *collSchema, output []string) []*fieldInfo {
	all := len(output) == 0 || slices.Contains(output, "*")
	var out []*fieldInfo
	for i := range s.fields {
		f := &s.fields[i]
		if all || f.pk || slices.Contains(output, f.name) {
			out = append(out, f)
		}
	}
	return out
}

// droppedKeys collects dynamic keys that had no column, for one warning.
type droppedKeys map[string]bool

func (d droppedKeys) add(keys []string) {
	for _, k := range keys {
		d[k] = true
	}
}

func (d droppedKeys) warnings() []string {
	if len(d) == 0 {
		return nil
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	const show = 10
	more := ""
	if len(keys) > show {
		more = fmt.Sprintf(" and %d more", len(keys)-show)
		keys = keys[:show]
	}
	return []string{fmt.Sprintf("dynamic fields first seen after the first page are not shown: %s%s (browse the collection to see every field)", strings.Join(keys, ", "), more)}
}
