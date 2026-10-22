//go:build integration

package qdrant

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// writeBodies are request bodies that would write if the op ran: each
// mutation op is sent with a real body, so a guard that let one through
// would change People.
var writeBodies = map[string]string{
	"upsert":             `"points":[{"id":{"num":"1"},"payload":{"name":{"string_value":"pwned"}},"vectors":{"vectors":{}}}]`,
	"delete":             `"points":{"points":{"ids":[{"num":"1"}]}}`,
	"set_payload":        `"payload":{"name":{"string_value":"pwned"}},"points_selector":{"points":{"ids":[{"num":"1"}]}}`,
	"overwrite_payload":  `"payload":{"name":{"string_value":"pwned"}},"points_selector":{"points":{"ids":[{"num":"1"}]}}`,
	"delete_payload":     `"keys":["name"],"points_selector":{"points":{"ids":[{"num":"1"}]}}`,
	"clear_payload":      `"points":{"points":{"ids":[{"num":"1"}]}}`,
	"update_vectors":     `"points":[]`,
	"delete_vectors":     `"points_selector":{"points":{"ids":[{"num":"1"}]}},"vectors":{"names":["x"]}`,
	"create_field_index": `"field_name":"name"`,
	"delete_field_index": `"field_name":"age"`,
	"update_batch":       `"operations":[{"delete_points":{"points":{"points":{"ids":[{"num":"1"}]}}}}]`,
	"delete_collection":  `"timeout":"1"`,
	"update_collection":  `"optimizers_config":{"indexing_threshold":"1"}`,
	"create_snapshot":    `"wait":true`,
	"create_alias":       `"alias_name":"x"`,
}

// TestReadOnlyProbes sends every Qdrant write family through Query on the
// read-only paths (a read-only connection and Query.ReadOnly) and through
// the Mutator on a read-only connection: each is refused with ErrReadOnly
// and People is unchanged afterwards.
func TestReadOnlyProbes(t *testing.T) {
	rw := open(t, driver.ConnConfig{})
	ro := open(t, driver.ConnConfig{ReadOnly: true})
	ctx := ctx30(t)
	ops := append([]string{}, mutationOps...)
	for op := range writeBodies {
		ops = append(ops, op)
	}
	for _, op := range ops {
		body, ok := writeBodies[op]
		if !ok {
			body = writeBodies["delete"]
		}
		text := `{"op":"` + op + `","collection":"tobira_people",` + body + `}`
		if _, err := ro.Query(ctx, driver.Query{Text: text}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only conn, op %q: err = %v, want ErrReadOnly", op, err)
		}
		if _, err := rw.Query(ctx, driver.Query{Text: text, ReadOnly: true}); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("Query.ReadOnly, op %q: err = %v, want ErrReadOnly", op, err)
		}
		// Without read-only, Query still never writes.
		if _, err := rw.Query(ctx, driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("writable Query, op %q: err = %v, want ErrUnsupported", op, err)
		}
	}
	ref := driver.CollectionRef{Namespace: defaultNamespace, Name: peopleColl}
	key := driver.Object{{Key: "id", Value: driver.Int(1)}}
	_, insErr := ro.Insert(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(5000)}, {Key: "name", Value: driver.String("x")}})
	for name, err := range map[string]error{
		"Insert": insErr,
		"Update": ro.Update(ctx, ref, key, driver.Object{{Key: "name", Value: driver.String("pwned")}}),
		"Delete": ro.Delete(ctx, ref, key),
	} {
		if !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("read-only %s: err = %v, want ErrReadOnly", name, err)
		}
	}
	// Browse with ReadOnly takes a raw filter: a filter message cannot
	// write, and a write body in it is a syntax error.
	b := driver.BrowseOptions{ReadOnly: true, Filter: driver.Filter{Raw: `{"points":{"ids":[1]}}`}}
	if _, err := ro.Browse(ctx, ref, b); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("Browse with a write body as filter: err = %v, want ErrSyntax", err)
	}
	assertPeopleUnchanged(t, rw)
}

// TestReadOnlyAPIKey: Qdrant's own read-only API key is the server-side
// guard under the driver's. A writable connection opened with it reads,
// and its writes fail with ErrPermission from the server.
func TestReadOnlyAPIKey(t *testing.T) {
	c := open(t, driver.ConnConfig{DSN: testDSN(testROAPIKey)})
	ctx := ctx30(t)
	_, rows, _ := collect(t, c, driver.Query{Text: `{"op":"scroll","collection":"tobira_people","limit":3}`})
	if len(rows) != 3 {
		t.Errorf("read with the read-only key: %d rows", len(rows))
	}
	ref := driver.CollectionRef{Namespace: defaultNamespace, Name: peopleColl}
	err := c.Update(ctx, ref, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "name", Value: driver.String("pwned")}})
	if !errors.Is(err, driver.ErrPermission) {
		t.Errorf("write with the read-only key: err = %v, want ErrPermission", err)
	}
	assertPeopleUnchanged(t, open(t, driver.ConnConfig{}))
}

func assertPeopleUnchanged(t *testing.T, c driver.Querier) {
	t.Helper()
	cols, rows, _ := collect(t, c, driver.Query{Text: `{"op":"scroll","collection":"tobira_people"}`})
	if len(rows) != len(drivertest.People.Rows) {
		t.Fatalf("People has %d rows, want %d", len(rows), len(drivertest.People.Rows))
	}
	for r, want := range drivertest.People.Rows {
		for i, pc := range drivertest.People.Columns {
			if got := rows[r][colIndex(t, cols, pc.Name)]; !drivertest.EqualValue(want[i], got) {
				t.Errorf("People row %d %s = %s, want %s", r, pc.Name, got, want[i])
			}
		}
	}
}
