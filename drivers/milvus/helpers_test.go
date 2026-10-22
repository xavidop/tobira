package milvus

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/xavidop/tobira/driver"
)

// fakeServer is an in-process Milvus gRPC service. Every RPC it does not
// override answers Unimplemented, and every call is recorded, so a test can
// prove the driver decided before the network (no calls) or check which
// RPCs it made.
type fakeServer struct {
	milvuspb.UnimplementedMilvusServiceServer

	mu    sync.Mutex
	calls []string
	// dbHeaders are the dbname headers DescribeCollection received.
	dbHeaders []string

	describe  func(*milvuspb.DescribeCollectionRequest) (*milvuspb.DescribeCollectionResponse, error)
	query     func(*milvuspb.QueryRequest) (*milvuspb.QueryResults, error)
	search    func(*milvuspb.SearchRequest) (*milvuspb.SearchResults, error)
	load      func(*milvuspb.LoadCollectionRequest) (*commonpb.Status, error)
	loadState func(*milvuspb.GetLoadStateRequest) (*milvuspb.GetLoadStateResponse, error)
	index     func(*milvuspb.DescribeIndexRequest) (*milvuspb.DescribeIndexResponse, error)
}

func (f *fakeServer) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

// Calls returns the RPC names received so far.
func (f *fakeServer) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeServer) DescribeCollection(ctx context.Context, r *milvuspb.DescribeCollectionRequest) (*milvuspb.DescribeCollectionResponse, error) {
	f.record("DescribeCollection")
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	f.dbHeaders = append(f.dbHeaders, md.Get("dbname")...)
	f.mu.Unlock()
	if f.describe == nil {
		return f.UnimplementedMilvusServiceServer.DescribeCollection(context.Background(), r)
	}
	return f.describe(r)
}

func (f *fakeServer) Query(_ context.Context, r *milvuspb.QueryRequest) (*milvuspb.QueryResults, error) {
	f.record("Query")
	if f.query == nil {
		return f.UnimplementedMilvusServiceServer.Query(context.Background(), r)
	}
	return f.query(r)
}

func (f *fakeServer) Search(_ context.Context, r *milvuspb.SearchRequest) (*milvuspb.SearchResults, error) {
	f.record("Search")
	if f.search == nil {
		return f.UnimplementedMilvusServiceServer.Search(context.Background(), r)
	}
	return f.search(r)
}

func (f *fakeServer) LoadCollection(_ context.Context, r *milvuspb.LoadCollectionRequest) (*commonpb.Status, error) {
	f.record("LoadCollection")
	if f.load == nil {
		return f.UnimplementedMilvusServiceServer.LoadCollection(context.Background(), r)
	}
	return f.load(r)
}

func (f *fakeServer) GetLoadState(_ context.Context, r *milvuspb.GetLoadStateRequest) (*milvuspb.GetLoadStateResponse, error) {
	f.record("GetLoadState")
	if f.loadState == nil {
		return f.UnimplementedMilvusServiceServer.GetLoadState(context.Background(), r)
	}
	return f.loadState(r)
}

func (f *fakeServer) DescribeIndex(_ context.Context, r *milvuspb.DescribeIndexRequest) (*milvuspb.DescribeIndexResponse, error) {
	f.record("DescribeIndex")
	if f.index == nil {
		return f.UnimplementedMilvusServiceServer.DescribeIndex(context.Background(), r)
	}
	return f.index(r)
}

// Every other RPC the driver could send is recorded too.

func (f *fakeServer) Insert(ctx context.Context, r *milvuspb.InsertRequest) (*milvuspb.MutationResult, error) {
	f.record("Insert")
	return f.UnimplementedMilvusServiceServer.Insert(ctx, r)
}

func (f *fakeServer) Upsert(ctx context.Context, r *milvuspb.UpsertRequest) (*milvuspb.MutationResult, error) {
	f.record("Upsert")
	return f.UnimplementedMilvusServiceServer.Upsert(ctx, r)
}

func (f *fakeServer) Delete(ctx context.Context, r *milvuspb.DeleteRequest) (*milvuspb.MutationResult, error) {
	f.record("Delete")
	return f.UnimplementedMilvusServiceServer.Delete(ctx, r)
}

func (f *fakeServer) ShowCollections(ctx context.Context, r *milvuspb.ShowCollectionsRequest) (*milvuspb.ShowCollectionsResponse, error) {
	f.record("ShowCollections")
	return f.UnimplementedMilvusServiceServer.ShowCollections(ctx, r)
}

// newFakeConn returns a conn to a fresh fakeServer over bufconn.
func newFakeConn(t *testing.T, readOnly bool) (*conn, *fakeServer) {
	t.Helper()
	return newFakeConnCfg(t, driver.ConnConfig{ReadOnly: readOnly})
}

func newFakeConnCfg(t *testing.T, cfg driver.ConnConfig) (*conn, *fakeServer) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fake := &fakeServer{}
	milvuspb.RegisterMilvusServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(authInterceptor("", "default")))
	if err != nil {
		t.Fatal(err)
	}
	c := newConn(milvuspb.NewMilvusServiceClient(cc), cc.Close, "default", cfg)
	t.Cleanup(func() {
		_ = c.Close()
		srv.Stop()
	})
	return c, fake
}

// okStatus is a success status.
func okStatus() *commonpb.Status { return &commonpb.Status{} }

// errStatus is a Milvus failure status with the given numeric code.
func errStatus(code int32, reason string) *commonpb.Status {
	return &commonpb.Status{Code: code, Reason: reason}
}

// peopleProto is the schema of a small People-like collection: id (Int64
// key), name (VarChar), age (nullable Int64), meta (JSON), embedding
// (FloatVector(2)), with the dynamic field on.
func peopleProto() *schemapb.CollectionSchema {
	return &schemapb.CollectionSchema{
		Name:               "people",
		EnableDynamicField: true,
		Fields: []*schemapb.FieldSchema{
			{FieldID: 100, Name: "id", DataType: schemapb.DataType_Int64, IsPrimaryKey: true},
			{FieldID: 101, Name: "name", DataType: schemapb.DataType_VarChar, TypeParams: []*commonpb.KeyValuePair{{Key: "max_length", Value: "64"}}},
			{FieldID: 102, Name: "age", DataType: schemapb.DataType_Int64, Nullable: true},
			{FieldID: 103, Name: "meta", DataType: schemapb.DataType_JSON},
			{FieldID: 104, Name: "embedding", DataType: schemapb.DataType_FloatVector, TypeParams: []*commonpb.KeyValuePair{{Key: "dim", Value: "2"}}},
			{FieldID: 105, Name: "$meta", DataType: schemapb.DataType_JSON, IsDynamic: true},
		},
	}
}

// peopleSchema is peopleProto as the driver sees it.
func peopleSchema() *collSchema {
	return schemaFromProto("default", &milvuspb.DescribeCollectionResponse{Status: okStatus(), Schema: peopleProto(), CollectionName: "people"})
}

// describeOK answers DescribeCollection with schema.
func describeOK(schema *schemapb.CollectionSchema) func(*milvuspb.DescribeCollectionRequest) (*milvuspb.DescribeCollectionResponse, error) {
	return func(r *milvuspb.DescribeCollectionRequest) (*milvuspb.DescribeCollectionResponse, error) {
		return &milvuspb.DescribeCollectionResponse{Status: okStatus(), Schema: schema, CollectionName: r.GetCollectionName(), DbName: r.GetDbName()}, nil
	}
}
