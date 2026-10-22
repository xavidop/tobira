package mongodb

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestUpdateDocument(t *testing.T) {
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	got, err := updateDocument(driver.Object{m("name", driver.String("x")), m("address", driver.ObjectValue(m("city", driver.String("y")))), m("gone", driver.Null())})
	if err != nil {
		t.Fatal(err)
	}
	if s := extJSON(t, got); s != `{"$set":{"name":"x","address":{"city":"y"},"gone":null}}` {
		t.Errorf("got %s", s)
	}
	for name, ch := range map[string]driver.Object{
		"empty":    {},
		"_id":      {m("_id", driver.Int(1))},
		"operator": {m("$inc", driver.Int(1))},
		"blank":    {m("", driver.Int(1))},
		"dotted":   {m("address.city", driver.String("y"))},
	} {
		if _, err := updateDocument(ch); !errors.Is(err, driver.ErrSyntax) && !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("%s: want ErrSyntax or ErrUnsupported, got %v", name, err)
		}
	}
}

func TestKeyFilter(t *testing.T) {
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	got, err := keyFilter(driver.Object{m("id", driver.Int(7)), m("_id", driver.String("65a1b2c3d4e5f60718293a4b"))})
	if err != nil {
		t.Fatal(err)
	}
	if s := extJSON(t, got); s != `{"id":7,"_id":{"$in":[{"$oid":"65a1b2c3d4e5f60718293a4b"},"65a1b2c3d4e5f60718293a4b"]}}` {
		t.Errorf("got %s", s)
	}
	if _, err := keyFilter(nil); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("empty key: want ErrSyntax, got %v", err)
	}
	if _, err := keyFilter(driver.Object{m("$where", driver.String("1"))}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("operator key: want ErrSyntax, got %v", err)
	}
	for name, v := range map[string]driver.Value{
		"operator object":  driver.ObjectValue(m("$gt", driver.Int(1))),
		"nested operator":  driver.ObjectValue(m("a", driver.ObjectValue(m("$ne", driver.Null())))),
		"operator in list": driver.List(driver.ObjectValue(m("$exists", driver.Bool(true)))),
		"operator in JSON": driver.JSON([]byte(`{"$gt":1}`)),
	} {
		if _, err := keyFilter(driver.Object{m("_id", v)}); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("%s: want ErrSyntax, got %v", name, err)
		}
	}
	if _, err := keyFilter(driver.Object{m("_id", driver.ObjectValue(m("tenant", driver.Int(1))))}); err != nil {
		t.Errorf("a plain composite _id is a valid key: %v", err)
	}
}

// The write re-checks every key field, not only the resolved _id.
func TestWriteFilter(t *testing.T) {
	m := func(k string, v driver.Value) driver.Member { return driver.Member{Key: k, Value: v} }
	key, err := keyFilter(driver.Object{m("id", driver.Int(7)), m("tenant", driver.String("a"))})
	if err != nil {
		t.Fatal(err)
	}
	got := writeFilter(rawField(t, int32(3)), key)
	if s := extJSON(t, got); s != `{"$and":[{"_id":3},{"id":7,"tenant":"a"}]}` {
		t.Errorf("got %s", s)
	}
}
