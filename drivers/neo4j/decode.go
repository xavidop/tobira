package neo4j

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"

	"github.com/xavidop/tobira/driver"
)

// decode turns a value the Go driver returns into a driver.Value. Nodes,
// relationships and paths become objects (id, labels or type, properties),
// points objects of their coordinates, durations their ISO 8601 text, and
// maps objects with their keys in order.
func decode(v any) driver.Value {
	switch x := v.(type) {
	case nil:
		return driver.Null()
	case bool:
		return driver.Bool(x)
	case int64:
		return driver.Int(x)
	case int:
		return driver.Int(int64(x))
	case float64:
		return driver.Float(x)
	case string:
		return driver.String(x)
	case []byte:
		return driver.Bytes(x)
	case time.Time:
		return driver.Time(x)
	case dbtype.Date:
		return driver.Time(x.Time())
	case dbtype.LocalDateTime:
		return driver.Time(x.Time())
	case dbtype.LocalTime:
		return driver.Time(x.Time())
	case dbtype.Time:
		return driver.Time(x.Time())
	case dbtype.Duration:
		return driver.String(x.String())
	case dbtype.Point2D:
		return driver.ObjectValue(
			driver.Member{Key: "srid", Value: driver.Int(int64(x.SpatialRefId))},
			driver.Member{Key: "x", Value: driver.Float(x.X)},
			driver.Member{Key: "y", Value: driver.Float(x.Y)})
	case dbtype.Point3D:
		return driver.ObjectValue(
			driver.Member{Key: "srid", Value: driver.Int(int64(x.SpatialRefId))},
			driver.Member{Key: "x", Value: driver.Float(x.X)},
			driver.Member{Key: "y", Value: driver.Float(x.Y)},
			driver.Member{Key: "z", Value: driver.Float(x.Z)})
	case dbtype.Node:
		return nodeValue(x)
	case dbtype.Relationship:
		return relationshipValue(x)
	case dbtype.Path:
		nodes := make([]driver.Value, len(x.Nodes))
		for i, n := range x.Nodes {
			nodes[i] = nodeValue(n)
		}
		rels := make([]driver.Value, len(x.Relationships))
		for i, r := range x.Relationships {
			rels[i] = relationshipValue(r)
		}
		return driver.ObjectValue(
			driver.Member{Key: "nodes", Value: driver.List(nodes...)},
			driver.Member{Key: "relationships", Value: driver.List(rels...)})
	case []any:
		out := make([]driver.Value, len(x))
		for i, e := range x {
			out[i] = decode(e)
		}
		return driver.List(out...)
	case map[string]any:
		return mapValue(x)
	case dbtype.InvalidValue:
		return driver.Unknown("invalid", nil, x.Message)
	}
	return driver.Unknown(fmt.Sprintf("%T", v), nil, fmt.Sprint(v))
}

func mapValue(m map[string]any) driver.Value {
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
	ms := make([]driver.Member, len(keys))
	for i, k := range keys {
		ms[i] = driver.Member{Key: k, Value: decode(m[k])}
	}
	return driver.ObjectValue(ms...)
}

func labelsValue(labels []string) driver.Value {
	out := make([]driver.Value, len(labels))
	for i, l := range labels {
		out[i] = driver.String(l)
	}
	return driver.List(out...)
}

func nodeValue(n dbtype.Node) driver.Value {
	return driver.ObjectValue(
		driver.Member{Key: "id", Value: driver.String(n.ElementId)},
		driver.Member{Key: "labels", Value: labelsValue(n.Labels)},
		driver.Member{Key: "properties", Value: mapValue(n.Props)})
}

func relationshipValue(r dbtype.Relationship) driver.Value {
	return driver.ObjectValue(
		driver.Member{Key: "id", Value: driver.String(r.ElementId)},
		driver.Member{Key: "type", Value: driver.String(r.Type)},
		driver.Member{Key: "start", Value: driver.String(r.StartElementId)},
		driver.Member{Key: "end", Value: driver.String(r.EndElementId)},
		driver.Member{Key: "properties", Value: mapValue(r.Props)})
}

// nativeTypeOf names the Cypher type of a value, for Column.NativeType.
func nativeTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return ""
	case bool:
		return "Boolean"
	case int64, int:
		return "Integer"
	case float64:
		return "Float"
	case string:
		return "String"
	case []byte:
		return "ByteArray"
	case time.Time:
		return "DateTime"
	case dbtype.Date:
		return "Date"
	case dbtype.LocalDateTime:
		return "LocalDateTime"
	case dbtype.LocalTime:
		return "LocalTime"
	case dbtype.Time:
		return "Time"
	case dbtype.Duration:
		return "Duration"
	case dbtype.Point2D, dbtype.Point3D:
		return "Point"
	case dbtype.Node:
		return "Node"
	case dbtype.Relationship:
		return "Relationship"
	case dbtype.Path:
		return "Path"
	case []any:
		return "List"
	case map[string]any:
		return "Map"
	}
	return fmt.Sprintf("%T", v)
}

// encode turns a driver.Value into what the Go driver sends as a parameter
// or a property. A Decimal goes as a float, Neo4j having no exact decimal;
// JSON is parsed into its lists and maps; a vector is a list of floats.
func encode(v driver.Value) (any, error) {
	switch v.Type() {
	case driver.TypeNull:
		return nil, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return b, nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		return n, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return f, nil
	case driver.TypeDecimal:
		s, _ := v.AsText()
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(f, 0) {
			return nil, fmt.Errorf("decimal %q does not fit a float", s)
		}
		return f, nil
	case driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return b, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t, nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		return out, nil
	case driver.TypeVector:
		fs, _ := v.AsVector()
		out := make([]any, len(fs))
		for i, f := range fs {
			out[i] = float64(f)
		}
		return out, nil
	case driver.TypeList:
		out := make([]any, 0, v.Len())
		for _, e := range v.Elements() {
			p, err := encode(e)
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, nil
	case driver.TypeObject:
		out := map[string]any{}
		for _, m := range v.Members() {
			p, err := encode(m.Value)
			if err != nil {
				return nil, err
			}
			out[m.Key] = p
		}
		return out, nil
	}
	native, _, _ := v.AsUnknown()
	return nil, fmt.Errorf("cannot send a %s value (%s)", v.Type(), native)
}

// typeOfNative maps a type name db.schema.nodeTypeProperties reports
// ("Long", "StringArray", …) to a driver.Type.
func typeOfNative(name string) driver.Type {
	switch name {
	case "Long", "Integer":
		return driver.TypeInt
	case "Double", "Float":
		return driver.TypeFloat
	case "String":
		return driver.TypeString
	case "Boolean":
		return driver.TypeBool
	case "ByteArray":
		return driver.TypeBytes
	case "Date", "DateTime", "LocalDateTime", "Time", "LocalTime":
		return driver.TypeTime
	case "Duration":
		return driver.TypeString
	case "Point":
		return driver.TypeObject
	case "":
		return driver.TypeUnknown
	}
	if len(name) > 5 && name[len(name)-5:] == "Array" {
		return driver.TypeList
	}
	return driver.TypeUnknown
}
