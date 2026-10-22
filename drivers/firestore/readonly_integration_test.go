//go:build integration

package firestore

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// A read-only connection refuses every request outside the allow-list and
// every Mutator call, before anything is sent; reads still work.
func TestReadOnlyConnection(t *testing.T) {
	c := openTest(t, testProject, true)
	m, _ := driver.As[driver.Mutator](c)
	ref := driver.CollectionRef{Name: "users"}
	key := driver.Object{{Key: "_id", Value: driver.String("ada")}}
	for name, err := range map[string]error{
		"insert": func() error {
			_, err := m.Insert(ctxT(t), ref, driver.Object{{Key: "name", Value: driver.String("x")}})
			return err
		}(),
		"update": m.Update(ctxT(t), ref, key, driver.Object{{Key: "name", Value: driver.String("x")}}),
		"delete": m.Delete(ctxT(t), ref, key),
	} {
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("%s on a read-only connection: %v", name, err)
		}
	}
	for _, text := range []string{
		`{"delete":"users/ada"}`,
		`{"collection":"users","set":{"name":"x"}}`,
		`{"collection":"users"};{"delete":"users/ada"}`,
		`not json`,
	} {
		if _, err := c.Query(ctxT(t), driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Query(%s) on a read-only connection: %v", text, err)
		}
	}
	_, rows, _ := collect(t, c, driver.Query{Text: `{"document":"users/ada"}`})
	if len(rows) != 1 {
		t.Errorf("read on a read-only connection: %d rows", len(rows))
	}
	a := admin(ctxT(t), t, testProject)
	snap, err := a.Doc("users/ada").Get(ctxT(t))
	if err != nil || snap.Data()["name"] != "ada" {
		t.Errorf("users/ada changed: %v %v", snap.Data(), err)
	}
}
