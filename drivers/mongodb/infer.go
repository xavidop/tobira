package mongodb

import (
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

// maxFieldDepth caps how deep Describe infers nested Fields.
const maxFieldDepth = 8

// fieldAcc gathers what a set of documents holds under one field name.
type fieldAcc struct {
	name     string
	present  int // documents holding the field
	nulls    int // of which null or undefined
	types    []driver.Type
	natives  []string
	subdocs  []bson.Raw // values that are documents
	elemdocs []bson.Raw // documents inside array values
}

func (a *fieldAcc) add(rv bson.RawValue, nested bool) {
	a.present++
	if rv.Type == bson.TypeNull || rv.Type == bson.TypeUndefined {
		a.nulls++
		return
	}
	if t := valueType(rv); !slices.Contains(a.types, t) {
		a.types = append(a.types, t)
	}
	if n := nativeName(rv); !slices.Contains(a.natives, n) {
		a.natives = append(a.natives, n)
	}
	if !nested {
		return
	}
	switch rv.Type {
	case bson.TypeEmbeddedDocument:
		a.subdocs = append(a.subdocs, rv.Document())
	case bson.TypeArray:
		vals, _ := rv.Array().Values()
		for _, e := range vals {
			if d, ok := e.DocumentOK(); ok {
				a.elemdocs = append(a.elemdocs, d)
			}
		}
	}
}

// typeAndNative returns the field's type: the one driver type its non-null
// values share (TypeUnknown when they differ, TypeNull when all are null),
// and the native type names sorted and joined with "|", such as
// "int|string".
func (a *fieldAcc) typeAndNative() (driver.Type, string) {
	native := strings.Join(slices.Sorted(slices.Values(a.natives)), "|")
	switch len(a.types) {
	case 0:
		return driver.TypeNull, "null"
	case 1:
		return a.types[0], native
	}
	return driver.TypeUnknown, native
}

// valueType is the driver type valueOf gives rv, without building the value.
func valueType(rv bson.RawValue) driver.Type {
	switch rv.Type {
	case bson.TypeDouble:
		return driver.TypeFloat
	case bson.TypeString, bson.TypeSymbol:
		return driver.TypeString
	case bson.TypeInt32, bson.TypeInt64:
		return driver.TypeInt
	case bson.TypeDecimal128:
		return driver.TypeDecimal
	case bson.TypeBoolean:
		return driver.TypeBool
	case bson.TypeDateTime:
		return driver.TypeTime
	case bson.TypeNull, bson.TypeUndefined:
		return driver.TypeNull
	case bson.TypeEmbeddedDocument:
		return driver.TypeObject
	case bson.TypeArray:
		return driver.TypeList
	case bson.TypeBinary:
		if sub, _, ok := rv.BinaryOK(); ok && sub == bson.TypeBinaryGeneric {
			return driver.TypeBytes
		}
	}
	return driver.TypeUnknown
}

// accumulate gathers the fields of docs in first-seen order, with _id
// first when any document has one. A field repeated inside one document
// counts once (the first).
func accumulate(docs []bson.Raw, nested bool) []*fieldAcc {
	var out []*fieldAcc
	index := map[string]*fieldAcc{}
	for _, d := range docs {
		elems, err := d.Elements()
		if err != nil {
			continue
		}
		seen := make(map[string]bool, len(elems))
		for _, e := range elems {
			k := e.Key()
			if seen[k] {
				continue
			}
			seen[k] = true
			a, ok := index[k]
			if !ok {
				a = &fieldAcc{name: k}
				index[k] = a
				out = append(out, a)
			}
			a.add(e.Value(), nested)
		}
	}
	if i := slices.IndexFunc(out, func(a *fieldAcc) bool { return a.name == "_id" }); i > 0 {
		id := out[i]
		out = append(out[:i], out[i+1:]...)
		out = append([]*fieldAcc{id}, out...)
	}
	return out
}

// inferColumns returns the result columns for docs: the union of their
// top-level fields, _id first, then in first-seen order.
func inferColumns(docs []bson.Raw) []driver.Column {
	accs := accumulate(docs, false)
	cols := make([]driver.Column, len(accs))
	for i, a := range accs {
		cols[i].Name = a.name
		cols[i].Type, cols[i].NativeType = a.typeAndNative()
	}
	return cols
}

// inferFields returns the schema fields of docs, like inferColumns, with
// Nullable set for a field missing from (or null in) some document, and
// nested Fields for documents and for documents inside arrays.
func inferFields(docs []bson.Raw) []driver.Field {
	return inferFieldsDepth(docs, 0)
}

func inferFieldsDepth(docs []bson.Raw, depth int) []driver.Field {
	accs := accumulate(docs, depth < maxFieldDepth)
	out := make([]driver.Field, len(accs))
	for i, a := range accs {
		f := driver.Field{Name: a.name, Nullable: a.present < len(docs) || a.nulls > 0}
		f.Type, f.NativeType = a.typeAndNative()
		if nested := slices.Concat(a.subdocs, a.elemdocs); len(nested) > 0 {
			f.Fields = inferFieldsDepth(nested, depth+1) // documents and documents inside arrays, merged
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

// rowOf lays doc out along n columns; a field the document lacks is NULL.
// omitted counts the document's fields that have no column.
func rowOf(doc bson.Raw, index map[string]int, n int) (row driver.Row, omitted int) {
	row = make(driver.Row, n)
	elems, err := doc.Elements()
	if err != nil {
		return row, 1
	}
	filled := make([]bool, n)
	for _, e := range elems {
		i, ok := index[e.Key()]
		if !ok {
			omitted++
			continue
		}
		if !filled[i] {
			row[i] = valueOf(e.Value())
			filled[i] = true
		}
	}
	return row, omitted
}
