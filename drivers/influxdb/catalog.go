package influxdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// maxCollections caps a Collections listing.
const maxCollections = 10_000

// internalDatabase is InfluxDB 3's own database, hidden from Namespaces.
const internalDatabase = "_internal"

// Namespaces lists the databases through the HTTP API: InfluxDB 3's Flight
// API has no SHOW DATABASES.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	body, err := c.get(ctx, "namespaces", "/api/v3/configure/database?format=json")
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, driver.Wrap(driver.ErrConnection, Name, "namespaces", fmt.Errorf("unexpected database list: %w", err))
	}
	out := make([]driver.Namespace, 0, len(rows))
	for _, r := range rows {
		name, _ := r["iox::database"].(string)
		if name == "" || name == internalDatabase {
			continue
		}
		out = append(out, driver.Namespace{Name: name, Label: "database", Default: name == c.set.database})
	}
	slices.SortFunc(out, func(a, b driver.Namespace) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// collect runs a catalog statement and reads at most max rows.
func (c *conn) collect(ctx context.Context, op, db, sql string, params map[string]any, max int) ([]driver.Row, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	rs, err := c.run(ctx, op, sql, langSQL, db, params, max)
	if err != nil {
		return nil, err
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	return rows, err
}

// Collections lists the tables (measurements) of a database, at most
// maxCollections of them, by name.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	if err := c.check(ctx, "collections"); err != nil {
		return nil, err
	}
	rows, err := c.collect(ctx, "collections", ns.Name,
		"SELECT table_name FROM information_schema.tables WHERE table_schema = 'iox' ORDER BY table_name", nil, maxCollections)
	if err != nil {
		return nil, err
	}
	db := ns.Name
	if db == "" {
		db = c.set.database
	}
	out := make([]driver.Collection, 0, len(rows))
	for _, r := range rows {
		name, _ := r[0].AsText()
		out = append(out, driver.Collection{Ref: driver.CollectionRef{Namespace: db, Name: name}, Type: driver.CollectionMeasurement, RowEstimate: -1})
	}
	return out, nil
}

// describeTable reads a table's columns from information_schema.
func (c *conn) describeTable(ctx context.Context, op string, ref driver.CollectionRef) (*table, error) {
	rows, err := c.collect(ctx, op, ref.Namespace,
		"SELECT column_name, data_type, is_nullable FROM information_schema.columns"+
			" WHERE table_schema = 'iox' AND table_name = $1 ORDER BY ordinal_position",
		map[string]any{"1": ref.Name}, 0)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("table %q not found", ref.Name))
	}
	t := &table{ref: ref}
	for _, r := range rows {
		name, _ := r[0].AsText()
		native, _ := r[1].AsText()
		nullable, _ := r[2].AsText()
		role := "field"
		switch {
		case name == "time":
			role = "timestamp"
		case strings.HasPrefix(native, "Dictionary("):
			role = "tag"
		}
		t.fields = append(t.fields, driver.Field{
			Name: name, NativeType: native, Type: fieldType(native), Nullable: nullable == "YES", Comment: role,
		})
	}
	return t, nil
}

// Describe reports a table's columns. Comment says each column's role:
// tag, field or timestamp. The primary key is the series key: the tags,
// then time.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if err := c.check(ctx, "describe"); err != nil {
		return nil, err
	}
	if ref.Name == "" {
		return nil, driver.Wrap(driver.ErrNotFound, Name, "describe", errors.New("no table named"))
	}
	t, err := c.describeTable(ctx, "describe", ref)
	if err != nil {
		return nil, err
	}
	pk := t.tags()
	if slices.ContainsFunc(t.fields, func(f driver.Field) bool { return f.Name == "time" }) {
		pk = append(pk, "time")
	}
	return &driver.CollectionSchema{
		Ref: ref, Type: driver.CollectionMeasurement, Fields: t.fields, PrimaryKey: pk,
		RowEstimate: -1, SizeBytes: -1, Comment: "measurement",
	}, nil
}

// Browse pages through a table newest first (see buildBrowse).
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	if err := c.check(ctx, "browse"); err != nil {
		return nil, err
	}
	t, err := c.describeTable(ctx, "browse", ref)
	if err != nil {
		return nil, err
	}
	st, err := buildBrowse(t, opts)
	if err != nil {
		return nil, err
	}
	rs, err := c.run(ctx, "browse", st.sql, langSQL, ref.Namespace, st.params, st.limit)
	if err != nil {
		return nil, err
	}
	rs.nextCursor = cursorAt(st.offset + st.limit)
	return rs, nil
}

// maxRangePoints caps the rows of one Range call; past it the result is
// cut and Stats.Truncated is set.
const maxRangePoints = 100_000

// Range reads a field over time, raw or aggregated per date_bin window
// (see buildRange), at most maxRangePoints rows.
func (c *conn) Range(ctx context.Context, ref driver.CollectionRef, q driver.RangeQuery) (driver.ResultSet, error) {
	if err := c.check(ctx, "range"); err != nil {
		return nil, err
	}
	t, err := c.describeTable(ctx, "range", ref)
	if err != nil {
		return nil, err
	}
	st, err := buildRange(t, q)
	if err != nil {
		return nil, err
	}
	return c.run(ctx, "range", st.sql, langSQL, ref.Namespace, st.params, c.rangeCap)
}
