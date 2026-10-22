//go:build integration

package mongodb

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

const probeDB = "tobira_probes"

// probeState captures what the probes could change in probeDB: the
// documents of c, the collections, the indexes of c and the users.
func probeState(t *testing.T) string {
	t.Helper()
	ctx := ctx30(t)
	db := admin.Database(probeDB)
	var sb strings.Builder
	cur, err := db.Collection("c").Find(ctx, bson.D{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for cur.Next(ctx) {
		sb.WriteString(cur.Current.String())
	}
	_ = cur.Close(ctx)
	names, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	sb.WriteString(strings.Join(names, ","))
	specs, err := db.Collection("c").Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		sb.WriteString(s.Name)
	}
	users, err := admin.Database(probeDB).RunCommand(ctx, bson.D{{Key: "usersInfo", Value: 1}}).Raw()
	if err != nil {
		t.Fatal(err)
	}
	sb.WriteString(users.Lookup("users").String())
	return sb.String()
}

func seedProbes(t *testing.T) {
	t.Helper()
	ctx := ctx30(t)
	db := admin.Database(probeDB)
	_ = db.Drop(ctx)
	if _, err := db.Collection("c").InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: 1}, {Key: "a", Value: 1}},
		bson.D{{Key: "_id", Value: 2}, {Key: "a", Value: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("d").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationReadOnlyProbes sends every write family and escape attempt
// with Query.ReadOnly, and on a read-only connection, and checks each is
// ErrReadOnly and that nothing changed.
func TestIntegrationReadOnlyProbes(t *testing.T) {
	seedProbes(t)
	before := probeState(t)
	rw := openTest(t, probeDB, false)
	ro := openTest(t, probeDB, true)
	for _, p := range readOnlyProbes {
		t.Run(p.name, func(t *testing.T) {
			for name, run := range map[string]func() error{
				"Query.ReadOnly": func() error {
					rs, err := rw.Query(ctx30(t), driver.Query{Text: p.cmd, ReadOnly: true})
					if err == nil {
						_ = rs.Close()
					}
					return err
				},
				"read-only conn": func() error {
					rs, err := ro.Query(ctx30(t), driver.Query{Text: p.cmd})
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
	t.Run("connections still serve reads", func(t *testing.T) {
		for _, c := range []driver.Conn{rw, ro} {
			if _, rows, _ := collect(t, c, driver.Query{Text: `{"find":"c"}`, ReadOnly: true}); len(rows) != 2 {
				t.Errorf("got %d rows", len(rows))
			}
		}
	})
}

// TestIntegrationReadOnlyAllowed runs every allow-listed command on a
// read-only connection: the guard lets them through and the server runs
// them.
func TestIntegrationReadOnlyAllowed(t *testing.T) {
	seedProbes(t)
	ro := openTest(t, probeDB, true)
	for _, text := range readOnlyAllowed {
		rs, err := ro.Query(ctx30(t), driver.Query{Text: text})
		if err != nil {
			t.Errorf("%s: %v", text, err)
			continue
		}
		if _, _, err := driver.Collect(context.Background(), rs, 0); err != nil {
			t.Errorf("%s rows: %v", text, err)
		}
	}
}
