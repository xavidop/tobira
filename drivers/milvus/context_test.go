package milvus

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"

	"github.com/xavidop/tobira/driver"
)

// Describe does not swallow the caller's cancellation while
// it samples the dynamic field.
func TestDescribeSampleKeepsCancellation(t *testing.T) {
	c, fake := newFakeConn(t, false)
	fake.describe = describeOK(peopleProto())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.query = func(*milvuspb.QueryRequest) (*milvuspb.QueryResults, error) {
		cancel()
		time.Sleep(50 * time.Millisecond)
		return &milvuspb.QueryResults{Status: okStatus()}, nil
	}
	_, err := c.Describe(ctx, driver.CollectionRef{Name: "people"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Describe cancelled during sampling = %v, want context.Canceled", err)
	}
}

// The dbname header follows the request's database.
func TestDBHeaderPerCall(t *testing.T) {
	c, fake := newFakeConn(t, false)
	fake.describe = describeOK(peopleProto())
	for _, ns := range []string{"", "db2"} {
		_, _ = c.getSchema(context.Background(), "describe", c.db(ns), "people")
	}
	fake.mu.Lock()
	got := slices.Clone(fake.dbHeaders)
	fake.mu.Unlock()
	if !slices.Equal(got, []string{"default", "db2"}) {
		t.Errorf("dbname headers = %v, want [default db2]", got)
	}
}
