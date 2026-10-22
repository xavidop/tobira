//go:build integration

package cosmos

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// A read-only connection refuses every write path before any request, and
// the guard refuses what Cosmos SQL could use to run code or write.
func TestReadOnlyConnection(t *testing.T) {
	ro := openTest(t, testDB, true)
	ctx := ctx30(t)
	ref := driver.CollectionRef{Name: ordersName}
	key := driver.Object{{Key: "tenant", Value: driver.String("acme")}, {Key: "id", Value: driver.String("o1")}}
	if _, err := ro.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.String("x")}, {Key: "tenant", Value: driver.String("acme")}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Insert: want ErrReadOnly, got %v", err)
	}
	if err := ro.Update(ctx, ref, key, driver.Object{{Key: "total", Value: driver.Int(0)}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Update: want ErrReadOnly, got %v", err)
	}
	if err := ro.Delete(ctx, ref, key); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Delete: want ErrReadOnly, got %v", err)
	}
	for _, text := range []string{
		"DELETE FROM orders o",
		"SELECT udf.total(o) FROM orders o",
		"SELECT * FROM orders o WHERE udf.big(o)",
		`{"container":"orders","query":"SELECT * FROM c","sproc":"bulkDelete"}`,
		"SELECT * FROM orders o; SELECT * FROM orders o",
	} {
		if _, err := ro.Query(ctx, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Query(%q): want ErrReadOnly, got %v", text, err)
		}
	}
	if _, err := ro.Browse(ctx, ref, driver.BrowseOptions{Filter: driver.Filter{Raw: "udf.big(c)"}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Browse with a UDF raw filter: want ErrReadOnly, got %v", err)
	}
	if _, err := ro.Browse(ctx, ref, driver.BrowseOptions{Filter: driver.Filter{Raw: "true) OR (true"}}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("Browse with an unbalanced raw filter: want ErrReadOnly, got %v", err)
	}
	_, rows, _ := runRows(t, ro, driver.Query{Text: "SELECT * FROM orders o"})
	if len(rows) != 3 {
		t.Errorf("orders after the refusals: %d items, want 3", len(rows))
	}
}
