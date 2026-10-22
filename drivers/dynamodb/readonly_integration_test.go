//go:build integration

package dynamodb

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
)

const probeTable = "tobira_it_probes"

// readOnlyProbes are writes and escape attempts the guard must refuse.
var readOnlyProbes = []string{
	`DELETE FROM "tobira_it_probes" WHERE pk = 'a'`,
	`delete from tobira_it_probes where pk = 'a'`,
	`UPDATE "tobira_it_probes" SET v = 9 WHERE pk = 'a'`,
	`UPDATE "tobira_it_probes" REMOVE v WHERE pk = 'a'`,
	`UPDATE "tobira_it_probes" SET v = 9 WHERE pk = 'a' RETURNING ALL NEW *`,
	`INSERT INTO "tobira_it_probes" VALUE {'pk': 'z', 'v': 1}`,
	`  /* SELECT */ DELETE FROM "tobira_it_probes" WHERE pk = 'a'`,
	"-- SELECT\nDELETE FROM \"tobira_it_probes\" WHERE pk = 'a'",
	`SELECT * FROM "tobira_it_probes"; DELETE FROM "tobira_it_probes" WHERE pk = 'a'`,
	`EXISTS(SELECT * FROM "tobira_it_probes" WHERE pk = 'a')`,
	`SELECT * FROM "tobira_it_probes" WHERE EXISTS(SELECT * FROM "tobira_it_probes")`,
	`"DELETE" FROM "tobira_it_probes" WHERE pk = 'a'`,
	"SELECT * FROM \"tobira_it_probes\" WHERE v = `1`",
	`SELECT * FROM "tobira_it_probes" WHERE pk = 'unterminated`,
	`SELECT * FROM "tobira_it_probes" /* x /* y */ DELETE FROM "tobira_it_probes" WHERE pk = 'a' */`,
	``,
}

// readOnlyAllowed are reads the guard lets through and the server runs.
var readOnlyAllowed = []string{
	`SELECT * FROM "tobira_it_probes"`,
	`select pk, v from tobira_it_probes where pk = 'a'`,
	`SELECT * FROM "tobira_it_probes" WHERE pk = 'it''s; DELETE'`,
	`SELECT * FROM "tobira_it_probes" WHERE begins_with(pk, 'a') AND v > 0`,
	`SELECT * FROM "tobira_it_probes" -- a comment`,
	`/* leading */ SELECT * FROM "tobira_it_probes" WHERE pk IN ['a', 'b'];`,
	`SELECT * FROM "tobira_it_probes" WHERE pk = 'a' ORDER BY pk DESC`,
}

func seedProbes(t *testing.T) {
	t.Helper()
	if err := createTable(ctx30(t), probeTable, "pk", "S", "", "", false); err != nil {
		t.Fatal(err)
	}
	put(t, probeTable, item{"pk": str("a"), "v": num(1)}, item{"pk": str("b"), "v": num(2)})
}

// probeState is the table's content and the table list.
func probeState(t *testing.T) string {
	t.Helper()
	out, err := admin.Scan(ctx30(t), &dynamodb.ScanInput{TableName: aws.String(probeTable), ConsistentRead: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, it := range out.Items {
		rows = append(rows, fmt.Sprint(valueOf(&types.AttributeValueMemberM{Value: it})))
	}
	slices.Sort(rows)
	tables, err := admin.ListTables(ctx30(t), &dynamodb.ListTablesInput{})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(rows, ";") + "|" + strings.Join(tables.TableNames, ",")
}

// TestIntegrationReadOnlyProbes sends every write family and escape attempt
// with Query.ReadOnly, and on a read-only connection, and checks each is
// ErrReadOnly and that nothing changed.
func TestIntegrationReadOnlyProbes(t *testing.T) {
	seedProbes(t)
	before := probeState(t)
	rw := openTest(t, false)
	ro := openTest(t, true)
	for _, p := range readOnlyProbes {
		t.Run(p, func(t *testing.T) {
			for name, run := range map[string]func() error{
				"Query.ReadOnly": func() error {
					rs, err := rw.Query(ctx30(t), driver.Query{Text: p, ReadOnly: true})
					if err == nil {
						_ = rs.Close()
					}
					return err
				},
				"read-only conn": func() error {
					rs, err := ro.Query(ctx30(t), driver.Query{Text: p})
					if err == nil {
						_ = rs.Close()
					}
					return err
				},
			} {
				if err := run(); !errors.Is(err, driver.ErrReadOnly) {
					t.Errorf("%s: want ErrReadOnly, got %v", name, err)
				}
			}
			if after := probeState(t); after != before {
				t.Fatalf("state changed:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
	t.Run("edits on a read-only conn", func(t *testing.T) {
		ref := driver.CollectionRef{Name: probeTable}
		_, err := ro.Insert(ctx30(t), ref, driver.Object{{Key: "pk", Value: driver.String("z")}})
		for name, err := range map[string]error{
			"insert": err,
			"update": ro.Update(ctx30(t), ref, driver.Object{{Key: "pk", Value: driver.String("a")}}, driver.Object{{Key: "v", Value: driver.Int(5)}}),
			"delete": ro.Delete(ctx30(t), ref, driver.Object{{Key: "pk", Value: driver.String("a")}}),
		} {
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%s: want ErrReadOnly, got %v", name, err)
			}
		}
		if after := probeState(t); after != before {
			t.Fatalf("state changed:\nbefore %s\nafter  %s", before, after)
		}
	})
}

// TestIntegrationReadOnlyAllowed runs every allowed read on a read-only
// connection: the guard lets it through and the server runs it.
func TestIntegrationReadOnlyAllowed(t *testing.T) {
	seedProbes(t)
	ro := openTest(t, true)
	for _, text := range readOnlyAllowed {
		rs, err := ro.Query(ctx30(t), driver.Query{Text: text})
		if err != nil {
			t.Errorf("%s: %v", text, err)
			continue
		}
		if _, _, err := driver.Collect(ctx30(t), rs, 0); err != nil {
			t.Errorf("%s rows: %v", text, err)
		}
	}
	if _, rows, _, err := browse(t, ro, probeTable, driver.BrowseOptions{}); err != nil || len(rows) != 2 {
		t.Errorf("Browse on a read-only conn: %d rows, %v", len(rows), err)
	}
}
