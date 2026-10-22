//go:build integration

package postgres

import (
	"fmt"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// TestMutatorArraysFromEditedJSON writes array cells the way the TUI's cell
// editor parses edited JSON: whole numbers are Int, every other number is a
// Decimal holding its exact text.
func TestMutatorArraysFromEditedJSON(t *testing.T) {
	c := openPG(t, false)
	name := fmt.Sprintf("tobira_arrays_%d", scratchSeq.Add(1))
	mustExec(t, c, "CREATE TABLE "+name+" (id serial PRIMARY KEY, f float8[], n int[], m numeric[], r real[])")
	mustExec(t, c, "INSERT INTO "+name+" (f, n, m, r) VALUES ('{1.5}', '{1}', '{1.0}', '{0.5}')")
	t.Cleanup(func() { mustExec(t, c, "DROP TABLE IF EXISTS "+name) })
	ref := driver.CollectionRef{Namespace: "public", Name: name}
	d := driver.Decimal
	changes := driver.Object{
		{Key: "f", Value: driver.List(driver.Int(1), d("2.5"), d("1e3"))},
		{Key: "n", Value: driver.List(driver.Int(7), driver.Int(-3))},
		{Key: "m", Value: driver.List(d("1.10"), driver.Int(2), d("123456789012345678901234.5"))},
		{Key: "r", Value: driver.List(d("0.25"))},
	}
	if err := c.Update(ctxT(t), ref, idKey(1), changes); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := c.Insert(ctxT(t), ref, changes); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	rows, _, _ := collect(t, c, driver.Query{Text: "SELECT f::text, n::text, m::text, r::text FROM " + name + " ORDER BY id"})
	want := []string{"{1,2.5,1000}", "{7,-3}", "{1.10,2,123456789012345678901234.5}", "{0.25}"}
	for _, r := range rows {
		for i, w := range want {
			if got := r[i].String(); got != w {
				t.Errorf("column %d = %s, want %s", i, got, w)
			}
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
}
