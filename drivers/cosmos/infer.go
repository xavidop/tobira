package cosmos

import (
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// valueColumn names the one column of a result whose items are not all
// objects, such as SELECT VALUE COUNT(1).
const valueColumn = "value"

// maxFieldDepth caps how deep Describe infers nested Fields.
const maxFieldDepth = 8

// systemProps are the properties Cosmos DB adds to every item, in the
// order results list them (last, after the item's own fields).
var systemProps = []string{"_rid", "_self", "_etag", "_attachments", "_ts", "_lsn"}

func isSystemProp(name string) bool { return slices.Contains(systemProps, name) }

// fieldAcc gathers what a set of documents holds under one field name.
type fieldAcc struct {
	name    string
	present int // documents holding the field
	nulls   int // of which null
	types   []driver.Type
	natives []string
	nested  []driver.Object // object values, and objects inside array values
}

func (a *fieldAcc) add(v driver.Value, nested bool) {
	a.present++
	if v.IsNull() {
		a.nulls++
		return
	}
	if t := v.Type(); !slices.Contains(a.types, t) {
		a.types = append(a.types, t)
	}
	if n := nativeName(v); !slices.Contains(a.natives, n) {
		a.natives = append(a.natives, n)
	}
	if !nested {
		return
	}
	if o, ok := v.AsObject(); ok {
		a.nested = append(a.nested, o)
	}
	if l, ok := v.AsList(); ok {
		for _, e := range l {
			if o, ok := e.AsObject(); ok {
				a.nested = append(a.nested, o)
			}
		}
	}
}

// nativeName is the JSON type of a decoded value.
func nativeName(v driver.Value) string {
	switch v.Type() {
	case driver.TypeNull:
		return "null"
	case driver.TypeBool:
		return "boolean"
	case driver.TypeInt, driver.TypeFloat, driver.TypeDecimal:
		return "number"
	case driver.TypeString:
		return "string"
	case driver.TypeList:
		return "array"
	case driver.TypeObject:
		return "object"
	}
	return v.Type().String()
}

// typeAndNative returns the field's type and its JSON type names sorted
// and joined with "|". Numbers that are all integers are Int; integers
// mixed with fractions are Float; a number too large for either is
// Decimal. Other mixtures are TypeUnknown, and a field that is always null
// is TypeNull.
func (a *fieldAcc) typeAndNative() (driver.Type, string) {
	native := strings.Join(slices.Sorted(slices.Values(a.natives)), "|")
	if len(a.types) == 0 {
		return driver.TypeNull, "null"
	}
	if len(a.natives) > 1 {
		return driver.TypeUnknown, native
	}
	if len(a.types) == 1 {
		return a.types[0], native
	}
	if slices.Contains(a.types, driver.TypeFloat) {
		return driver.TypeFloat, native
	}
	return driver.TypeDecimal, native
}

// accumulate gathers the fields of docs: id first, then first-seen order,
// then the system properties in their fixed order.
func accumulate(docs []driver.Object, nested bool) []*fieldAcc {
	var out []*fieldAcc
	index := map[string]*fieldAcc{}
	for _, d := range docs {
		seen := make(map[string]bool, len(d))
		for _, m := range d {
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
			a.add(m.Value, nested)
		}
	}
	rank := func(a *fieldAcc) int {
		switch {
		case a.name == "id":
			return 0
		case isSystemProp(a.name):
			return 2 + slices.Index(systemProps, a.name)
		}
		return 1
	}
	slices.SortStableFunc(out, func(x, y *fieldAcc) int { return rank(x) - rank(y) })
	return out
}

// objectsOf returns docs as objects, or ok=false when one is not an object.
func objectsOf(docs []driver.Value) ([]driver.Object, bool) {
	out := make([]driver.Object, len(docs))
	for i, d := range docs {
		o, ok := d.AsObject()
		if !ok {
			return nil, false
		}
		out[i] = o
	}
	return out, true
}

// inferColumns returns the result columns for docs: the union of their
// top-level fields when every item is an object, else one "value" column.
func inferColumns(docs []driver.Value) []driver.Column {
	objs, ok := objectsOf(docs)
	if !ok {
		a := &fieldAcc{name: valueColumn}
		for _, d := range docs {
			a.add(d, false)
		}
		t, native := a.typeAndNative()
		return []driver.Column{{Name: valueColumn, Type: t, NativeType: native}}
	}
	accs := accumulate(objs, false)
	cols := make([]driver.Column, len(accs))
	for i, a := range accs {
		cols[i].Name = a.name
		cols[i].Type, cols[i].NativeType = a.typeAndNative()
	}
	return cols
}

// inferFields returns the schema fields of docs, like inferColumns, with
// Nullable set for a field missing from (or null in) some document, nested
// Fields for objects and objects inside arrays, and Generated for the
// system properties.
func inferFields(docs []driver.Object) []driver.Field { return inferFieldsDepth(docs, 0) }

func inferFieldsDepth(docs []driver.Object, depth int) []driver.Field {
	accs := accumulate(docs, depth < maxFieldDepth)
	out := make([]driver.Field, len(accs))
	for i, a := range accs {
		f := driver.Field{Name: a.name, Nullable: a.present < len(docs) || a.nulls > 0}
		f.Type, f.NativeType = a.typeAndNative()
		f.Generated = depth == 0 && isSystemProp(a.name)
		if len(a.nested) > 0 {
			f.Fields = inferFieldsDepth(a.nested, depth+1)
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

// rowOf lays an item out along the columns: an object's fields by name (a
// missing one is NULL), or the item itself in a lone value column.
// omitted counts the object's fields that have no column.
func rowOf(item driver.Value, index map[string]int, n int, valueMode bool) (row driver.Row, omitted int) {
	row = make(driver.Row, n)
	if valueMode {
		row[0] = item
		return row, 0
	}
	obj, ok := item.AsObject()
	if !ok {
		return row, 1
	}
	filled := make([]bool, n)
	for _, m := range obj {
		i, ok := index[m.Key]
		if !ok {
			omitted++
			continue
		}
		if !filled[i] {
			row[i] = m.Value
			filled[i] = true
		}
	}
	return row, omitted
}
