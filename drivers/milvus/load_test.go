package milvus

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"

	"github.com/xavidop/tobira/driver"
)

func notLoadedQuery(*milvuspb.QueryRequest) (*milvuspb.QueryResults, error) {
	return &milvuspb.QueryResults{Status: errStatus(101, "collection not loaded")}, nil
}

// A read-only connection never loads a collection: it explains instead.
func TestReadOnlyNeverLoads(t *testing.T) {
	c, fake := newFakeConn(t, true)
	fake.describe = describeOK(peopleProto())
	fake.query = notLoadedQuery
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"people"}`})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Query on an unloaded collection (ro) = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "not loaded") || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("the error lacks the hint: %v", err)
	}
	b, _ := driver.As[driver.Browser](driver.Conn(c))
	_, err = b.Browse(context.Background(), driver.CollectionRef{Name: "people"}, driver.BrowseOptions{})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Browse on an unloaded collection (ro) = %v", err)
	}
	if slices.Contains(fake.Calls(), "LoadCollection") {
		t.Errorf("a read-only connection called LoadCollection: %v", fake.Calls())
	}
}

// Query.ReadOnly on a writable connection does not load either.
func TestReadOnlyFlagNeverLoads(t *testing.T) {
	c, fake := newFakeConn(t, false)
	fake.describe = describeOK(peopleProto())
	fake.query = notLoadedQuery
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"people"}`, ReadOnly: true})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Query.ReadOnly on an unloaded collection = %v", err)
	}
	b, _ := driver.As[driver.Browser](driver.Conn(c))
	_, err = b.Browse(context.Background(), driver.CollectionRef{Name: "people"}, driver.BrowseOptions{ReadOnly: true})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Browse.ReadOnly on an unloaded collection = %v", err)
	}
	if slices.Contains(fake.Calls(), "LoadCollection") {
		t.Errorf("a read-only request called LoadCollection: %v", fake.Calls())
	}
}

// A writable connection loads the collection once and retries.
func TestWritableAutoLoads(t *testing.T) {
	c, fake := newFakeConn(t, false)
	fake.describe = describeOK(peopleProto())
	loaded := false
	fake.load = func(r *milvuspb.LoadCollectionRequest) (*commonpb.Status, error) {
		if r.GetCollectionName() != "people" || r.GetDbName() != "default" {
			t.Errorf("load request = %v", r)
		}
		loaded = true
		return okStatus(), nil
	}
	fake.loadState = func(*milvuspb.GetLoadStateRequest) (*milvuspb.GetLoadStateResponse, error) {
		return &milvuspb.GetLoadStateResponse{Status: okStatus(), State: commonpb.LoadState_LoadStateLoaded}, nil
	}
	fake.query = func(r *milvuspb.QueryRequest) (*milvuspb.QueryResults, error) {
		if !loaded {
			return notLoadedQuery(r)
		}
		return &milvuspb.QueryResults{Status: okStatus(), FieldsData: []*schemapb.FieldData{
			{FieldName: "id", Type: schemapb.DataType_Int64, Field: &schemapb.FieldData_Scalars{Scalars: &schemapb.ScalarField{Data: &schemapb.ScalarField_LongData{LongData: &schemapb.LongArray{Data: []int64{1}}}}}},
		}}, nil
	}
	rs, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"people","outputFields":["id"]}`})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := driver.Collect(context.Background(), rs, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v, err %v", rows, err)
	}
	if !loaded {
		t.Error("the writable connection did not load the collection")
	}
}

// A load that fails (no index) keeps its error and says why.
func TestAutoLoadFailureHint(t *testing.T) {
	c, fake := newFakeConn(t, false)
	fake.describe = describeOK(peopleProto())
	fake.query = notLoadedQuery
	fake.load = func(*milvuspb.LoadCollectionRequest) (*commonpb.Status, error) {
		return errStatus(700, "index not found[collection=people]"), nil
	}
	_, err := c.Query(context.Background(), driver.Query{Text: `{"collection":"people"}`})
	if err == nil || !strings.Contains(err.Error(), "index") {
		t.Errorf("failed auto-load = %v, want the index hint", err)
	}
}
