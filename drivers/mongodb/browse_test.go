package mongodb

import (
	"errors"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

func extJSON(t *testing.T, d bson.D) string {
	t.Helper()
	b, err := bson.MarshalExtJSON(d, false, false)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBuildFilter(t *testing.T) {
	cond := func(f string, op driver.Op, v driver.Value) driver.Condition {
		return driver.Condition{Field: f, Op: op, Value: v}
	}
	for _, tc := range []struct {
		name string
		f    driver.Filter
		want string
	}{
		{"empty", driver.Filter{}, `{}`},
		{"eq", driver.Filter{Conditions: []driver.Condition{cond("active", driver.OpEq, driver.Bool(true))}}, `{"active":true}`},
		{"eq null", driver.Filter{Conditions: []driver.Condition{cond("a", driver.OpEq, driver.Null())}}, `{"a":null}`},
		{"ne gt le", driver.Filter{Conditions: []driver.Condition{cond("a", driver.OpNe, driver.Int(1)), cond("b.c", driver.OpGt, driver.Float(2.5)), cond("d", driver.OpLe, driver.Int(3))}},
			`{"$and":[{"a":{"$ne":1}},{"b.c":{"$gt":2.5}},{"d":{"$lte":3}}]}`},
		{"in", driver.Filter{Conditions: []driver.Condition{cond("a", driver.OpIn, driver.List(driver.Int(1), driver.String("x")))}}, `{"a":{"$in":[1,"x"]}}`},
		{"is null", driver.Filter{Conditions: []driver.Condition{{Field: "a", Op: driver.OpIsNull}}}, `{"a":null}`},
		{"not null", driver.Filter{Conditions: []driver.Condition{{Field: "a", Op: driver.OpNotNull}}}, `{"a":{"$ne":null}}`},
		{"contains escapes regex", driver.Filter{Conditions: []driver.Condition{cond("id", driver.OpContains, driver.String("a.b"))}},
			`{"$expr":{"$regexMatch":{"input":{"$convert":{"input":"$id","to":"string","onError":null,"onNull":null}},"regex":"a\\.b"}}}`},
		{"prefix", driver.Filter{Conditions: []driver.Condition{cond("name", driver.OpPrefix, driver.String("Ad"))}},
			`{"$expr":{"$regexMatch":{"input":{"$convert":{"input":"$name","to":"string","onError":null,"onNull":null}},"regex":"^Ad"}}}`},
		{"objectId hex matches both", driver.Filter{Conditions: []driver.Condition{cond("_id", driver.OpEq, driver.String("65a1b2c3d4e5f60718293a4b"))}},
			`{"_id":{"$in":[{"$oid":"65a1b2c3d4e5f60718293a4b"},"65a1b2c3d4e5f60718293a4b"]}}`},
		{"raw anded", driver.Filter{Conditions: []driver.Condition{cond("a", driver.OpEq, driver.Int(1))}, Raw: `{"b":{"$exists":true}}`},
			`{"$and":[{"a":1},{"b":{"$exists":true}}]}`},
		{"raw only", driver.Filter{Raw: `{"b":2}`}, `{"b":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildFilter(tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if s := extJSON(t, got); s != tc.want {
				t.Errorf("got  %s\nwant %s", s, tc.want)
			}
		})
	}
	for name, f := range map[string]driver.Filter{
		"operator field": {Conditions: []driver.Condition{cond("$where", driver.OpEq, driver.String("sleep(1)"))}},
		"empty field":    {Conditions: []driver.Condition{cond("", driver.OpEq, driver.Int(1))}},
		"bad raw":        {Raw: `{"a":`},
		"raw not object": {Raw: `[1]`},
		"in not a list":  {Conditions: []driver.Condition{cond("a", driver.OpIn, driver.Int(1))}},
	} {
		if _, err := buildFilter(f); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%s: want ErrSyntax, got %v", name, err)
		}
	}
	if _, err := buildFilter(driver.Filter{Conditions: []driver.Condition{cond("a", driver.Op("near"), driver.Int(1))}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("unknown op: want ErrUnsupported, got %v", err)
	}
}

func TestRawFilterGuard(t *testing.T) {
	for _, raw := range []string{`{"$out":"x"}`, `{"a":{"$merge":1}}`} {
		if err := checkFilterReadOnly(raw); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("checkFilterReadOnly(%s): want ErrReadOnly, got %v", raw, err)
		}
	}
	if err := checkFilterReadOnly(`{"a":{"$gt":1}}`); err != nil {
		t.Errorf("plain filter refused: %v", err)
	}
}

func TestBrowseCursor(t *testing.T) {
	oid := bson.NewObjectID()
	for _, id := range []any{int32(5), "d1", oid, bson.D{{Key: "a", Value: 1}}} {
		enc, err := keysetCursor(rawField(t, id))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(enc, "k:") {
			t.Errorf("keyset cursor %q", enc)
		}
		got, err := decodeCursor(enc)
		if err != nil || got.after == nil || !got.after.Equal(rawField(t, id)) {
			t.Errorf("round trip of %v: %+v %v", id, got, err)
		}
	}
	got, err := decodeCursor(offsetCursor(40))
	if err != nil || got.offset != 40 || got.after != nil {
		t.Errorf("offset cursor: %+v %v", got, err)
	}
	if got, err := decodeCursor(""); err != nil || got.after != nil || got.offset != 0 {
		t.Errorf("empty cursor: %+v %v", got, err)
	}
	// A large composite _id still makes a usable cursor.
	big := bson.D{{Key: "tenant", Value: strings.Repeat("t", 40000)}, {Key: "n", Value: int32(1)}}
	enc, err := keysetCursor(rawField(t, big))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeCursor(enc); err != nil || !got.after.Equal(rawField(t, big)) {
		t.Errorf("large composite _id cursor (%d bytes): %v", len(enc), err)
	}
	for _, bad := range []string{"x", "k:!!", "o:-1", "o:x", "k:" + strings.Repeat("A", maxCursorLen+1)} {
		if _, err := decodeCursor(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decodeCursor(%q): want ErrSyntax, got %v", bad, err)
		}
	}
}
