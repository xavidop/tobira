package firestore

import (
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Pseudo-columns every document row carries.
const (
	idColumn   = "_id"   // the document id
	pathColumn = "_path" // the document path, for collection-group results
	// idFieldColumn shows a stored field named _id, which the document id
	// column would otherwise hide; filters, sorts and edits map it back.
	idFieldColumn = "_id_field"
)

// storedName is the stored field a column name stands for.
func storedName(col string) string {
	if col == idFieldColumn {
		return idColumn
	}
	return col
}

// maxFieldDepth caps how deep Describe infers nested Fields.
const maxFieldDepth = 8

// docRow is one document read: its id, its path relative to the database
// and its fields, sorted by name. bad notes a document the SDK could not
// decode, whose fields are left out.
type docRow struct {
	id, path string
	fields   driver.Object
	natives  []string // each field's Firestore type, aligned with fields; nil = derive from the value
	bad      bool
	renamed  bool // a stored _id field is shown as _id_field
	hidden   bool // a stored _id_field field is hidden by the renamed _id
}

// fieldAcc gathers what a set of documents holds under one field name.
type fieldAcc struct {
	name    string
	present int // documents holding the field
	nulls   int // of which null
	types   []driver.Type
	natives []string
	nested  []driver.Object // map values, and maps inside arrays
}

func (a *fieldAcc) add(v driver.Value, native string, nested bool) {
	a.present++
	if v.IsNull() {
		a.nulls++
		return
	}
	if t := v.Type(); !slices.Contains(a.types, t) {
		a.types = append(a.types, t)
	}
	if !slices.Contains(a.natives, native) {
		a.natives = append(a.natives, native)
	}
	if !nested {
		return
	}
	switch v.Type() {
	case driver.TypeObject:
		o, _ := v.AsObject()
		a.nested = append(a.nested, o)
	case driver.TypeList:
		list, _ := v.AsList()
		for _, e := range list {
			if o, ok := e.AsObject(); ok {
				a.nested = append(a.nested, o)
			}
		}
	}
}

// typeAndNative returns the field's type: the one type its non-null values
// share, Float for a mix of integers and doubles (JavaScript clients store
// whole numbers as integers), TypeUnknown for any other mix and TypeNull
// when all are null; and the native names sorted and joined with "|".
func (a *fieldAcc) typeAndNative() (driver.Type, string) {
	native := strings.Join(slices.Sorted(slices.Values(a.natives)), "|")
	switch len(a.types) {
	case 0:
		return driver.TypeNull, "null"
	case 1:
		return a.types[0], native
	case 2:
		if slices.Contains(a.types, driver.TypeInt) && slices.Contains(a.types, driver.TypeFloat) {
			return driver.TypeFloat, native
		}
	}
	return driver.TypeUnknown, native
}

// nativeOf names a converted value's Firestore type for inference.
func nativeOf(v driver.Value) string {
	switch v.Type() {
	case driver.TypeBool:
		return "boolean"
	case driver.TypeInt:
		return "integer"
	case driver.TypeFloat:
		return "double"
	case driver.TypeDecimal:
		return "decimal128"
	case driver.TypeString:
		return "string"
	case driver.TypeBytes:
		return "bytes"
	case driver.TypeTime:
		return "timestamp"
	case driver.TypeList:
		return "array"
	case driver.TypeObject:
		return "map"
	case driver.TypeVector:
		return "vector"
	case driver.TypeUnknown:
		n, _, _ := v.AsUnknown()
		return n
	}
	return "null"
}

// accumulate gathers the fields of objs in first-seen order. natives, when
// not nil, holds each object's field types aligned with its members. A
// field repeated inside one object counts once (the first).
func accumulate(objs []driver.Object, natives [][]string, nested bool) []*fieldAcc {
	var out []*fieldAcc
	index := map[string]*fieldAcc{}
	for oi, o := range objs {
		seen := make(map[string]bool, len(o))
		for mi, m := range o {
			if seen[m.Key] {
				continue
			}
			seen[m.Key] = true
			a, ok := index[m.Key]
			if !ok {
				a = &fieldAcc{name: m.Key}
				index[m.Key] = a
				out = append(out, a)
			}
			native := nativeOf(m.Value)
			if natives != nil && mi < len(natives[oi]) {
				native = natives[oi][mi]
			}
			a.add(m.Value, native, nested)
		}
	}
	return out
}

// idCol and pathCol are the pseudo-columns.
func idCol() driver.Column {
	return driver.Column{Name: idColumn, NativeType: "document id", Type: driver.TypeString, Key: true}
}

func pathCol() driver.Column {
	return driver.Column{Name: pathColumn, NativeType: "document path", Type: driver.TypeString}
}

// inferColumns returns the result columns for rows: _id (the key), _path
// when withPath, then the union of the documents' top-level fields in
// first-seen order. A stored field named like a pseudo-column is hidden
// by it.
func inferColumns(rows []docRow, withPath bool) []driver.Column {
	objs := make([]driver.Object, len(rows))
	natives := make([][]string, len(rows))
	for i, r := range rows {
		objs[i], natives[i] = r.fields, r.natives
	}
	cols := []driver.Column{idCol()}
	if withPath {
		cols = append(cols, pathCol())
	}
	for _, a := range accumulate(objs, natives, false) {
		if a.name == idColumn || withPath && a.name == pathColumn {
			continue
		}
		col := driver.Column{Name: a.name}
		col.Type, col.NativeType = a.typeAndNative()
		cols = append(cols, col)
	}
	return cols
}

// inferFields returns the schema fields of rows: _id first, then their
// fields as inferColumns finds them, with Nullable for a field missing
// from (or null in) some document and nested Fields for maps and for maps
// inside arrays.
func inferFields(rows []docRow) []driver.Field {
	objs := make([]driver.Object, len(rows))
	natives := make([][]string, len(rows))
	for i, r := range rows {
		objs[i], natives[i] = r.fields, r.natives
	}
	fields := []driver.Field{{Name: idColumn, NativeType: "document id", Type: driver.TypeString}}
	for _, f := range inferFieldsDepth(objs, natives, 0) {
		if f.Name != idColumn {
			fields = append(fields, f)
		}
	}
	return fields
}

func inferFieldsDepth(objs []driver.Object, natives [][]string, depth int) []driver.Field {
	accs := accumulate(objs, natives, depth < maxFieldDepth)
	out := make([]driver.Field, len(accs))
	for i, a := range accs {
		f := driver.Field{Name: a.name, Nullable: a.present < len(objs) || a.nulls > 0}
		f.Type, f.NativeType = a.typeAndNative()
		if len(a.nested) > 0 {
			f.Fields = inferFieldsDepth(a.nested, nil, depth+1)
		}
		out[i] = f
	}
	return out
}

// columnIndex maps column names to their positions.
func columnIndex(cols []driver.Column) map[string]int {
	m := make(map[string]int, len(cols))
	for i, c := range cols {
		if _, dup := m[c.Name]; !dup {
			m[c.Name] = i
		}
	}
	return m
}

// rowOf lays a document out along n columns; a field the document lacks
// is NULL. omitted counts the document's fields that have no column.
func rowOf(d docRow, index map[string]int, n int, withPath bool) (row driver.Row, omitted int) {
	row = make(driver.Row, n)
	if i, ok := index[idColumn]; ok {
		row[i] = driver.String(d.id)
	}
	if withPath {
		if i, ok := index[pathColumn]; ok {
			row[i] = driver.String(d.path)
		}
	}
	for _, m := range d.fields {
		if m.Key == idColumn || withPath && m.Key == pathColumn {
			continue
		}
		i, ok := index[m.Key]
		if !ok {
			omitted++
			continue
		}
		row[i] = m.Value
	}
	return row, omitted
}
