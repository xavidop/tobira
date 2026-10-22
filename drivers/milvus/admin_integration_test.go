//go:build integration

package milvus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// adminClient is the test-side administrator: schema, index, load and
// insert calls over the milvus-proto gRPC stubs, as the driver does. The
// SDK's milvusclient package is not used because it links Milvus's
// embedded etcd server, which does not compile in the go.work workspace.
type adminClient struct {
	svc milvuspb.MilvusServiceClient
	cc  *grpc.ClientConn
}

func newAdminClient(addr, token string) (*adminClient, error) {
	cc, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(authInterceptor(token, defaultDB)),
	)
	if err != nil {
		return nil, err
	}
	return &adminClient{svc: milvuspb.NewMilvusServiceClient(cc), cc: cc}, nil
}

func (a *adminClient) Close() error { return a.cc.Close() }

func (a *adminClient) createCollection(ctx context.Context, sch *entity.Schema) error {
	b, err := proto.Marshal(sch.ProtoMessage())
	if err != nil {
		return err
	}
	st, err := a.svc.CreateCollection(ctx, &milvuspb.CreateCollectionRequest{
		CollectionName: sch.CollectionName,
		Schema:         b,
		ShardsNum:      1,
	})
	return checkRPC(st, err)
}

func (a *adminClient) dropCollection(ctx context.Context, name string) error {
	st, err := a.svc.DropCollection(ctx, &milvuspb.DropCollectionRequest{CollectionName: name})
	return checkRPC(st, err)
}

func (a *adminClient) createDatabase(ctx context.Context, name string) error {
	st, err := a.svc.CreateDatabase(ctx, &milvuspb.CreateDatabaseRequest{DbName: name})
	return checkRPC(st, err)
}

func (a *adminClient) dropDatabase(ctx context.Context, name string) error {
	st, err := a.svc.DropDatabase(ctx, &milvuspb.DropDatabaseRequest{DbName: name})
	return checkRPC(st, err)
}

// createIndex builds idx on a field and waits until it is finished.
func (a *adminClient) createIndex(ctx context.Context, coll, field string, idx index.Index) error {
	st, err := a.svc.CreateIndex(ctx, &milvuspb.CreateIndexRequest{
		CollectionName: coll,
		FieldName:      field,
		ExtraParams:    entity.MapKvPairs(idx.Params()),
	})
	if err := checkRPC(st, err); err != nil {
		return err
	}
	return poll(ctx, func() (bool, error) {
		resp, err := a.svc.DescribeIndex(ctx, &milvuspb.DescribeIndexRequest{CollectionName: coll, FieldName: field})
		if err := checkRPC(resp, err); err != nil {
			return false, err
		}
		for _, d := range resp.GetIndexDescriptions() {
			switch d.GetState() {
			case commonpb.IndexState_Finished:
				return true, nil
			case commonpb.IndexState_Failed:
				return false, fmt.Errorf("index %s.%s failed: %s", coll, field, d.GetIndexStateFailReason())
			}
		}
		return false, nil
	})
}

// loadCollection loads a collection and waits until it is fully loaded.
func (a *adminClient) loadCollection(ctx context.Context, name string) error {
	st, err := a.svc.LoadCollection(ctx, &milvuspb.LoadCollectionRequest{CollectionName: name})
	if err := checkRPC(st, err); err != nil {
		return err
	}
	return poll(ctx, func() (bool, error) {
		resp, err := a.svc.GetLoadingProgress(ctx, &milvuspb.GetLoadingProgressRequest{CollectionName: name})
		if err := checkRPC(resp, err); err != nil {
			return false, err
		}
		return resp.GetProgress() >= 100, nil
	})
}

func (a *adminClient) loadState(ctx context.Context, name string) (commonpb.LoadState, error) {
	resp, err := a.svc.GetLoadState(ctx, &milvuspb.GetLoadStateRequest{CollectionName: name})
	if err := checkRPC(resp, err); err != nil {
		return 0, err
	}
	return resp.GetState(), nil
}

// insert writes the columns as rows. A column the collection has no field
// for becomes a dynamic key.
func (a *adminClient) insert(ctx context.Context, coll string, cols ...column.Column) error {
	desc, err := a.svc.DescribeCollection(ctx, &milvuspb.DescribeCollectionRequest{CollectionName: coll})
	if err := checkRPC(desc, err); err != nil {
		return err
	}
	fields := map[string]*schemapb.FieldSchema{}
	for _, f := range desc.GetSchema().GetFields() {
		fields[f.GetName()] = f
	}
	var (
		data    []*schemapb.FieldData
		dynamic []column.Column
		rows    int
	)
	for _, c := range cols {
		rows = c.Len()
		f, ok := fields[c.Name()]
		if !ok {
			dynamic = append(dynamic, c)
			continue
		}
		if f.GetNullable() {
			c.SetNullable(true)
		}
		c.CompactNullableValues()
		data = append(data, c.FieldData())
	}
	if len(dynamic) > 0 {
		vals := make([][]byte, rows)
		for i := range vals {
			m := map[string]any{}
			for _, c := range dynamic {
				m[c.Name()], _ = c.Get(i)
			}
			if vals[i], err = json.Marshal(m); err != nil {
				return err
			}
		}
		data = append(data, &schemapb.FieldData{
			Type:      schemapb.DataType_JSON,
			IsDynamic: true,
			Field: &schemapb.FieldData_Scalars{Scalars: &schemapb.ScalarField{
				Data: &schemapb.ScalarField_JsonData{JsonData: &schemapb.JSONArray{Data: vals}},
			}},
		})
	}
	resp, err := a.svc.Insert(ctx, &milvuspb.InsertRequest{
		CollectionName:  coll,
		FieldsData:      data,
		NumRows:         uint32(rows),
		SchemaTimestamp: desc.GetUpdateTimestamp(),
	})
	return checkRPC(resp, err)
}

// poll runs f every 100 ms until it reports done, fails or ctx ends.
func poll(ctx context.Context, f func() (bool, error)) error {
	for {
		done, err := f()
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
