package chroma

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func obj(kv ...any) driver.Object {
	var o driver.Object
	for i := 0; i < len(kv); i += 2 {
		o = append(o, driver.Member{Key: kv[i].(string), Value: kv[i+1].(driver.Value)})
	}
	return o
}

func TestMutatorReadOnlyBeforeNetwork(t *testing.T) {
	c := newTestConn(t, true)
	ref := driver.CollectionRef{Name: "c"}
	key := obj("id", driver.Int(1))
	_, err := c.Insert(t.Context(), ref, obj("id", driver.Int(1), "embedding", driver.Vector([]float32{1})))
	errs := map[string]error{
		"Insert": err,
		"Update": c.Update(t.Context(), ref, key, obj("name", driver.String("x"))),
		"Delete": c.Delete(t.Context(), ref, key),
	}
	for name, err := range errs {
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s on a read-only conn: want ErrReadOnly, got %v", name, err)
		}
	}
}

func TestMutatorChecksBeforeNetwork(t *testing.T) {
	c := newTestConn(t, false)
	ref := driver.CollectionRef{Name: "c"}
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"insert without embedding": {func() error {
			_, err := c.Insert(t.Context(), ref, obj("id", driver.Int(1), "name", driver.String("x")))
			return err
		}(), driver.ErrSyntax},
		"insert bad embedding": {func() error { _, err := c.Insert(t.Context(), ref, obj("embedding", driver.String("x"))); return err }(), driver.ErrSyntax},
		"insert bad id": {func() error {
			_, err := c.Insert(t.Context(), ref, obj("id", driver.Float(1.5), "embedding", driver.Vector([]float32{1})))
			return err
		}(), driver.ErrSyntax},
		"insert bad metadata": {func() error {
			_, err := c.Insert(t.Context(), ref, obj("embedding", driver.Vector([]float32{1}), "x", driver.List()))
			return err
		}(), driver.ErrUnsupported},
		"update empty key":      {c.Update(t.Context(), ref, nil, obj("a", driver.Int(1))), driver.ErrUnsupported},
		"update other key":      {c.Update(t.Context(), ref, obj("name", driver.String("x")), obj("a", driver.Int(1))), driver.ErrUnsupported},
		"update extra key":      {c.Update(t.Context(), ref, obj("id", driver.Int(1), "name", driver.String("x")), obj("a", driver.Int(1))), driver.ErrUnsupported},
		"update bad id":         {c.Update(t.Context(), ref, obj("id", driver.Bool(true)), obj("a", driver.Int(1))), driver.ErrSyntax},
		"update null document":  {c.Update(t.Context(), ref, obj("id", driver.Int(1)), obj("document", driver.Null())), driver.ErrUnsupported},
		"update null embedding": {c.Update(t.Context(), ref, obj("id", driver.Int(1)), obj("embedding", driver.Null())), driver.ErrUnsupported},
		"update nothing":        {c.Update(t.Context(), ref, obj("id", driver.Int(1)), nil), driver.ErrSyntax},
		"delete bad key":        {c.Delete(t.Context(), ref, obj("id", driver.Null())), driver.ErrSyntax},
	} {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: want %v, got %v", name, tc.want, tc.err)
		}
	}
}

func TestSplitValues(t *testing.T) {
	w, err := splitValues(obj(
		"id", driver.Int(7), "embedding", driver.Vector([]float32{1, 2}), "document", driver.String("doc"),
		"uri", driver.String("s3://x"), "name", driver.String("n"), "meta_id", driver.String("shadow"),
		"age", driver.Null(), "score", driver.Decimal("1.50"),
	), false)
	if err != nil {
		t.Fatal(err)
	}
	if w.id == nil || *w.id != "7" || len(w.embedding) != 2 || w.document == nil || *w.document != "doc" || w.uri == nil || *w.uri != "s3://x" {
		t.Errorf("split = %+v", w)
	}
	b, _ := json.Marshal(w.metadata)
	// A NULL member is left out on insert; meta_id writes the key "id".
	if string(b) != `{"id":"shadow","name":"n","score":1.5}` {
		t.Errorf("metadata = %s", b)
	}
	w, err = splitValues(obj("age", driver.Null(), "name", driver.String("x")), true)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(w.metadata)
	// On update a NULL removes the key.
	if string(b) != `{"age":null,"name":"x"}` {
		t.Errorf("update metadata = %s", b)
	}
}

func TestMutatorAgainstFake(t *testing.T) {
	f := newFake(t)
	coll := f.addColl("default_database", "things", "l2", 2)
	coll.add("1", []float64{1, 0}, "one", map[string]any{"name": "a", "age": 3})
	c := openFake(t, f, driver.ConnConfig{})
	ref := driver.CollectionRef{Name: "things"}
	ctx := t.Context()

	// An existing id: Chroma's add would silently keep the old record.
	_, err := c.Insert(ctx, ref, obj("id", driver.Int(1), "embedding", driver.Vector([]float32{0, 1})))
	if !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("insert existing: want ErrConflict, got %v", err)
	}
	if n := f.writes(); n != 0 {
		t.Errorf("insert existing sent %d writes", n)
	}
	key, err := c.Insert(ctx, ref, obj("id", driver.Int(2), "embedding", driver.Vector([]float32{0, 1}), "name", driver.String("b")))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := key.Get("id"); v.Type() != driver.TypeInt {
		t.Errorf("insert key = %v, want an Int id", key)
	}
	key, err = c.Insert(ctx, ref, obj("embedding", driver.Vector([]float32{1, 1})))
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := key.Get("id")
	if s, _ := gen.AsText(); !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(s) {
		t.Errorf("generated id = %s, want a UUIDv4", gen)
	}

	// Update merges, NULL removes a key, a missing record is ErrNotFound.
	if err := c.Update(ctx, ref, obj("id", driver.Int(1)), obj("age", driver.Null(), "name", driver.String("z"))); err != nil {
		t.Fatal(err)
	}
	if m := coll.recs[0].meta; m["name"] != "z" || m["age"] != nil {
		t.Errorf("after update: metadata %v", m)
	}
	before := f.writes()
	if err := c.Update(ctx, ref, obj("id", driver.Int(99)), obj("name", driver.String("z"))); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("update missing: want ErrNotFound, got %v", err)
	}
	if err := c.Update(ctx, ref, obj("id", driver.Int(1)), obj("id", driver.Int(5))); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("update changing the id: want ErrUnsupported, got %v", err)
	}
	if err := c.Delete(ctx, ref, obj("id", driver.Int(99))); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("delete missing: want ErrNotFound, got %v", err)
	}
	if n := f.writes(); n != before {
		t.Errorf("refused writes reached the server: %d", n-before)
	}
	// Writing the id unchanged is allowed (the editor sends whole rows).
	if err := c.Update(ctx, ref, obj("id", driver.Int(1)), obj("id", driver.Int(1), "name", driver.String("y"))); err != nil {
		t.Errorf("update with the same id: %v", err)
	}
	if err := c.Delete(ctx, ref, obj("id", driver.Int(2))); err != nil {
		t.Fatal(err)
	}
	if len(coll.recs) != 2 {
		t.Errorf("after delete: %d records, want 2", len(coll.recs))
	}
	if _, err := c.Insert(ctx, driver.CollectionRef{Name: "nope"}, obj("embedding", driver.Vector([]float32{1, 1}))); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("insert into a missing collection: want ErrNotFound, got %v", err)
	}
	if err := c.Update(ctx, ref, obj("id", driver.String("1")), obj("x", driver.List())); err == nil || strings.Contains(err.Error(), "panic") {
		t.Errorf("empty list metadata: want a refusal, got %v", err)
	}
}
