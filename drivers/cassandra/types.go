package cassandra

import (
	"fmt"
	"strings"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"github.com/xavidop/tobira/driver"
)

// Values map to driver types as follows:
//
//   - tinyint, smallint, int, bigint and counter: Int;
//   - varint and decimal: Decimal (exact, as text);
//   - float and double: Float; boolean: Bool; blob: Bytes;
//   - text, ascii and varchar: String;
//   - uuid, timeuuid, inet, duration (in CQL's 1h30m form) and time
//     (15:04:05.999999999): String;
//   - timestamp (milliseconds) and date: Time in UTC;
//   - list, set and tuple: List; map and user-defined types: Object (map
//     keys as text; a UDT's fields in declaration order);
//   - vector<float, n>: Vector; a vector of another type: List;
//   - anything else (custom types): Unknown, with the raw bytes.
//
// A collection of more than maxCollectionElems elements, or a value nested
// deeper than maxDepth, is shown as an Unknown placeholder rather than
// read into memory element by element.

// maxCollectionElems caps the elements of one collection value (a variable
// so tests can lower it).
var maxCollectionElems = 10000

// maxDepth caps the nesting of collections, tuples and UDTs.
const maxDepth = 32

// typeOfNative maps a CQL type as system_schema spells it ("int",
// "frozen<map<text, text>>", "vector<float, 3>", a UDT name) to a driver
// type.
func typeOfNative(native string) driver.Type {
	s := strings.TrimSpace(native)
	for len(s) > len("frozen<") && strings.EqualFold(s[:len("frozen<")], "frozen<") {
		s = strings.TrimSpace(strings.TrimSuffix(s[len("frozen<"):], ">"))
	}
	if s == "" || strings.HasPrefix(s, "'") {
		return driver.TypeUnknown
	}
	name, params, _ := strings.Cut(s, "<")
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tinyint", "smallint", "int", "bigint", "counter":
		return driver.TypeInt
	case "varint", "decimal":
		return driver.TypeDecimal
	case "float", "double":
		return driver.TypeFloat
	case "boolean":
		return driver.TypeBool
	case "text", "ascii", "varchar", "uuid", "timeuuid", "inet", "duration", "time":
		return driver.TypeString
	case "timestamp", "date":
		return driver.TypeTime
	case "blob":
		return driver.TypeBytes
	case "list", "set", "tuple":
		return driver.TypeList
	case "map":
		return driver.TypeObject
	case "vector":
		sub, _, _ := strings.Cut(params, ",")
		if strings.EqualFold(strings.TrimSpace(sub), "float") {
			return driver.TypeVector
		}
		return driver.TypeList
	}
	if params == "" && isUDTName(s) {
		return driver.TypeObject
	}
	return driver.TypeUnknown
}

// isUDTName reports whether s reads as a (possibly qualified or quoted)
// type name rather than a native type.
func isUDTName(s string) bool {
	for _, part := range strings.Split(s, ".") {
		if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
			continue
		}
		if part == "" || !isLetter(part[0]) {
			return false
		}
		for i := 1; i < len(part); i++ {
			if c := part[i]; !isLetter(c) && !isDigit(c) && c != '_' {
				return false
			}
		}
	}
	return true
}

// nativeNames names the simple CQL types.
var nativeNames = map[gocql.Type]string{
	gocql.TypeAscii: "ascii", gocql.TypeBigInt: "bigint", gocql.TypeBlob: "blob", gocql.TypeBoolean: "boolean",
	gocql.TypeCounter: "counter", gocql.TypeDecimal: "decimal", gocql.TypeDouble: "double", gocql.TypeFloat: "float",
	gocql.TypeInt: "int", gocql.TypeText: "text", gocql.TypeTimestamp: "timestamp", gocql.TypeUUID: "uuid",
	gocql.TypeVarchar: "varchar", gocql.TypeVarint: "varint", gocql.TypeTimeUUID: "timeuuid", gocql.TypeInet: "inet",
	gocql.TypeDate: "date", gocql.TypeTime: "time", gocql.TypeSmallInt: "smallint", gocql.TypeTinyInt: "tinyint",
	gocql.TypeDuration: "duration",
}

// nativeName spells a result column's type in CQL ("map<text, int>").
// frozen<> is not reported by the protocol, so it is never shown.
func nativeName(info gocql.TypeInfo) string {
	switch v := info.(type) {
	case gocql.CollectionType:
		switch v.Type() {
		case gocql.TypeMap:
			return "map<" + nativeName(v.Key) + ", " + nativeName(v.Elem) + ">"
		case gocql.TypeSet:
			return "set<" + nativeName(v.Elem) + ">"
		}
		return "list<" + nativeName(v.Elem) + ">"
	case gocql.TupleTypeInfo:
		parts := make([]string, len(v.Elems))
		for i, e := range v.Elems {
			parts[i] = nativeName(e)
		}
		return "tuple<" + strings.Join(parts, ", ") + ">"
	case gocql.UDTTypeInfo:
		return v.Name
	case gocql.VectorType:
		return fmt.Sprintf("vector<%s, %d>", nativeName(v.SubType), v.Dimensions)
	}
	if n, ok := nativeNames[info.Type()]; ok {
		return n
	}
	if s, ok := info.(fmt.Stringer); ok {
		return s.String()
	}
	return "custom"
}

// columnType is the driver type of a result column.
func columnType(info gocql.TypeInfo) driver.Type {
	switch v := info.(type) {
	case gocql.UDTTypeInfo:
		return driver.TypeObject
	case gocql.VectorType:
		if v.SubType.Type() == gocql.TypeFloat {
			return driver.TypeVector
		}
		return driver.TypeList
	}
	if _, ok := nativeNames[info.Type()]; !ok && info.Type() != gocql.TypeList && info.Type() != gocql.TypeSet &&
		info.Type() != gocql.TypeMap && info.Type() != gocql.TypeTuple {
		return driver.TypeUnknown
	}
	return typeOfNative(nativeName(info))
}
