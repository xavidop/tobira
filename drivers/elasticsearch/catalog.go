package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Namespaces returns the single namespace, default.
func (c *conn) Namespaces(ctx context.Context) ([]driver.Namespace, error) {
	if err := c.check(ctx, "namespaces"); err != nil {
		return nil, err
	}
	return []driver.Namespace{{Name: defaultNamespace, Label: "cluster", Default: true}}, nil
}

// hiddenName reports a system or hidden-by-convention name.
func hiddenName(s string) bool { return strings.HasPrefix(s, ".") }

// Collections lists the open and closed indexes (CollectionIndex), the
// data streams (CollectionIndex, whose backing indexes are hidden) and the
// aliases (CollectionView), leaving out hidden ones and names starting
// with a dot. Data streams and aliases are skipped silently when the
// server or the credentials do not allow listing them.
func (c *conn) Collections(ctx context.Context, ns driver.NamespaceRef) ([]driver.Collection, error) {
	const op = "collections"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if ns.Name != "" && ns.Name != defaultNamespace {
		return nil, c.errorf(driver.ErrNotFound, op, "namespace %q does not exist (the only one is %q)", ns.Name, defaultNamespace)
	}
	data, err := c.do(ctx, op, http.MethodGet, pathOf("_cat", "indices"), url.Values{
		"format": {"json"}, "h": {"index,docs.count,status"}, "expand_wildcards": {"open,closed"},
	}, nil)
	if err != nil {
		return nil, err
	}
	var cat []struct {
		Index  string `json:"index"`
		Docs   string `json:"docs.count"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, c.errorf(nil, op, "unexpected _cat/indices response")
	}
	var out []driver.Collection
	for _, ix := range cat {
		if hiddenName(ix.Index) {
			continue
		}
		col := driver.Collection{Ref: driver.CollectionRef{Namespace: defaultNamespace, Name: ix.Index}, Type: driver.CollectionIndex, RowEstimate: -1}
		if n, err := strconv.ParseInt(ix.Docs, 10, 64); err == nil {
			col.RowEstimate = n
		}
		if ix.Status == "close" {
			col.Comment = "closed"
		}
		out = append(out, col)
	}
	out = append(out, c.dataStreams(ctx)...)
	out = append(out, c.aliases(ctx)...)
	slices.SortFunc(out, func(a, b driver.Collection) int { return strings.Compare(a.Ref.Name, b.Ref.Name) })
	return out, nil
}

// dataStreams lists the visible data streams, or none on any error.
func (c *conn) dataStreams(ctx context.Context) []driver.Collection {
	params := url.Values{"expand_wildcards": {"open"}}
	if c.fl == flavorOpenSearch {
		params = nil // OpenSearch refuses the parameter; it lists no hidden streams anyway
	}
	data, err := c.do(ctx, "collections", http.MethodGet, pathOf("_data_stream"), params, nil)
	if err != nil {
		return nil
	}
	var r struct {
		DataStreams []struct {
			Name   string `json:"name"`
			Hidden bool   `json:"hidden"`
		} `json:"data_streams"`
	}
	if json.Unmarshal(data, &r) != nil {
		return nil
	}
	var out []driver.Collection
	for _, ds := range r.DataStreams {
		if ds.Hidden || hiddenName(ds.Name) {
			continue
		}
		out = append(out, driver.Collection{Ref: driver.CollectionRef{Namespace: defaultNamespace, Name: ds.Name}, Type: driver.CollectionIndex, RowEstimate: -1, Comment: "data stream"})
	}
	return out
}

// aliases lists the visible aliases, or none on any error.
func (c *conn) aliases(ctx context.Context) []driver.Collection {
	data, err := c.do(ctx, "collections", http.MethodGet, pathOf("_alias"), nil, nil)
	if err != nil {
		return nil
	}
	var r map[string]struct {
		Aliases map[string]struct {
			Hidden bool `json:"is_hidden"`
		} `json:"aliases"`
	}
	if json.Unmarshal(data, &r) != nil {
		return nil
	}
	targets := map[string][]string{}
	for index, ix := range r {
		for name, a := range ix.Aliases {
			if a.Hidden || hiddenName(name) {
				continue
			}
			targets[name] = append(targets[name], index)
		}
	}
	var out []driver.Collection
	for name, idx := range targets {
		slices.Sort(idx)
		out = append(out, driver.Collection{Ref: driver.CollectionRef{Namespace: defaultNamespace, Name: name}, Type: driver.CollectionView, RowEstimate: -1, Comment: "alias of " + strings.Join(idx, ", ")})
	}
	return out
}

// Describe reads a collection's mapping: _id first (the primary key),
// then the mapped fields with their nested properties. An alias or data
// stream merges the mappings of its indexes. The document count comes
// from _count; the DDL is the mapping JSON.
func (c *conn) Describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	const op = "describe"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if !validTarget(ref.Name) || strings.ContainsAny(ref.Name, "*,") {
		return nil, c.errorf(driver.ErrNotFound, op, "no index %q", ref.Name)
	}
	data, err := c.do(ctx, op, http.MethodGet, pathOf(ref.Name, "_mapping"), nil, nil)
	if err != nil {
		return nil, err
	}
	m, indices, err := parseMappings(data)
	if err != nil {
		return nil, c.errorf(nil, op, "%v", err)
	}
	if len(indices) == 0 {
		return nil, c.errorf(driver.ErrNotFound, op, "no index %q", ref.Name)
	}
	sch := &driver.CollectionSchema{
		Ref:         driver.CollectionRef{Namespace: defaultNamespace, Name: ref.Name},
		Type:        driver.CollectionIndex,
		PrimaryKey:  []string{"_id"},
		RowEstimate: -1,
		SizeBytes:   -1,
	}
	if !slices.Equal(indices, []string{ref.Name}) {
		sch.Comment = "over " + strings.Join(indices, ", ")
		if !strings.HasPrefix(indices[0], ".ds-") {
			sch.Type = driver.CollectionView // an alias
		}
	}
	sch.Fields = append([]driver.Field{{Name: "_id", NativeType: "_id", Type: driver.TypeString}}, schemaFields(m)...)
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") == nil {
		sch.DDL = pretty.String()
	}
	if cnt, err := c.do(ctx, op, http.MethodGet, pathOf(ref.Name, "_count"), nil, nil); err == nil {
		var r struct {
			Count int64 `json:"count"`
		}
		if json.Unmarshal(cnt, &r) == nil {
			sch.RowEstimate = r.Count
		}
	} else if !errors.Is(err, driver.ErrPermission) {
		return nil, err
	}
	return sch, nil
}
