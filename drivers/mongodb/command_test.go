package mongodb

import (
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

func TestParseCommand(t *testing.T) {
	cmd, err := parseCommand(` {"find":"orders","filter":{"_id":{"$oid":"65a1b2c3d4e5f60718293a4b"},"n":{"$numberDecimal":"1.5"}},"limit":10} `)
	if err != nil {
		t.Fatal(err)
	}
	if cmd[0].Key != "find" || cmd[0].Value != "orders" {
		t.Fatalf("first element %v", cmd[0])
	}
	f := cmd[1].Value.(bson.D)
	if _, ok := f[0].Value.(bson.ObjectID); !ok {
		t.Errorf("$oid not decoded: %T", f[0].Value)
	}
	if _, ok := f[1].Value.(bson.Decimal128); !ok {
		t.Errorf("$numberDecimal not decoded: %T", f[1].Value)
	}
	for _, bad := range []string{``, `   `, `[1]`, `"find"`, `{"find":"c"} {"drop":"c"}`, `{"find":"c"} trailing`, `{"find":`, `{}`} {
		if _, err := parseCommand(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseCommand(%q): want ErrSyntax, got %v", bad, err)
		}
	}
}

func TestPrepareCommand(t *testing.T) {
	prep := func(text string, ro bool) (bson.D, error) {
		cmd, err := parseCommand(text)
		if err != nil {
			t.Fatal(err)
		}
		return prepareCommand(cmd, ro)
	}
	t.Run("reserved keys refused on every call", func(t *testing.T) {
		for _, text := range []string{`{"find":"c","$db":"admin"}`, `{"find":"c","lsid":{}}`, `{"find":"c","txnNumber":1}`, `{"find":"c","$readPreference":{}}`} {
			if _, err := prep(text, false); !errors.Is(err, driver.ErrSyntax) {
				t.Errorf("%s writable: want ErrSyntax, got %v", text, err)
			}
		}
	})
	t.Run("read concern local on read-only reads", func(t *testing.T) {
		got, err := prep(`{"find":"c"}`, true)
		if err != nil {
			t.Fatal(err)
		}
		rc, ok := lookup(got, "readConcern")
		if !ok || rc.(bson.D)[0].Value != "local" {
			t.Errorf("readConcern = %v", rc)
		}
		got, _ = prep(`{"find":"c","readConcern":{"level":"majority"}}`, true)
		if rc, _ := lookup(got, "readConcern"); rc.(bson.D)[0].Value != "majority" {
			t.Errorf("explicit readConcern replaced: %v", rc)
		}
		got, _ = prep(`{"find":"c"}`, false)
		if _, ok := lookup(got, "readConcern"); ok {
			t.Error("readConcern added on a writable call")
		}
	})
	t.Run("aggregate gets a cursor", func(t *testing.T) {
		got, err := prep(`{"aggregate":"c","pipeline":[]}`, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := lookup(got, "cursor"); !ok {
			t.Error("no cursor option added")
		}
		got, _ = prep(`{"aggregate":"c","pipeline":[],"explain":true}`, false)
		if _, ok := lookup(got, "cursor"); ok {
			t.Error("cursor added to an explain aggregate")
		}
	})
	t.Run("read-only applies the guard", func(t *testing.T) {
		if _, err := prep(`{"drop":"c"}`, true); !errors.Is(err, driver.ErrReadOnly) {
			t.Errorf("want ErrReadOnly, got %v", err)
		}
		if _, err := prep(`{"drop":"c"}`, false); err != nil {
			t.Errorf("writable drop refused: %v", err)
		}
	})
}
