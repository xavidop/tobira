package redis

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// defaultDatabases is the database count when CONFIG GET databases is
// refused (managed services, ACLs).
const defaultDatabases = 16

// Namespaces lists the logical databases 0..N-1, N from CONFIG GET
// databases read at Open (16 when refused, at most nsCap). The comment holds the key
// count from INFO keyspace.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	cl, _, release, err := c.client("namespaces", "")
	if err != nil {
		return nil, err
	}
	defer release()
	n := defaultDatabases
	if c.dbCount > 0 {
		n = c.dbLimit()
	}
	keys := map[string]string{}
	if info, err := cl.Info(ctx, "keyspace").Result(); err == nil {
		for line := range strings.SplitSeq(info, "\n") {
			db, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || !strings.HasPrefix(db, "db") {
				continue
			}
			for kv := range strings.SplitSeq(rest, ",") {
				if k, v, _ := strings.Cut(kv, "="); k == "keys" {
					keys[strings.TrimPrefix(db, "db")] = v
				}
			}
		}
	} else if ctx.Err() != nil {
		return nil, mapError(ctx, "namespaces", ctx.Err())
	}
	out := make([]driver.Namespace, n)
	for i := range n {
		name := itoa(i)
		comment := "empty"
		if k, ok := keys[name]; ok {
			comment = k + " keys"
		}
		out[i] = driver.Namespace{Name: name, Label: "db", Default: i == c.defaultDB, Comment: comment}
	}
	return out, nil
}

// Collections lists "*" (every key) and the key prefixes found by
// SCAN-sampling up to catalogSample keys (see prefixCollections). Row
// estimates are exact only when the sample covered the whole database.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	cl, _, release, err := c.client("collections", ns.Name)
	if err != nil {
		return nil, err
	}
	defer release()
	keys, complete, err := sampleKeys(ctx, cl, allKeys, catalogSample, scanTimeBudget)
	if err != nil {
		return nil, mapError(ctx, "collections", err)
	}
	total, err := cl.DBSize(ctx).Result()
	if err != nil {
		return nil, mapError(ctx, "collections", err)
	}
	types, err := keyTypes(ctx, cl, keys)
	if err != nil {
		return nil, mapError(ctx, "collections", err)
	}
	out := []driver.Collection{{Ref: driver.CollectionRef{Namespace: ns.Name, Name: allKeys}, Type: driver.CollectionKeyPattern, RowEstimate: total, Comment: "every key"}}
	out = append(out, prefixCollections(ns.Name, keys, types, complete)...)
	return out, nil
}

// maxPrefixes caps the prefixes Collections lists. Collection names reach
// the AI assistant as schema even under send_rows: never, and on Redis they
// are key prefixes, that is data: the cap bounds how much of it a listing
// carries.
const maxPrefixes = 500

// prefixCollections groups the sampled keys (with their TYPE) into prefix
// collections, split at the first ':'. A key without a ':' is left in "*"
// only: its name is the whole key, row data such as a session token or an
// e-mail address, so it never names a collection, nor joins the collection
// of the same name (inCollection agrees); nor does a key starting with ":".
// A prefix whose sampled keys are all hashes is a collection of records;
// any other prefix is a key pattern. Only the first maxPrefixes prefixes (sorted) are listed; the last
// listed one's Comment says how many more the sample held.
func prefixCollections(ns string, keys, types []string, complete bool) []driver.Collection {
	type stat struct {
		count   int64
		allHash bool
	}
	stats := map[string]*stat{}
	for i, k := range keys {
		p, _, ok := strings.Cut(k, ":")
		if !ok || p == "" || p == allKeys || types[i] == "none" {
			continue
		}
		s, ok := stats[p]
		if !ok {
			s = &stat{allHash: true}
			stats[p] = s
		}
		s.count++
		s.allHash = s.allHash && types[i] == "hash"
	}
	names := slices.Sorted(maps.Keys(stats))
	more := len(names) - maxPrefixes
	if more > 0 {
		names = names[:maxPrefixes]
	}
	out := make([]driver.Collection, 0, len(names))
	for _, p := range names {
		s := stats[p]
		col := driver.Collection{Ref: driver.CollectionRef{Namespace: ns, Name: p}, Type: driver.CollectionKeyPattern, RowEstimate: -1, Comment: p + ":*"}
		if s.allHash {
			col.Type = driver.CollectionCollection
			col.Comment = "hash records " + p + ":*"
		}
		if complete {
			col.RowEstimate = s.count
		}
		out = append(out, col)
	}
	if more > 0 {
		out[len(out)-1].Comment += fmt.Sprintf(" (… %d more prefixes not listed; browse * for every key)", more)
	}
	return out
}

// keyTypes returns TYPE for each key, pipelined in chunks.
func keyTypes(ctx context.Context, cl *goredis.Client, keys []string) ([]string, error) {
	out := make([]string, len(keys))
	for start := 0; start < len(keys); start += scanCount {
		end := min(start+scanCount, len(keys))
		cmds := make([]*goredis.StatusCmd, end-start)
		if _, err := cl.Pipelined(ctx, func(p goredis.Pipeliner) error {
			for i, k := range keys[start:end] {
				cmds[i] = p.Type(ctx, k)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		for i, cmd := range cmds {
			out[start+i] = cmd.Val()
		}
	}
	return out, nil
}

// Describe samples up to describeSample keys of the collection (within
// scanTimeBudget). Every collection has the fields _key, type, ttl and
// value; a collection of hashes adds the hash fields, typed by inference,
// and its primary key is the field that mirrors the key suffix when there
// is one. A prefix with no keys is ErrNotFound.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	cl, _, release, err := c.client("describe", ref.Namespace)
	if err != nil {
		return nil, err
	}
	defer release()
	if ref.Name == "" {
		return nil, driver.Wrap(driver.ErrNotFound, Name, "describe", errors.New("empty collection name"))
	}
	rec, keys, complete, err := sampleRecords(ctx, cl, ref.Name)
	if err != nil {
		return nil, mapError(ctx, "describe", err)
	}
	if len(keys) == 0 && ref.Name != allKeys {
		why := "no key matches " + ref.Name + ":*"
		if !complete {
			why += " in the sampled part of the keyspace"
		}
		return nil, driver.Wrap(driver.ErrNotFound, Name, "describe", errors.New(why))
	}
	rows, err := fetchRows(ctx, cl, keys, rec)
	if err != nil {
		return nil, mapError(ctx, "describe", err)
	}
	sample := make([]driver.Row, 0, len(rows))
	for _, r := range rows {
		if r != nil {
			sample = append(sample, r)
		}
	}
	sch := &driver.CollectionSchema{
		Ref:         ref,
		Type:        driver.CollectionKeyPattern,
		Fields:      collectionFields(rec),
		PrimaryKey:  primaryKey(rec),
		RowEstimate: -1,
		SizeBytes:   -1,
		Schemaless:  true,
		Sample:      sample,
		Comment:     fmt.Sprintf("inferred from %d sampled keys", len(sample)),
	}
	if rec != nil {
		sch.Type = driver.CollectionCollection
	}
	switch {
	case ref.Name == allKeys:
		if n, err := cl.DBSize(ctx).Result(); err == nil {
			sch.RowEstimate = n
		}
	case complete:
		sch.RowEstimate = int64(len(keys))
	}
	return sch, nil
}
