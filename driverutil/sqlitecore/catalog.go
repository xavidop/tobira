package sqlitecore

import (
	"cmp"
	"slices"

	"github.com/xavidop/tobira/driver"
)

// The parsers below read catalog rows decoded to Go values (int64, float64,
// string, []byte or nil), as a driver without database/sql gets them from
// its wire protocol. Each tolerates short rows and wrong value types (a
// hostile server picks them) by skipping or zeroing, never panicking.

// AsString returns v as text: a string or []byte, else "".
func AsString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return ""
}

// AsInt returns v as an int64, else 0.
func AsInt(v any) int64 {
	if n, ok := v.(int64); ok {
		return n
	}
	return 0
}

// Collections reads "name, type" rows from sqlite_schema (type 'table' or
// 'view') as the collections of namespace ns.
func Collections(ns string, rows [][]any) []driver.Collection {
	var out []driver.Collection
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		ct := driver.CollectionTable
		if AsString(row[1]) == "view" {
			ct = driver.CollectionView
		}
		out = append(out, driver.Collection{Ref: driver.CollectionRef{Namespace: ns, Name: AsString(row[0])}, Type: ct, RowEstimate: -1})
	}
	return out
}

// ApplyTableXInfo fills s.Fields and s.PrimaryKey from table_xinfo rows
// (cid, name, type, notnull, dflt_value, pk, hidden), skipping hidden
// columns of virtual tables, then marks a rowid alias (see MarkRowidAlias).
// s.Type and s.DDL must already be set.
func ApplyTableXInfo(s *driver.CollectionSchema, rows [][]any) {
	type pkCol struct {
		pos  int64
		name string
	}
	var pks []pkCol
	for _, row := range rows {
		if len(row) < 7 {
			continue
		}
		name, typ := AsString(row[1]), AsString(row[2])
		notnull, pk, hidden := AsInt(row[3]), AsInt(row[5]), AsInt(row[6])
		if hidden == 1 {
			continue
		}
		s.Fields = append(s.Fields, driver.Field{
			Name: name, NativeType: typ, Type: MapType(typ), Nullable: notnull == 0,
			Default: AsString(row[4]), Generated: hidden == 2 || hidden == 3,
		})
		if pk > 0 {
			pks = append(pks, pkCol{pk, name})
		}
	}
	slices.SortFunc(pks, func(a, b pkCol) int { return cmp.Compare(a.pos, b.pos) })
	for _, p := range pks {
		s.PrimaryKey = append(s.PrimaryKey, p.name)
	}
	MarkRowidAlias(s)
}

// IndexList reads index_list rows (seq, name, unique, origin, partial) as
// indexes without their columns; IndexColumns reads each one's.
func IndexList(rows [][]any) []driver.Index {
	var idx []driver.Index
	for _, row := range rows {
		if len(row) < 4 {
			continue
		}
		idx = append(idx, driver.Index{Name: AsString(row[1]), Unique: AsInt(row[2]) != 0, Primary: AsString(row[3]) == "pk", Method: "btree"})
	}
	return idx
}

// IndexColumns reads index_info rows (seqno, cid, name) as the indexed
// column names; an expression column (a NULL name) is left out.
func IndexColumns(rows [][]any) []string {
	var out []string
	for _, col := range rows {
		if len(col) >= 3 && col[2] != nil {
			out = append(out, AsString(col[2]))
		}
	}
	return out
}

// ApplyForeignKeys fills s.ForeignKeys from foreign_key_list rows (id, seq,
// table, from, to, …), grouping a composite key's rows by id. The
// referenced table is in s's namespace; a NULL "to" (the referenced
// table's primary key) leaves RefFields short.
func ApplyForeignKeys(s *driver.CollectionSchema, rows [][]any) {
	byID := map[int64]int{} // fk id -> index in s.ForeignKeys
	for _, row := range rows {
		if len(row) < 5 {
			continue
		}
		id := AsInt(row[0])
		i, ok := byID[id]
		if !ok {
			i = len(s.ForeignKeys)
			byID[id] = i
			s.ForeignKeys = append(s.ForeignKeys, driver.ForeignKey{References: driver.CollectionRef{Namespace: s.Ref.Namespace, Name: AsString(row[2])}})
		}
		fk := &s.ForeignKeys[i]
		fk.Fields = append(fk.Fields, AsString(row[3]))
		if row[4] != nil {
			fk.RefFields = append(fk.RefFields, AsString(row[4]))
		}
	}
}
