package drivertest

import (
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// sampleSize caps the Sample a schemaless Describe returns.
const sampleSize = 100

// inferType returns the column type of rows[*][i]: the one type its
// non-NULL values share, or TypeUnknown with a native union such as
// "int|string" when they differ. A column of NULLs is TypeNull.
func inferType(rows []driver.Row, i int) (driver.Type, string) {
	vals := make([]driver.Value, len(rows))
	for k, r := range rows {
		vals[k] = r[i]
	}
	return inferValues(vals)
}

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
		return types[0], ""
	}
	names := make([]string, len(types))
	for k, t := range types {
		names[k] = t.String()
	}
	return driver.TypeUnknown, strings.Join(names, "|")
}

// nestedFields infers the fields of the Object values of rows[*][i].
func nestedFields(rows []driver.Row, i int) []driver.Field {
	vals := make([]driver.Value, len(rows))
	for k, r := range rows {
		vals[k] = r[i]
	}
	return objectFields(vals)
}

// objectFields returns the union of the members of the Object values in
// vals, in first-seen order, each typed by inferValues and nested in turn.
func objectFields(vals []driver.Value) []driver.Field {
	var names []string
	members := map[string][]driver.Value{}
	for _, v := range vals {
		obj, ok := v.AsObject()
		if !ok {
			continue
		}
		for _, m := range obj {
			if _, seen := members[m.Key]; !seen {
				names = append(names, m.Key)
			}
			members[m.Key] = append(members[m.Key], m.Value)
		}
	}
	var out []driver.Field
	for _, n := range names {
		typ, native := inferValues(members[n])
		if native == "" {
			native = typ.String()
		}
		out = append(out, driver.Field{Name: n, NativeType: native, Type: typ, Nullable: true, Fields: objectFields(members[n])})
	}
	return out
}
