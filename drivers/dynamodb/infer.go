package dynamodb

import (
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
)

// maxFieldDepth bounds how deep Describe infers nested map fields.
const maxFieldDepth = 8

// typeInfo accumulates the types one attribute holds across items.
type typeInfo struct {
	natives map[string]bool // DynamoDB types seen, NULL excluded
	allInt  bool            // every N seen is a whole number within int64
	seen    int             // items holding the attribute (NULL included)
	null    bool            // some item holds NULL
	maps    []map[string]types.AttributeValue
}

func (ti *typeInfo) add(av types.AttributeValue) {
	ti.seen++
	nt := nativeType(av)
	if nt == "NULL" || nt == "" {
		ti.null = true
		return
	}
	if ti.natives == nil {
		ti.natives = map[string]bool{}
		ti.allInt = true
	}
	ti.natives[nt] = true
	switch v := av.(type) {
	case *types.AttributeValueMemberN:
		if valueOf(v).Type() != driver.TypeInt {
			ti.allInt = false
		}
	case *types.AttributeValueMemberM:
		ti.maps = append(ti.maps, v.Value)
	}
}

// resolve returns the tobira type and the native type text: one DynamoDB
// type gives its tobira type (N as Int when every value is whole, else
// Decimal); several give Unknown, natively "N|S"; none gives Null.
func (ti *typeInfo) resolve() (driver.Type, string) {
	switch len(ti.natives) {
	case 0:
		if ti.null {
			return driver.TypeNull, "NULL"
		}
		return driver.TypeNull, ""
	case 1:
		for nt := range ti.natives {
			if nt == "N" && ti.allInt {
				return driver.TypeInt, nt
			}
			return typeOfNative(nt), nt
		}
	}
	names := sortedKeys(ti.natives)
	return driver.TypeUnknown, strings.Join(names, "|")
}

// collect gathers the typeInfo of every attribute of items.
func collect(items []map[string]types.AttributeValue) map[string]*typeInfo {
	infos := map[string]*typeInfo{}
	for _, it := range items {
		for k, v := range it {
			ti := infos[k]
			if ti == nil {
				ti = &typeInfo{}
				infos[k] = ti
			}
			ti.add(v)
		}
	}
	return infos
}

// inferColumns returns the columns of a result. With a projection (the
// plain names a SELECT lists) they are exactly its names, in its order.
// Otherwise the key attributes come first, in key order, then every other
// attribute the items hold, sorted by name.
func inferColumns(items []map[string]types.AttributeValue, keys, projection []string) []driver.Column {
	infos := collect(items)
	var names []string
	if projection != nil {
		for _, p := range projection {
			if !slices.Contains(names, p) {
				names = append(names, p)
			}
		}
	} else {
		names = append(names, keys...)
		for _, k := range sortedKeys(infos) {
			if !slices.Contains(keys, k) {
				names = append(names, k)
			}
		}
	}
	cols := make([]driver.Column, len(names))
	for i, n := range names {
		cols[i] = driver.Column{Name: n}
		if ti := infos[n]; ti != nil {
			cols[i].Type, cols[i].NativeType = ti.resolve()
		}
	}
	return cols
}

// inferFields describes a table from sampled items: the key attributes
// first (never nullable; typed from the table's declaration when the
// sample does not show them), then every other attribute sorted by name,
// nullable when some item lacks it or holds NULL. Maps get nested fields,
// merged across items, down to 8 levels.
func inferFields(items []map[string]types.AttributeValue, meta *tableMeta) []driver.Field {
	infos := collect(items)
	var fields []driver.Field
	for _, k := range meta.keys {
		f := driver.Field{Name: k, NativeType: meta.keyTypes[k], Type: typeOfNative(meta.keyTypes[k])}
		if ti := infos[k]; ti != nil && len(ti.natives) > 0 {
			f.Type, f.NativeType = ti.resolve()
		}
		fields = append(fields, f)
	}
	for _, k := range sortedKeys(infos) {
		if slices.Contains(meta.keys, k) {
			continue
		}
		fields = append(fields, fieldOf(k, infos[k], len(items), 1))
	}
	return fields
}

func fieldOf(name string, ti *typeInfo, total, depth int) driver.Field {
	f := driver.Field{Name: name, Nullable: ti.null || ti.seen < total}
	f.Type, f.NativeType = ti.resolve()
	if len(ti.maps) > 0 && depth < maxFieldDepth {
		sub := collect(ti.maps)
		for _, k := range sortedKeys(sub) {
			f.Fields = append(f.Fields, fieldOf(k, sub[k], len(ti.maps), depth+1))
		}
	}
	return f
}
