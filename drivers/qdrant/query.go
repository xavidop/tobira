package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	pb "github.com/qdrant/go-client/qdrant"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/xavidop/tobira/driver"
)

// The query ops: the only requests Query sends. Each one is a read RPC.
const (
	opScroll   = "scroll"
	opSearch   = "search"
	opQuery    = "query"
	opCount    = "count"
	opRetrieve = "retrieve"
)

// Bounds on the work one call may ask for.
const (
	defaultK      = 10    // search/query/Similar results when none is given
	maxK          = 1000  // cap on search/query/Similar results and retrieve ids
	scrollPage    = 256   // points per scroll RPC
	defaultBrowse = 100   // Browse page when Limit is 0
	maxBrowse     = 1000  // cap on a Browse page
	maxOffset     = 10000 // cap on a search/query offset (the server walks past it)
)

// newRequest returns an empty request message for an allowed op, or nil.
// This switch is the allow-list: every message is a read request.
func newRequest(op string) proto.Message {
	switch op {
	case opScroll:
		return &pb.ScrollPoints{}
	case opSearch:
		return &pb.SearchPoints{}
	case opQuery:
		return &pb.QueryPoints{}
	case opCount:
		return &pb.CountPoints{}
	case opRetrieve:
		return &pb.GetPoints{}
	}
	return nil
}

// request is a parsed native query.
type request struct {
	op  string
	msg proto.Message
}

func (r request) collection() string {
	if m, ok := r.msg.(interface{ GetCollectionName() string }); ok {
		return m.GetCollectionName()
	}
	return ""
}

// parseRequest parses a native query: a JSON object with an "op" (scroll,
// search, query, count or retrieve), the collection as "collection" (or
// "collection_name") and the rest of the op's gRPC request message in its
// JSON form. Anything else is refused before a byte is sent. With readOnly
// every refusal is ErrReadOnly, so a text the guard cannot read is never
// let through (fail closed); otherwise a disallowed op is ErrUnsupported
// (Query never writes: edits go through the Mutator) and a malformed text
// ErrSyntax.
func parseRequest(text string, readOnly bool) (request, error) {
	refuse := func(kind error, format string, args ...any) (request, error) {
		msg := fmt.Sprintf(format, args...)
		if readOnly && kind != driver.ErrReadOnly {
			kind, msg = driver.ErrReadOnly, "read-only: "+msg
		}
		return request{}, driver.Wrap(kind, Name, "query", errors.New(msg))
	}
	fields, err := topLevelObject(text)
	if err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	rawOp, ok := fields["op"]
	if !ok {
		return refuse(driver.ErrSyntax, `the request needs an "op": one of scroll, search, query, count or retrieve`)
	}
	var op string
	if err := json.Unmarshal(rawOp, &op); err != nil || bytes.TrimSpace(rawOp)[0] != '"' {
		return refuse(driver.ErrSyntax, `"op" must be a string`)
	}
	msg := newRequest(op)
	if msg == nil {
		if readOnly {
			return refuse(driver.ErrReadOnly, "read-only: op %q is refused; only scroll, search, query, count and retrieve run", op)
		}
		return refuse(driver.ErrUnsupported, "op %q is not a query op: only scroll, search, query, count and retrieve run (edit points with the editor)", op)
	}
	delete(fields, "op")
	if c, ok := fields["collection"]; ok {
		if _, dup := fields["collection_name"]; dup {
			return refuse(driver.ErrSyntax, `give "collection" or "collection_name", not both`)
		}
		if _, dup := fields["collectionName"]; dup {
			return refuse(driver.ErrSyntax, `give "collection" or "collectionName", not both`)
		}
		delete(fields, "collection")
		fields["collection_name"] = c
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	if err := protojson.Unmarshal(body, msg); err != nil {
		return refuse(driver.ErrSyntax, "invalid %s request: %v", op, err)
	}
	if readOnly {
		if kind := inferenceInput(msg.ProtoReflect()); kind != "" {
			return refuse(driver.ErrReadOnly, "read-only: server-side inference (a %s vector input) is refused; search by a vector, or by text through tobira's embedder", kind)
		}
	}
	req := request{op: op, msg: msg}
	if req.collection() == "" {
		return request{}, driver.Wrap(driver.ErrSyntax, Name, "query", fmt.Errorf(`the %s request needs a "collection"`, op))
	}
	return req, nil
}

// inferenceTypes are the vector inputs that ask the server to run an
// embedding model, keyed by message name: on a server set up for inference
// (Qdrant Cloud Inference, for one) they send data to a model and cost
// money, so a read-only request may not carry one.
var inferenceTypes = map[protoreflect.FullName]string{
	(&pb.Document{}).ProtoReflect().Descriptor().FullName():        "document",
	(&pb.Image{}).ProtoReflect().Descriptor().FullName():           "image",
	(&pb.InferenceObject{}).ProtoReflect().Descriptor().FullName(): "object",
}

// inferenceInput walks every populated field of m, at any depth (prefetch,
// recommend, discover, context and so on), and names the first inference
// input it finds, or returns "".
func inferenceInput(m protoreflect.Message) string {
	if kind, ok := inferenceTypes[m.Descriptor().FullName()]; ok {
		return kind
	}
	found := ""
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
			return true
		}
		switch {
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len() && found == ""; i++ {
				found = inferenceInput(l.Get(i).Message())
			}
		case fd.IsMap():
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					found = inferenceInput(mv.Message())
					return found == ""
				})
			}
		default:
			found = inferenceInput(v.Message())
		}
		return found == ""
	})
	return found
}

// topLevelObject decodes text as exactly one JSON object and returns its
// members. A duplicate key is an error: encoding/json would keep the last
// one, so {"op":"scroll","op":"upsert"} must not be read as either.
func topLevelObject(text string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	tok, err := dec.Token()
	if err != nil {
		return nil, errors.New("the request must be a JSON object")
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("the request must be a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		key, _ := tok.(string)
		if _, dup := fields[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		fields[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("the request must be one JSON object, with nothing after it")
	}
	return fields, nil
}

// Query runs a native request (see parseRequest). It never writes.
func (c *conn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	start := time.Now()
	if err := c.check(ctx, "query"); err != nil {
		return nil, err
	}
	readOnly := c.readOnly || q.ReadOnly
	req, err := parseRequest(q.Text, readOnly)
	if err != nil {
		return nil, err
	}
	if len(q.Params) > 0 {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("qdrant requests take no parameters: put values in the JSON"))
	}
	if err := checkNamespace("query", q.Namespace); err != nil {
		return nil, err
	}
	switch m := req.msg.(type) {
	case *pb.CountPoints:
		resp, err := c.client.GetPointsClient().Count(ctx, m)
		if err != nil {
			return nil, mapError(ctx, "query", err)
		}
		cols := []driver.Column{{Name: "count", NativeType: "uint64", Type: driver.TypeInt}}
		rows := []driver.Row{{driver.Int(int64(min(resp.GetResult().GetCount(), 1<<63-1)))}}
		return driver.NewSliceResultSet(cols, rows, driver.Stats{RowsAffected: -1, Duration: time.Since(start)}), nil
	case *pb.ScrollPoints:
		return c.queryScroll(ctx, m, q.Limit, start)
	}
	schema, err := c.getSchema(ctx, "query", req.collection())
	if err != nil {
		return nil, err
	}
	var (
		pts         []point
		withVectors bool
		score       string
		warnings    []string
	)
	switch m := req.msg.(type) {
	case *pb.SearchPoints:
		warnings = capRequest(m)
		m.Limit, warnings = clampK(m.Limit, warnings)
		if m.WithPayload == nil {
			m.WithPayload = pb.NewWithPayload(true)
		}
		resp, err := c.client.GetPointsClient().Search(ctx, m) //nolint:staticcheck // the search op is part of the native language; Qdrant 1.x still serves it (query supersedes it)
		if err != nil {
			return nil, mapError(ctx, "query", err)
		}
		pts, withVectors, score = scoredPoints(resp.GetResult()), wantsVectors(m.GetWithVectors()), scoreColumn
	case *pb.QueryPoints:
		warnings = capRequest(m)
		k := uint64(defaultK)
		if m.Limit != nil {
			k = m.GetLimit()
		}
		k, warnings = clampK(k, warnings)
		m.Limit = &k
		if m.WithPayload == nil {
			m.WithPayload = pb.NewWithPayload(true)
		}
		resp, err := c.client.GetPointsClient().Query(ctx, m)
		if err != nil {
			return nil, mapError(ctx, "query", err)
		}
		pts, withVectors, score = scoredPoints(resp.GetResult()), wantsVectors(m.GetWithVectors()), scoreColumn
	case *pb.GetPoints:
		if len(m.GetIds()) > maxK {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "query", fmt.Errorf("retrieve takes at most %d ids", maxK))
		}
		if m.WithPayload == nil {
			m.WithPayload = pb.NewWithPayload(true)
		}
		resp, err := c.client.GetPointsClient().Get(ctx, m)
		if err != nil {
			return nil, mapError(ctx, "query", err)
		}
		pts, withVectors = retrievedPoints(resp.GetResult()), wantsVectors(m.GetWithVectors())
	}
	sh := newShape(schema, pts, withVectors, score, func(s float32) float64 { return float64(s) })
	rows := make([]driver.Row, len(pts))
	for i, p := range pts {
		rows[i], _ = sh.row(p)
	}
	rs := newPagedResultSet(sh.columns(pts), rows, nil, q.Limit, start)
	rs.stats.Warnings = warnings
	return rs, nil
}

// clampK applies maxK to a requested result count, adding a warning when it
// cuts.
func clampK(k uint64, warnings []string) (uint64, []string) {
	if k > maxK {
		return maxK, append(warnings, fmt.Sprintf("limit %d is above the driver's cap: returning at most %d points", k, maxK))
	}
	if k == 0 {
		return defaultK, warnings
	}
	return k, warnings
}

// capRequest bounds the server work of a search or query beyond its result
// limit: the offset is cut to maxOffset and every prefetch limit, at any
// depth, to maxK. It returns one warning per cut.
func capRequest(msg proto.Message) []string {
	var warnings []string
	capOffset := func(o *uint64) {
		if o != nil && *o > maxOffset {
			warnings = append(warnings, fmt.Sprintf("offset %d is above the driver's cap: using %d", *o, maxOffset))
			*o = maxOffset
		}
	}
	var capPrefetch func(ps []*pb.PrefetchQuery)
	capPrefetch = func(ps []*pb.PrefetchQuery) {
		for _, p := range ps {
			if p.Limit != nil && *p.Limit > maxK {
				warnings = append(warnings, fmt.Sprintf("prefetch limit %d is above the driver's cap: using %d", *p.Limit, maxK))
				*p.Limit = maxK
			}
			capPrefetch(p.GetPrefetch())
		}
	}
	switch m := msg.(type) {
	case *pb.SearchPoints:
		capOffset(m.Offset)
	case *pb.QueryPoints:
		capOffset(m.Offset)
		capPrefetch(m.GetPrefetch())
	}
	return warnings
}

// wantsVectors reports whether a with_vectors selector asks for vectors.
func wantsVectors(s *pb.WithVectorsSelector) bool {
	switch o := s.GetSelectorOptions().(type) {
	case *pb.WithVectorsSelector_Enable:
		return o.Enable
	case *pb.WithVectorsSelector_Include:
		return len(o.Include.GetNames()) > 0
	}
	return false
}

// queryScroll streams a scroll: the request's limit, when given, caps the
// total, and the points come in pages of scrollPage. Columns are fixed by
// the first page; payload keys first seen later are reported in a warning.
// A scroll with order_by returns no next offset, so it is one page of at
// most maxK points.
func (c *conn) queryScroll(ctx context.Context, m *pb.ScrollPoints, limit int, start time.Time) (driver.ResultSet, error) {
	schema, err := c.getSchema(ctx, "query", m.GetCollectionName())
	if err != nil {
		return nil, err
	}
	if m.WithPayload == nil {
		m.WithPayload = pb.NewWithPayload(true)
	}
	var warnings []string
	remaining := uint64(1<<63 - 1) // the request's own cap
	if m.Limit != nil {
		remaining = uint64(m.GetLimit())
	}
	if m.OrderBy != nil {
		if m.Limit == nil {
			remaining = defaultK
		}
		remaining, warnings = clampK(remaining, warnings)
	}
	page := func(ctx context.Context) ([]point, bool, error) {
		n := min(remaining, scrollPage)
		if m.OrderBy != nil {
			n = remaining
		}
		if limit > 0 {
			n = min(n, uint64(limit)+1) // one extra row proves truncation
		}
		n32 := uint32(n)
		m.Limit = &n32
		resp, err := c.client.GetPointsClient().Scroll(ctx, m)
		if err != nil {
			return nil, false, mapError(ctx, "query", err)
		}
		pts := retrievedPoints(resp.GetResult())
		remaining -= min(remaining, uint64(len(pts)))
		next := resp.GetNextPageOffset()
		done := next == nil || remaining == 0 || m.OrderBy != nil || len(pts) == 0
		m.Offset = next
		return pts, done, nil
	}
	if remaining == 0 {
		sh := newShape(schema, nil, wantsVectors(m.GetWithVectors()), "", nil)
		return newPagedResultSet(sh.columns(nil), nil, nil, limit, start), nil
	}
	first, done, err := page(ctx)
	if err != nil {
		return nil, err
	}
	sh := newShape(schema, first, wantsVectors(m.GetWithVectors()), "", nil)
	dropped := droppedKeys{}
	toRows := func(pts []point) []driver.Row {
		rows := make([]driver.Row, len(pts))
		for i, p := range pts {
			var d []string
			rows[i], d = sh.row(p)
			dropped.add(d)
		}
		return rows
	}
	var fetch fetchFunc
	if !done {
		fetch = func(ctx context.Context) ([]driver.Row, bool, error) {
			pts, done, err := page(ctx)
			if err != nil {
				return nil, false, err
			}
			return toRows(pts), done, nil
		}
	}
	rs := newPagedResultSet(sh.columns(first), toRows(first), fetch, limit, start)
	rs.stats.Warnings = warnings
	rs.warn = dropped.warnings
	return rs, nil
}

// checkNamespace accepts the default namespace or none.
func checkNamespace(op, ns string) error {
	if ns == "" || ns == defaultNamespace {
		return nil
	}
	return driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("namespace %q does not exist: qdrant has only %q", ns, defaultNamespace))
}
