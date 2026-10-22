package chroma

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// decodeMeta decodes one metadata object the way the client does.
func decodeMeta(t *testing.T, s string) map[string]any {
	t.Helper()
	m, err := decodeMetadata(json.RawMessage(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMetaValue(t *testing.T) {
	m := decodeMeta(t, `{"i":5,"neg":-3,"f":2.0,"g":12.5,"e":1e3,"b":true,"s":"hé","l":["a","b"],"li":[1,2],"lf":[1.5,2],"lb":[true],
		"big":9223372036854775808,"sparse":{"indices":[1,4],"values":[0.5,0.25]}}`)
	for k, want := range map[string]driver.Value{
		"i":   driver.Int(5),
		"neg": driver.Int(-3),
		"f":   driver.Float(2),
		"g":   driver.Float(12.5),
		"e":   driver.Float(1000),
		"b":   driver.Bool(true),
		"s":   driver.String("hé"),
		"l":   driver.List(driver.String("a"), driver.String("b")),
		"li":  driver.List(driver.Int(1), driver.Int(2)),
		"lf":  driver.List(driver.Float(1.5), driver.Int(2)),
		"lb":  driver.List(driver.Bool(true)),
		"big": driver.Float(9223372036854775808),
		"sparse": driver.ObjectValue(
			driver.Member{Key: "indices", Value: driver.List(driver.Int(1), driver.Int(4))},
			driver.Member{Key: "values", Value: driver.List(driver.Float(0.5), driver.Float(0.25))},
		),
	} {
		got := metaValue(m[k])
		if !drivertest.EqualValue(want, got) {
			t.Errorf("%s: got %s (%s), want %s (%s)", k, got, got.Type(), want, want.Type())
		}
	}
	if !metaValue(nil).IsNull() {
		t.Error("nil should be NULL")
	}
}

func TestIDValue(t *testing.T) {
	for in, want := range map[string]driver.Value{
		"1":                     driver.Int(1),
		"0":                     driver.Int(0),
		"-7":                    driver.Int(-7),
		"9223372036854775807":   driver.Int(math.MaxInt64),
		"9223372036854775808":   driver.String("9223372036854775808"),
		"01":                    driver.String("01"),
		"-0":                    driver.String("-0"),
		"+1":                    driver.String("+1"),
		"1.0":                   driver.String("1.0"),
		"abc":                   driver.String("abc"),
		"":                      driver.String(""),
		"5c56c793-69f3-4fbf-87": driver.String("5c56c793-69f3-4fbf-87"),
	} {
		if got := idValue(in); !drivertest.EqualValue(want, got) {
			t.Errorf("idValue(%q) = %s (%s), want %s (%s)", in, got, got.Type(), want, want.Type())
		}
	}
}

func TestIDText(t *testing.T) {
	for _, tt := range []struct {
		v    driver.Value
		want string
	}{
		{driver.Int(7), "7"},
		{driver.Int(-2), "-2"},
		{driver.String("abc"), "abc"},
		{driver.String("007"), "007"},
	} {
		got, err := idText(tt.v)
		if err != nil || got != tt.want {
			t.Errorf("idText(%s) = %q, %v; want %q", tt.v, got, err, tt.want)
		}
	}
	for _, v := range []driver.Value{driver.Null(), driver.String(""), driver.Float(1.5), driver.Bool(true), driver.Vector([]float32{1})} {
		if _, err := idText(v); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("idText(%s): want ErrSyntax, got %v", v, err)
		}
	}
}

func TestEncodeMeta(t *testing.T) {
	at := time.Date(2026, 6, 1, 12, 0, 0, 5, time.FixedZone("x", 3600))
	for _, tt := range []struct {
		v    driver.Value
		want string // JSON
	}{
		{driver.Int(5), `5`},
		{driver.Float(2), `2.0`},
		{driver.Float(12.5), `12.5`},
		{driver.Bool(false), `false`},
		{driver.String("hé"), `"hé"`},
		{driver.Decimal("1.50"), `1.5`},
		{driver.Decimal("3"), `3.0`},
		{driver.Time(at), `"2026-06-01T11:00:00.000000005Z"`},
		{driver.JSON([]byte(` [ "a", 1 ] `)), `"[\"a\",1]"`},
		{driver.JSON([]byte(`[]`)), `"[]"`},
		{driver.List(driver.String("a"), driver.String("b")), `["a","b"]`},
		{driver.List(driver.Int(1), driver.Int(2)), `[1,2]`},
		{driver.List(driver.Float(1.5), driver.Int(2)), `[1.5,2.0]`},
		{driver.List(driver.Bool(true)), `[true]`},
	} {
		got, err := encodeMeta(tt.v)
		if err != nil {
			t.Errorf("encodeMeta(%s): %v", tt.v, err)
			continue
		}
		b, _ := json.Marshal(got)
		if string(b) != tt.want {
			t.Errorf("encodeMeta(%s) = %s, want %s", tt.v, b, tt.want)
		}
	}
	for _, v := range []driver.Value{
		driver.Decimal("0.1000000000000000000001"),
		driver.Float(math.NaN()),
		driver.Float(math.Inf(1)),
		driver.List(),
		driver.List(driver.String("a"), driver.Int(1)),
		driver.List(driver.List(driver.Int(1))),
		driver.List(driver.Null()),
		driver.ObjectValue(driver.Member{Key: "a", Value: driver.Int(1)}),
		driver.Bytes([]byte("x")),
		driver.Vector([]float32{1}),
		driver.JSON([]byte(`{bad`)),
	} {
		if _, err := encodeMeta(v); !errors.Is(err, driver.ErrUnsupported) && !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("encodeMeta(%s): want ErrUnsupported or ErrSyntax, got %v", v, err)
		}
	}
}

func TestEmbeddingOf(t *testing.T) {
	for _, v := range []driver.Value{
		driver.Vector([]float32{1, 0.5}),
		driver.List(driver.Int(1), driver.Float(0.5)),
		driver.JSON([]byte(`[1, 0.5]`)),
		driver.String("[1,0.5]"),
	} {
		got, err := embeddingOf(v)
		if err != nil || len(got) != 2 || got[0] != 1 || got[1] != 0.5 {
			t.Errorf("embeddingOf(%s) = %v, %v", v, got, err)
		}
	}
	for _, v := range []driver.Value{
		driver.Null(), driver.Vector(nil), driver.List(), driver.List(driver.String("a")),
		driver.Float(1), driver.Vector([]float32{float32(math.NaN())}), driver.String("nope"),
	} {
		if _, err := embeddingOf(v); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("embeddingOf(%s): want ErrSyntax, got %v", v, err)
		}
	}
}

func TestDisplayNames(t *testing.T) {
	for key, col := range map[string]string{
		"name":      "name",
		"id":        "meta_id",
		"document":  "meta_document",
		"embedding": "meta_embedding",
		"uri":       "meta_uri",
		"_distance": "meta__distance",
		"_query":    "meta__query",
		"meta_x":    "meta_meta_x",
		"meta_id":   "meta_meta_id",
		"Document":  "Document",
	} {
		if got := displayName(key); got != col {
			t.Errorf("displayName(%q) = %q, want %q", key, got, col)
		}
		if got := metaKey(col); got != key {
			t.Errorf("metaKey(%q) = %q, want %q", col, got, key)
		}
	}
}

func TestShapeColumns(t *testing.T) {
	doc := "hello"
	recs := []record{
		{id: "1", document: &doc, embedding: []float32{1, 0}, metadata: decodeMeta(t, `{"n":1,"x":1,"s":"a","id":"shadow"}`)},
		{id: "2", embedding: []float32{0, 1}, metadata: decodeMeta(t, `{"n":2.5,"x":"mixed","flag":true}`)},
	}
	ref := driver.CollectionRef{Namespace: "db", Name: "c"}
	sh := newShape(recs, shapeOpts{ref: &ref, documents: true, embeddings: true, metadatas: true})
	cols := sh.columns()
	var names []string
	types := map[string]driver.Type{}
	for _, c := range cols {
		names = append(names, c.Name)
		types[c.Name] = c.Type
	}
	if got := strings.Join(names, ","); got != "id,document,embedding,flag,meta_id,n,s,x" {
		t.Errorf("columns = %s", got)
	}
	for name, want := range map[string]driver.Type{
		"id": driver.TypeInt, "document": driver.TypeString, "embedding": driver.TypeVector,
		"flag": driver.TypeBool, "meta_id": driver.TypeString, "n": driver.TypeFloat, "s": driver.TypeString, "x": driver.TypeUnknown,
	} {
		if types[name] != want {
			t.Errorf("column %s: type %s, want %s", name, types[name], want)
		}
	}
	if !cols[0].Key || cols[0].Source == nil || *cols[0].Source != ref {
		t.Errorf("id column = %+v, want the key with a source", cols[0])
	}
	row := sh.row(recs[0])
	if n, ok := row[5].AsFloat(); !ok || n != 1 || row[5].Type() != driver.TypeFloat {
		t.Errorf("n in an Int+Float column = %s (%s), want Float 1", row[5], row[5].Type())
	}
	if !row[3].IsNull() {
		t.Errorf("missing key flag = %s, want NULL", row[3])
	}
	if w := strings.Join(sh.warnings(), " "); !strings.Contains(w, `"id"`) || !strings.Contains(w, "meta_id") {
		t.Errorf("warnings = %q, want the renamed key", sh.warnings())
	}
	// Any id that is not a canonical integer makes the column text.
	sh = newShape(append(recs, record{id: "abc"}), shapeOpts{})
	if c := sh.columns()[0]; c.Type != driver.TypeString {
		t.Errorf("mixed ids: id type %s, want String", c.Type)
	}
	if v := sh.row(recs[0])[0]; v.Type() != driver.TypeString {
		t.Errorf("mixed ids: id value %s (%s), want String", v, v.Type())
	}
}

func TestShapeLateKeys(t *testing.T) {
	sh := newShape([]record{{id: "1", metadata: decodeMeta(t, `{"a":1}`)}}, shapeOpts{metadatas: true})
	row := sh.row(record{id: "2", metadata: decodeMeta(t, `{"a":2,"late":true}`)})
	if len(row) != len(sh.columns()) {
		t.Fatalf("row has %d values for %d columns", len(row), len(sh.columns()))
	}
	if w := strings.Join(sh.warnings(), " "); !strings.Contains(w, "late") {
		t.Errorf("warnings = %q, want the key first seen later", sh.warnings())
	}
	// A non-integer id on a later page of an Int column is shown as text.
	if v := sh.row(record{id: "x"})[0]; v.Type() != driver.TypeString {
		t.Errorf("late text id = %s (%s)", v, v.Type())
	}
}
