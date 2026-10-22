package cassandra

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

// schemaTTL is how long a Describe answer is reused. Browse and the
// Mutator describe their table on every call, so a short cache saves
// three system_schema reads per page or edit; Invalidate (the host calls
// it after DDL and edits), a DDL statement run through Query and every
// Mutator write drop it.
const schemaTTL = 5 * time.Second

// schemaCache remembers Describe answers per table for a TTL. Errors are
// never cached, an answer loaded before an invalidate is not stored, and
// callers get their own copy. It is safe for concurrent use.
type schemaCache struct {
	ttl time.Duration
	now func() time.Time

	mu  sync.Mutex
	gen uint64
	m   map[driver.CollectionRef]schemaEntry
}

type schemaEntry struct {
	at  time.Time
	sch *driver.CollectionSchema
}

func newSchemaCache(ttl time.Duration) *schemaCache {
	return &schemaCache{ttl: ttl, now: time.Now, m: map[driver.CollectionRef]schemaEntry{}}
}

func (s *schemaCache) get(ctx context.Context, ref driver.CollectionRef, load func(context.Context) (*driver.CollectionSchema, error)) (*driver.CollectionSchema, error) {
	s.mu.Lock()
	if e, ok := s.m[ref]; ok && s.now().Sub(e.at) < s.ttl {
		s.mu.Unlock()
		return cloneSchema(e.sch), nil
	}
	gen := s.gen
	s.mu.Unlock()
	sch, err := load(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.gen == gen {
		s.m[ref] = schemaEntry{at: s.now(), sch: sch}
	}
	s.mu.Unlock()
	return cloneSchema(sch), nil
}

// invalidate drops every cached answer and every load in flight.
func (s *schemaCache) invalidate() {
	s.mu.Lock()
	s.gen++
	clear(s.m)
	s.mu.Unlock()
}

// cloneSchema copies the slices a caller might modify.
func cloneSchema(in *driver.CollectionSchema) *driver.CollectionSchema {
	out := *in
	out.Fields = slices.Clone(in.Fields)
	out.PrimaryKey = slices.Clone(in.PrimaryKey)
	out.PartitionKey = slices.Clone(in.PartitionKey)
	out.ClusteringKey = slices.Clone(in.ClusteringKey)
	out.Indexes = slices.Clone(in.Indexes)
	for i := range out.Indexes {
		out.Indexes[i].Fields = slices.Clone(in.Indexes[i].Fields)
	}
	return &out
}
