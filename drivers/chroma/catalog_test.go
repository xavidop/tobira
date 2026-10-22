package chroma

import (
	"errors"
	"slices"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestCatalogAgainstFake(t *testing.T) {
	f := newFake(t)
	f.dbs = []string{"default_database", "other"}
	coll := f.addColl("default_database", "people", "cosine", 2)
	coll.add("1", []float64{1, 0}, "doc one", map[string]any{"name": "a", "age": 3, "score": 1.5})
	coll.add("2", []float64{0, 1}, "", map[string]any{"name": "b", "active": true})
	f.addColl("other", "elsewhere", "l2", 0)
	c := openFake(t, f, driver.ConnConfig{})
	ctx := t.Context()

	nss, err := c.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nss) != 2 || nss[0].Name != "default_database" || !nss[0].Default || nss[1].Default || nss[0].Label != "database" {
		t.Errorf("namespaces = %+v", nss)
	}
	cols, err := c.Collections(ctx, driver.NamespaceRef{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0].Ref != (driver.CollectionRef{Namespace: "other", Name: "elsewhere"}) || cols[0].Type != driver.CollectionCollection {
		t.Errorf("collections(other) = %+v", cols)
	}
	cols, err = c.Collections(ctx, driver.NamespaceRef{})
	if err != nil || len(cols) != 1 || cols[0].Ref.Namespace != "default_database" {
		t.Errorf("collections(default) = %+v, %v", cols, err)
	}

	sch, err := c.Describe(ctx, driver.CollectionRef{Name: "people"})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]driver.Type{}
	native := map[string]string{}
	for _, fl := range sch.Fields {
		types[fl.Name], native[fl.Name] = fl.Type, fl.NativeType
	}
	for name, want := range map[string]driver.Type{
		"id": driver.TypeInt, "document": driver.TypeString, "embedding": driver.TypeVector,
		"name": driver.TypeString, "age": driver.TypeInt, "score": driver.TypeFloat, "active": driver.TypeBool,
	} {
		if types[name] != want {
			t.Errorf("field %s: %s, want %s", name, types[name], want)
		}
	}
	if native["embedding"] != "vector(2, cosine)" {
		t.Errorf("embedding native type = %q", native["embedding"])
	}
	if !slices.Equal(sch.PrimaryKey, []string{"id"}) || !sch.Schemaless || sch.RowEstimate != 2 || sch.Type != driver.CollectionCollection {
		t.Errorf("schema = %+v", sch)
	}
	if len(sch.Indexes) == 0 || sch.Indexes[0].Method != "hnsw cosine" {
		t.Errorf("indexes = %+v", sch.Indexes)
	}
	if sch.DDL == "" {
		t.Error("DDL (the configuration) is empty")
	}
	if _, err := c.Describe(ctx, driver.CollectionRef{Name: "missing"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Describe(missing): want ErrNotFound, got %v", err)
	}
}
