package duckdb

import (
	"strings"

	"github.com/xavidop/tobira/driver"
)

// typeNode is a parsed DuckDB type name, as duckdb-go's
// ColumnTypeDatabaseTypeName and duckdb_columns().data_type spell it:
// INTEGER, DECIMAL(10,2), INTEGER[], INTEGER[3], STRUCT("a" INTEGER, …),
// MAP(K, V), UNION("tag" T, …). Struct field order matters: duckdb-go
// scans a STRUCT into a Go map, and the parsed type puts the keys back in
// declaration order.
type typeNode struct {
	kind   string // "scalar", "list", "struct", "map", "union"
	name   string // scalar: the upper-cased name with its arguments
	elem   *typeNode
	key    *typeNode
	value  *typeNode
	fields []typeField
}

type typeField struct {
	name string
	typ  *typeNode
}

// String renders the node compactly (for tests and debugging).
func (n *typeNode) String() string {
	switch n.kind {
	case "list":
		return "list<" + n.elem.String() + ">"
	case "map":
		return "map<" + n.key.String() + "," + n.value.String() + ">"
	case "struct", "union":
		parts := make([]string, len(n.fields))
		for i, f := range n.fields {
			parts[i] = f.name + ":" + f.typ.String()
		}
		return n.kind + "<" + strings.Join(parts, ",") + ">"
	}
	return n.name
}

// parseType parses a type name. It never fails: text it cannot read
// becomes a scalar of that name.
func parseType(s string) *typeNode {
	p := typeParser{s: s}
	n := p.parse()
	if p.i != len(p.s) {
		return &typeNode{kind: "scalar", name: strings.ToUpper(strings.TrimSpace(s))}
	}
	return n
}

type typeParser struct {
	s string
	i int
}

func (p *typeParser) space() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *typeParser) peek() byte {
	p.space()
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

// parse reads one type and its [] or [n] suffixes.
func (p *typeParser) parse() *typeNode {
	n := p.base()
	for p.peek() == '[' {
		end := strings.IndexByte(p.s[p.i:], ']')
		if end < 0 {
			return n
		}
		p.i += end + 1
		n = &typeNode{kind: "list", elem: n}
	}
	return n
}

func (p *typeParser) base() *typeNode {
	p.space()
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != '(' && p.s[p.i] != '[' && p.s[p.i] != ',' && p.s[p.i] != ')' {
		p.i++
	}
	word := strings.ToUpper(strings.TrimSpace(p.s[start:p.i]))
	if p.peek() != '(' {
		return &typeNode{kind: "scalar", name: word}
	}
	switch word {
	case "STRUCT", "UNION":
		p.i++ // (
		n := &typeNode{kind: strings.ToLower(word)}
		for p.peek() != ')' && p.i < len(p.s) {
			name := p.fieldName()
			n.fields = append(n.fields, typeField{name: name, typ: p.parse()})
			if p.peek() == ',' {
				p.i++
			}
		}
		if p.peek() == ')' {
			p.i++
		}
		return n
	case "MAP":
		p.i++
		n := &typeNode{kind: "map", key: p.parse()}
		if p.peek() == ',' {
			p.i++
		}
		n.value = p.parse()
		if p.peek() == ')' {
			p.i++
		}
		return n
	}
	// DECIMAL(10,2) and the like: keep the arguments in the name.
	end := strings.IndexByte(p.s[p.i:], ')')
	if end < 0 {
		p.i = len(p.s)
		return &typeNode{kind: "scalar", name: word}
	}
	args := strings.ReplaceAll(p.s[p.i:p.i+end+1], " ", "")
	p.i += end + 1
	return &typeNode{kind: "scalar", name: word + args}
}

// fieldName reads a struct or union member name: "quoted ""name""" or a
// bare word.
func (p *typeParser) fieldName() string {
	p.space()
	if p.i < len(p.s) && p.s[p.i] == '"' {
		var sb strings.Builder
		for j := p.i + 1; j < len(p.s); j++ {
			if p.s[j] == '"' {
				if j+1 < len(p.s) && p.s[j+1] == '"' {
					sb.WriteByte('"')
					j++
					continue
				}
				p.i = j + 1
				return sb.String()
			}
			sb.WriteByte(p.s[j])
		}
		p.i = len(p.s)
		return sb.String()
	}
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != ' ' {
		p.i++
	}
	return p.s[start:p.i]
}

// mapType maps a DuckDB type name to a driver.Type.
func mapType(name string) driver.Type { return nodeType(parseType(name)) }

func nodeType(n *typeNode) driver.Type {
	switch n.kind {
	case "list":
		return driver.TypeList
	case "struct", "map", "union":
		return driver.TypeObject
	}
	name := n.name
	if i := strings.IndexByte(name, '('); i >= 0 {
		name = name[:i]
	}
	switch name {
	case "BOOLEAN", "BOOL":
		return driver.TypeBool
	case "TINYINT", "SMALLINT", "INTEGER", "INT", "BIGINT", "UTINYINT", "USMALLINT", "UINTEGER":
		return driver.TypeInt
	case "UBIGINT", "HUGEINT", "UHUGEINT", "BIGNUM", "VARINT", "DECIMAL", "NUMERIC":
		return driver.TypeDecimal
	case "FLOAT", "REAL", "DOUBLE":
		return driver.TypeFloat
	case "VARCHAR", "TEXT", "STRING", "ENUM", "BIT", "BITSTRING", "UUID", "INTERVAL":
		return driver.TypeString
	case "BLOB", "BYTEA", "GEOMETRY":
		return driver.TypeBytes
	case "JSON":
		return driver.TypeJSON
	case "DATE", "TIME", "TIMETZ", "TIME WITH TIME ZONE", "TIMESTAMP", "TIMESTAMPTZ", "TIMESTAMP WITH TIME ZONE",
		"TIMESTAMP_NS", "TIMESTAMP_MS", "TIMESTAMP_S", "DATETIME", "TIME_NS":
		return driver.TypeTime
	case "NULL", `"NULL"`:
		return driver.TypeNull
	}
	return driver.TypeUnknown
}
