package redis

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// The key-value columns every collection shows.
const (
	colKey   = "_key"
	colType  = "type"
	colTTL   = "ttl"
	colValue = "value"
	// colTruncated is true when the row's value preview was cut.
	colTruncated = "_truncated"
)

// redisJSONType is what TYPE reports for a RedisJSON document.
const redisJSONType = "ReJSON-RL"

// recordSchema describes a collection whose sampled keys are all hashes:
// the hash fields are shown as typed columns after the key-value ones.
type recordSchema struct {
	fields  []driver.Field
	idField string // the field mirroring the key suffix, "" if none
}

func (s *recordSchema) fieldNames() []string {
	if s == nil {
		return nil
	}
	names := make([]string, len(s.fields))
	for i, f := range s.fields {
		names[i] = f.Name
	}
	return names
}

// baseFields are the key-value fields of every collection.
func baseFields() []driver.Field {
	return []driver.Field{
		{Name: colKey, NativeType: "key", Type: driver.TypeString},
		{Name: colType, NativeType: "type", Type: driver.TypeString},
		{Name: colTTL, NativeType: "ttl (seconds, -1 = no expiry)", Type: driver.TypeInt},
		{Name: colValue, NativeType: "any", Type: driver.TypeUnknown, Nullable: true},
		{Name: colTruncated, NativeType: "bool (the value preview was cut)", Type: driver.TypeBool, Generated: true},
	}
}

// collectionFields are the fields of a collection: the key-value ones, then
// the record fields.
func collectionFields(rec *recordSchema) []driver.Field {
	fs := baseFields()
	if rec != nil {
		fs = append(fs, rec.fields...)
	}
	return fs
}

// primaryKey is the record's id field when it has one, else _key.
func primaryKey(rec *recordSchema) []string {
	if rec != nil && rec.idField != "" {
		return []string{rec.idField}
	}
	return []string{colKey}
}

// columnsFor turns fields into result columns attributed to ref.
func columnsFor(ref driver.CollectionRef, fields []driver.Field, pk []string) []driver.Column {
	src := ref
	cols := make([]driver.Column, len(fields))
	for i, f := range fields {
		cols[i] = driver.Column{Name: f.Name, NativeType: f.NativeType, Type: f.Type, Source: &src, Key: slices.Contains(pk, f.Name)}
	}
	return cols
}

// fetchRows reads one row per key, in order: _key, type, ttl, a value
// preview, _truncated, then the record fields (HMGET). A key that vanished since it was
// scanned yields a nil row. Two pipelined round trips per call.
func fetchRows(ctx context.Context, cl *goredis.Client, keys []string, rec *recordSchema) ([]driver.Row, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	types := make([]*goredis.StatusCmd, len(keys))
	ttls := make([]*goredis.DurationCmd, len(keys))
	_, err := cl.Pipelined(ctx, func(p goredis.Pipeliner) error {
		for i, k := range keys {
			types[i] = p.Type(ctx, k)
			ttls[i] = p.TTL(ctx, k)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	names := rec.fieldNames()
	previews := make([]func() (driver.Value, bool), len(keys))
	fields := make([]*goredis.SliceCmd, len(keys))
	_, err = cl.Pipelined(ctx, func(p goredis.Pipeliner) error {
		for i, k := range keys {
			typ := types[i].Val()
			if typ == "none" {
				continue
			}
			previews[i] = queuePreview(ctx, p, k, typ)
			if len(names) > 0 && typ == "hash" {
				fields[i] = p.HMGet(ctx, k, names...)
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, goredis.Nil) {
		var re goredis.Error
		if !errors.As(err, &re) { // a per-key error reply (WRONGTYPE after a race) is shown in place
			return nil, err
		}
	}
	rows := make([]driver.Row, len(keys))
	for i, k := range keys {
		if previews[i] == nil {
			continue
		}
		preview, cut := previews[i]()
		row := make(driver.Row, 0, 5+len(names))
		row = append(row, textValue(k), driver.String(types[i].Val()), driver.Int(ttlSeconds(ttls[i].Val())), preview, driver.Bool(cut))
		for j := range names {
			v := driver.Null()
			if fields[i] != nil {
				if vals := fields[i].Val(); j < len(vals) {
					if s, ok := vals[j].(string); ok {
						v = typedValue(s, rec.fields[j].Type)
					}
				}
			}
			row = append(row, v)
		}
		rows[i] = row
	}
	return rows, nil
}

// ttlSeconds converts go-redis's TTL reply, which keeps the -1 (no expiry)
// and -2 (no such key) codes as raw durations.
func ttlSeconds(d time.Duration) int64 {
	if d < 0 {
		return int64(d)
	}
	return int64(d / time.Second)
}

// queuePreview queues the commands that read a bounded preview of key and
// returns a function that builds the value once the pipeline ran, with
// whether the preview was cut (the real value is longer: more than
// previewBytes of a string, previewItems elements of a collection, or
// maxJSONPreview bytes of a JSON document).
func queuePreview(ctx context.Context, p goredis.Pipeliner, key, typ string) func() (driver.Value, bool) {
	switch typ {
	case "string":
		get := p.GetRange(ctx, key, 0, previewBytes-1)
		n := p.StrLen(ctx, key)
		return func() (driver.Value, bool) {
			cut := n.Val() > previewBytes
			return previewString(get.Val(), cut), cut
		}
	case "hash":
		cmd := p.HScan(ctx, key, 0, "", previewItems)
		n := p.HLen(ctx, key)
		return func() (driver.Value, bool) {
			kv, _ := cmd.Val()
			ms := make([]driver.Member, 0, len(kv)/2)
			for i := 0; i+1 < len(kv) && len(ms) < previewItems; i += 2 {
				ms = append(ms, driver.Member{Key: kv[i], Value: textValue(kv[i+1])})
			}
			return driver.ObjectValue(ms...), n.Val() > int64(len(ms))
		}
	case "list":
		cmd := p.LRange(ctx, key, 0, previewItems-1)
		n := p.LLen(ctx, key)
		return func() (driver.Value, bool) {
			items := cmd.Val()
			return stringList(items), n.Val() > int64(len(items))
		}
	case "set":
		cmd := p.SScan(ctx, key, 0, "", previewItems)
		n := p.SCard(ctx, key)
		return func() (driver.Value, bool) {
			ms, _ := cmd.Val()
			ms = slices.Clone(ms)
			slices.Sort(ms)
			ms = slices.Compact(ms)
			if len(ms) > previewItems {
				ms = ms[:previewItems]
			}
			return stringList(ms), n.Val() > int64(len(ms))
		}
	case "zset":
		cmd := p.ZRangeWithScores(ctx, key, 0, previewItems-1)
		n := p.ZCard(ctx, key)
		return func() (driver.Value, bool) {
			zs := cmd.Val()
			vs := make([]driver.Value, len(zs))
			for i, z := range zs {
				m, _ := z.Member.(string)
				vs[i] = driver.ObjectValue(driver.Member{Key: "member", Value: textValue(m)}, driver.Member{Key: "score", Value: driver.Float(z.Score)})
			}
			return driver.List(vs...), n.Val() > int64(len(zs))
		}
	case "stream":
		cmd := p.XRangeN(ctx, key, "-", "+", previewItems)
		n := p.XLen(ctx, key)
		return func() (driver.Value, bool) {
			msgs := cmd.Val()
			vs := make([]driver.Value, len(msgs))
			for i, m := range msgs {
				ms := make([]driver.Member, 0, len(m.Values))
				for _, f := range sortedKeys(m.Values) {
					ms = append(ms, driver.Member{Key: f, Value: replyValue(m.Values[f])})
				}
				vs[i] = driver.ObjectValue(driver.Member{Key: "id", Value: driver.String(m.ID)}, driver.Member{Key: "fields", Value: driver.ObjectValue(ms...)})
			}
			return driver.List(vs...), n.Val() > int64(len(msgs))
		}
	case redisJSONType:
		cmd := p.Do(ctx, "JSON.GET", key)
		return func() (driver.Value, bool) {
			s, err := cmd.Text()
			switch {
			case err != nil:
				return driver.Null(), false
			case len(s) > maxJSONPreview:
				return driver.String(s[:maxJSONPreview] + "…"), true
			}
			return driver.JSON([]byte(s)), false
		}
	}
	return func() (driver.Value, bool) { return driver.Unknown(typ, nil, "<"+typ+">"), false }
}

// previewString shows the first previewBytes of a string. When the value was
// cut, a rune split by the cut is dropped and "…" marks the cut; a value
// that is not text stays Bytes.
func previewString(s string, cut bool) driver.Value {
	if !cut {
		return textValue(s)
	}
	if utf8.ValidString(s) {
		return driver.String(s + "…")
	}
	for k := 1; k < utf8.UTFMax && k < len(s); k++ { // drop a rune split by the cut
		if t := s[:len(s)-k]; utf8.RuneStart(s[len(s)-k]) && utf8.ValidString(t) && !utf8.FullRuneInString(s[len(s)-k:]) {
			return driver.String(t + "…")
		}
	}
	return driver.Bytes([]byte(s))
}

func stringList(ss []string) driver.Value {
	vs := make([]driver.Value, len(ss))
	for i, s := range ss {
		vs[i] = textValue(s)
	}
	return driver.List(vs...)
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// sampleRecords decides whether the sampled keys of a collection are all
// hashes and, if so, infers the record schema from them. It returns the
// schema (nil when not a record collection), the sampled keys, their types,
// and whether the sample covered the whole keyspace.
func sampleRecords(ctx context.Context, cl *goredis.Client, coll string) (*recordSchema, []string, bool, error) {
	keys, complete, err := sampleKeys(ctx, cl, coll, describeSample, scanTimeBudget)
	if err != nil || len(keys) == 0 || coll == allKeys {
		return nil, keys, complete, err
	}
	types := make([]*goredis.StatusCmd, len(keys))
	if _, err := cl.Pipelined(ctx, func(p goredis.Pipeliner) error {
		for i, k := range keys {
			types[i] = p.Type(ctx, k)
		}
		return nil
	}); err != nil {
		return nil, nil, false, err
	}
	var hashKeys []string
	for i, k := range keys {
		switch types[i].Val() {
		case "hash":
			hashKeys = append(hashKeys, k)
		case "none":
		default:
			return nil, keys, complete, nil
		}
	}
	if len(hashKeys) == 0 {
		return nil, keys, complete, nil
	}
	scans := make([]*goredis.ScanCmd, len(hashKeys))
	if _, err := cl.Pipelined(ctx, func(p goredis.Pipeliner) error {
		for i, k := range hashKeys {
			scans[i] = p.HScan(ctx, k, 0, "", scanCount)
		}
		return nil
	}); err != nil && !errors.Is(err, goredis.Nil) {
		return nil, nil, false, err
	}
	var names []string
	seen := map[string]bool{}
	recs := make([]map[string]string, len(hashKeys))
	for i := range hashKeys {
		kv, _ := scans[i].Val()
		recs[i] = make(map[string]string, len(kv)/2)
		for j := 0; j+1 < len(kv); j += 2 {
			recs[i][kv[j]] = kv[j+1]
			if !seen[kv[j]] {
				seen[kv[j]] = true
				names = append(names, kv[j])
			}
		}
	}
	if len(names) > maxRecordFields {
		return nil, keys, complete, nil
	}
	for _, n := range names {
		switch n {
		case colKey, colType, colTTL, colValue, colTruncated:
			return nil, keys, complete, nil // would shadow a key-value column
		}
	}
	return &recordSchema{fields: inferFields(names, recs), idField: detectIDField(coll, names, hashKeys, recs)}, keys, complete, nil
}

// itoa is strconv.Itoa, kept short for namespace names.
func itoa(n int) string { return strconv.Itoa(n) }
