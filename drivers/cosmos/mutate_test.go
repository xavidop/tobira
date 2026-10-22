package cosmos

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

// etagServer is a fake account whose item read returns ETag "v1" and whose
// replace answers 412, as Cosmos DB does when the item changed in between.
func etagServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var ifMatch []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/":
			loc := `[{"name":"local","databaseAccountEndpoint":"` + srv.URL + `/"}]`
			_, _ = w.Write([]byte(`{"id":"fake","writableLocations":` + loc + `,"readableLocations":` + loc + `}`))
		case r.Method == http.MethodGet && r.URL.Path == "/dbs/app":
			_, _ = w.Write([]byte(`{"id":"app","_rid":"a","_self":"dbs/app/"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/dbs/app/colls/orders":
			_, _ = w.Write([]byte(`{"id":"orders","_rid":"b","_self":"dbs/app/colls/orders/","partitionKey":{"paths":["/tenant"],"kind":"Hash"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/dbs/app/colls/orders/docs/o1":
			w.Header().Set("Etag", `"v1"`)
			_, _ = w.Write([]byte(`{"id":"o1","tenant":"acme","total":1,"_etag":"\"v1\"","_ts":1}`))
		case r.Method == http.MethodPut && r.URL.Path == "/dbs/app/colls/orders/docs/o1":
			mu.Lock()
			ifMatch = append(ifMatch, r.Header.Get("If-Match"))
			mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"code":"PreconditionFailed","message":"Operation cannot be performed because one of the specified precondition is not met."}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"NotFound","message":"Resource Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &ifMatch
}

// Update replaces the item only if it still has the ETag it was read with:
// a change in between is ErrConflict.
func TestUpdateIsETagGuarded(t *testing.T) {
	srv, ifMatch := etagServer(t)
	dsn := "cosmos://" + strings.TrimPrefix(srv.URL, "http://") + "?tls=false&database=app"
	c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn, Password: driver.NewSecret(testKey)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	m := c.(driver.Mutator)
	key := driver.Object{{Key: "tenant", Value: driver.String("acme")}, {Key: "id", Value: driver.String("o1")}}
	err = m.Update(context.Background(), driver.CollectionRef{Name: "orders"}, key, driver.Object{{Key: "total", Value: driver.Int(2)}})
	if !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("Update after a concurrent change: want ErrConflict, got %v", err)
	}
	if len(*ifMatch) != 1 || (*ifMatch)[0] != `"v1"` {
		t.Errorf("replace If-Match headers = %q, want one \"v1\"", *ifMatch)
	}
	err = m.Delete(context.Background(), driver.CollectionRef{Name: "orders"}, driver.Object{{Key: "tenant", Value: driver.String("acme")}, {Key: "id", Value: driver.String("zz")}})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Delete of a missing item: want ErrNotFound, got %v", err)
	}
}

func TestItemKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  driver.Object
		ok   bool
	}{
		{"full", driver.Object{{Key: "tenant", Value: driver.String("a")}, {Key: "id", Value: driver.String("1")}}, true},
		{"no partition key", driver.Object{{Key: "id", Value: driver.String("1")}}, false},
		{"numeric id", driver.Object{{Key: "tenant", Value: driver.String("a")}, {Key: "id", Value: driver.Int(1)}}, false},
		{"extra field", driver.Object{{Key: "tenant", Value: driver.String("a")}, {Key: "id", Value: driver.String("1")}, {Key: "x", Value: driver.Int(1)}}, false},
		{"list partition key", driver.Object{{Key: "tenant", Value: driver.List()}, {Key: "id", Value: driver.String("1")}}, false},
	} {
		props := ordersProps()
		_, _, err := itemKey("update", tc.key, &props)
		if (err == nil) != tc.ok {
			t.Errorf("%s: itemKey error %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func ordersProps() azcosmos.ContainerProperties {
	return azcosmos.ContainerProperties{ID: "orders", PartitionKeyDefinition: azcosmos.PartitionKeyDefinition{Paths: []string{"/tenant"}}}
}

func TestChangeEvent(t *testing.T) {
	props := ordersProps()
	ev, err := changeEvent([]byte(`{"id":"o1","tenant":"acme","n":1,"_ts":1700000000,"_lsn":4}`), keyPaths(&props), "orders")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Op != "upsert" || ev.Channel != "orders" || ev.Time.Unix() != 1700000000 || len(ev.Key) != 2 || ev.Key[0].Key != "tenant" || ev.Key[1].Key != "id" {
		t.Errorf("event = %+v", ev)
	}
	if _, err := changeEvent([]byte(`[1]`), nil, "orders"); err == nil {
		t.Error("a non-object change: want an error")
	}
}
