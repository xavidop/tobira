package cassandra

import (
	"testing"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"github.com/xavidop/tobira/driver"
)

// ti parses a CQL type name the way the server describes result columns.

func ti(t *testing.T, name string) gocql.TypeInfo {
	t.Helper()
	return gocql.NewNativeType(5, gocql.TypeCustom, name) //nolint:staticcheck // the only exported parser of type names
}

func mustMarshal(t *testing.T, info gocql.TypeInfo, v any) []byte {
	t.Helper()
	b, err := gocql.Marshal(info, v)
	if err != nil {
		t.Fatalf("Marshal(%v, %v): %v", info, v, err)
	}
	return b
}

func TestTypeOfNative(t *testing.T) {
	for native, want := range map[string]driver.Type{
		"int": driver.TypeInt, "bigint": driver.TypeInt, "smallint": driver.TypeInt, "tinyint": driver.TypeInt,
		"counter": driver.TypeInt, "varint": driver.TypeDecimal, "decimal": driver.TypeDecimal,
		"float": driver.TypeFloat, "double": driver.TypeFloat, "boolean": driver.TypeBool,
		"text": driver.TypeString, "ascii": driver.TypeString, "varchar": driver.TypeString,
		"uuid": driver.TypeString, "timeuuid": driver.TypeString, "inet": driver.TypeString,
		"duration": driver.TypeString, "time": driver.TypeString,
		"timestamp": driver.TypeTime, "date": driver.TypeTime, "blob": driver.TypeBytes,
		"list<int>": driver.TypeList, "set<text>": driver.TypeList, "frozen<list<text>>": driver.TypeList,
		"map<text, text>": driver.TypeObject, "frozen<map<text, frozen<list<int>>>>": driver.TypeObject,
		"tuple<int, text>": driver.TypeList, "frozen<tuple<int, text>>": driver.TypeList,
		"vector<float, 3>": driver.TypeVector, "vector<int, 3>": driver.TypeList,
		"address": driver.TypeObject, "frozen<address>": driver.TypeObject,
		"'org.apache.cassandra.db.marshal.DynamicCompositeType(s=>UTF8Type)'": driver.TypeUnknown,
		"": driver.TypeUnknown,
	} {
		if got := typeOfNative(native); got != want {
			t.Errorf("typeOfNative(%q) = %s, want %s", native, got, want)
		}
	}
}

func TestNativeName(t *testing.T) {
	udt := gocql.UDTTypeInfo{Keyspace: "ks", Name: "address", Elements: []gocql.UDTField{{Name: "street", Type: ti(t, "text")}}}
	for _, tc := range []struct {
		info gocql.TypeInfo
		want string
	}{
		{ti(t, "int"), "int"},
		{ti(t, "list<text>"), "list<text>"},
		{ti(t, "map<text, frozen<list<int>>>"), "map<text, list<int>>"},
		{gocql.TupleTypeInfo{Elems: []gocql.TypeInfo{ti(t, "int"), ti(t, "text")}}, "tuple<int, text>"},
		{udt, "address"},
		{gocql.VectorType{SubType: ti(t, "float"), Dimensions: 3}, "vector<float, 3>"},
	} {
		if got := nativeName(tc.info); got != tc.want {
			t.Errorf("nativeName = %q, want %q", got, tc.want)
		}
	}
}

// A UDT value written before a field was added ends early: the missing

// fields read as NULL.

// encodeDecode marshals v for info through param and decodes it back.
