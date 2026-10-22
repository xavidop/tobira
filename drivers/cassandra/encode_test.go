package cassandra

import (
	"errors"
	"testing"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func encodeDecode(t *testing.T, info gocql.TypeInfo, v driver.Value) (driver.Value, error) {
	t.Helper()
	b, err := gocql.Marshal(info, param{v})
	if err != nil {
		return driver.Value{}, err
	}
	return decodeValue(info, b)
}

func TestParamRoundTrip(t *testing.T) {
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	str := driver.String
	udt := gocql.UDTTypeInfo{Name: "address", Elements: []gocql.UDTField{{Name: "street", Type: ti(t, "text")}, {Name: "zip", Type: ti(t, "int")}}}
	for _, tc := range []struct {
		info     gocql.TypeInfo
		in, want driver.Value
	}{
		{ti(t, "int"), driver.Int(7), driver.Int(7)},
		{ti(t, "int"), driver.Decimal("7"), driver.Int(7)},
		{ti(t, "int"), str("7"), driver.Int(7)},
		{ti(t, "bigint"), driver.Int(-1), driver.Int(-1)},
		{ti(t, "smallint"), driver.Int(12), driver.Int(12)},
		{ti(t, "tinyint"), driver.Int(-3), driver.Int(-3)},
		{ti(t, "varint"), driver.Decimal("123456789012345678901234567890"), driver.Decimal("123456789012345678901234567890")},
		{ti(t, "varint"), driver.Int(5), driver.Decimal("5")},
		{ti(t, "decimal"), driver.Decimal("1.50"), driver.Decimal("1.50")},
		{ti(t, "decimal"), driver.Int(3), driver.Decimal("3")},
		{ti(t, "decimal"), driver.Float(2.5), driver.Decimal("2.5")},
		{ti(t, "float"), driver.Float(1.5), driver.Float(1.5)},
		{ti(t, "double"), driver.Int(2), driver.Float(2)},
		{ti(t, "boolean"), driver.Bool(true), driver.Bool(true)},
		{ti(t, "text"), str("x"), str("x")},
		{ti(t, "text"), driver.JSON([]byte(`{"a":1}`)), str(`{"a":1}`)},
		{ti(t, "timestamp"), driver.Time(ts), driver.Time(ts)},
		{ti(t, "timestamp"), str("2026-06-01T12:00:00Z"), driver.Time(ts)},
		{ti(t, "date"), driver.Time(ts), driver.Time(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))},
		{ti(t, "date"), str("2026-06-01"), driver.Time(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))},
		{ti(t, "time"), str("13:45:06.123"), str("13:45:06.123")},
		{ti(t, "uuid"), str("123e4567-e89b-12d3-a456-426614174000"), str("123e4567-e89b-12d3-a456-426614174000")},
		{ti(t, "inet"), str("10.0.0.1"), str("10.0.0.1")},
		{ti(t, "duration"), str("1y2mo2d3h4m5s6ms"), str("1y2mo2d3h4m5s6ms")},
		{ti(t, "blob"), driver.Bytes([]byte{9}), driver.Bytes([]byte{9})},
		{ti(t, "list<text>"), driver.List(str("a"), str("b")), driver.List(str("a"), str("b"))},
		{ti(t, "frozen<list<text>>"), driver.JSON([]byte(`[]`)), driver.List()},
		{ti(t, "frozen<list<text>>"), driver.JSON([]byte(`["admin","beta"]`)), driver.List(str("admin"), str("beta"))},
		// The server sorts a set; the client sends it as given.
		{ti(t, "set<int>"), driver.List(driver.Int(2), driver.Int(1)), driver.List(driver.Int(2), driver.Int(1))},
		{ti(t, "map<text, text>"), driver.ObjectValue(driver.Member{Key: "k", Value: str("v")}), driver.ObjectValue(driver.Member{Key: "k", Value: str("v")})},
		{ti(t, "map<int, text>"), driver.JSON([]byte(`{"7":"seven"}`)), driver.ObjectValue(driver.Member{Key: "7", Value: str("seven")})},
		{gocql.TupleTypeInfo{Elems: []gocql.TypeInfo{ti(t, "int"), ti(t, "text")}}, driver.List(driver.Int(1), driver.Null()), driver.List(driver.Int(1), driver.Null())},
		{udt, driver.ObjectValue(driver.Member{Key: "street", Value: str("Main")}), driver.ObjectValue(driver.Member{Key: "street", Value: str("Main")}, driver.Member{Key: "zip", Value: driver.Null()})},
		{gocql.VectorType{SubType: ti(t, "float"), Dimensions: 2}, driver.Vector([]float32{1, 2}), driver.Vector([]float32{1, 2})},
		{gocql.VectorType{SubType: ti(t, "float"), Dimensions: 2}, driver.List(driver.Float(1), driver.Int(2)), driver.Vector([]float32{1, 2})},
		{ti(t, "int"), driver.Null(), driver.Null()},
	} {
		got, err := encodeDecode(t, tc.info, tc.in)
		if err != nil {
			t.Errorf("%s <- %s: %v", nativeName(tc.info), tc.in, err)
			continue
		}
		if got.Type() != tc.want.Type() || !drivertest.EqualValue(tc.want, got) {
			t.Errorf("%s <- %s = %s (%s), want %s (%s)", nativeName(tc.info), tc.in, got, got.Type(), tc.want, tc.want.Type())
		}
	}
}

func TestParamRefuses(t *testing.T) {
	udt := gocql.UDTTypeInfo{Name: "address", Elements: []gocql.UDTField{{Name: "street", Type: ti(t, "text")}}}
	for _, tc := range []struct {
		info gocql.TypeInfo
		in   driver.Value
	}{
		{ti(t, "int"), driver.String("seven")},
		{ti(t, "int"), driver.Int(1 << 40)},
		{ti(t, "tinyint"), driver.Int(300)},
		{ti(t, "int"), driver.Float(1.5)},
		{ti(t, "boolean"), driver.String("yes")},
		{ti(t, "timestamp"), driver.String("yesterday")},
		{ti(t, "uuid"), driver.String("not-a-uuid")},
		{ti(t, "inet"), driver.String("999.1.1.1")},
		{ti(t, "list<int>"), driver.String("[1")},
		{ti(t, "list<int>"), driver.List(driver.String("x"))},
		{ti(t, "map<text, int>"), driver.List(driver.Int(1))},
		{udt, driver.ObjectValue(driver.Member{Key: "nope", Value: driver.String("x")})},
		{gocql.TupleTypeInfo{Elems: []gocql.TypeInfo{ti(t, "int")}}, driver.List(driver.Int(1), driver.Int(2))},
		{gocql.VectorType{SubType: ti(t, "float"), Dimensions: 2}, driver.Vector([]float32{1})},
	} {
		if _, err := gocql.Marshal(tc.info, param{tc.in}); err == nil {
			t.Errorf("%s <- %s: accepted, want an error", nativeName(tc.info), tc.in)
		} else if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%s <- %s: %v, want ErrSyntax", nativeName(tc.info), tc.in, err)
		}
	}
}
