package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/xavidop/tobira/ai/tools"
)

// schemaTemplate is the URI template of the schema resources.
const schemaTemplate = "tobira://{connection}/{namespace}/{collection}/schema"

// errNotFound is the error of a schema URI that names nothing readable.
var errNotFound = errors.New("not found")

// errInternal is what a read that panicked returns.
var errInternal = errors.New("internal error: reading the resource failed")

// addSchemaResource registers the schema resource template. It is served
// through mcp-go directly: Genkit's resources would only add a second URI
// matcher in front of the same describe call.
func (s *srv) addSchemaResource() {
	s.mcp.AddResourceTemplate(mcp.NewResourceTemplate(schemaTemplate, "collection schema",
		mcp.WithTemplateDescription("A collection's schema as the describe tool returns it: fields and types, primary key, indexes, foreign keys, row estimate and comment. Never the DDL."),
		mcp.WithTemplateMIMEType("application/json"),
	), s.readSchema)
}

// notFoundURI is what a schema read that names nothing readable is
// rewritten to before mcp-go routes it. It matches no resource, so mcp-go
// answers MCP's resource-not-found (-32002), with one text for every such
// read: a hidden connection reads exactly like an unknown one. mcp-go has
// no other way to choose the error code of a read (a handler's error is
// always -32603).
const notFoundURI = "tobira:not-found"

// schemaURIPattern is the template's own matcher, so the parser accepts
// exactly the URIs mcp-go routes to readSchema.
var schemaURIPattern = mcp.NewResourceTemplate(schemaTemplate, "collection schema").URITemplate.Regexp()

// beforeReadResource classifies a resources/read before mcp-go routes it
// (its before-read hook may change the request). It keeps no state: a read
// that names nothing readable is recorded once, as failed, and rewritten
// to notFoundURI; any other read is left to readSchema, which reads and
// records it.
func (s *srv) beforeReadResource(ctx context.Context, _ any, req *mcp.ReadResourceRequest) {
	defer s.recoverHook("read resource")
	if req == nil {
		return
	}
	uri := req.Params.URI
	start := time.Now()
	in, err := s.describeSchema(ctx, uri)
	if !errors.Is(err, errNotFound) {
		return
	}
	req.Params.URI = notFoundURI
	s.record(s.callContext(ctx, nil), in.Connection, "resource "+uri, false, time.Since(start))
}

// readSchema serves a schema read and records it once, ok or failed.
func (s *srv) readSchema(ctx context.Context, req mcp.ReadResourceRequest) (contents []mcp.ResourceContents, err error) {
	start := time.Now()
	uri := req.Params.URI
	var in tools.DescribeInput
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("mcp resource read panicked", "panic", fmt.Sprint(r))
			contents, err = nil, errInternal
		}
		s.record(s.callContext(ctx, nil), in.Connection, "resource "+uri, err == nil, time.Since(start))
	}()
	var out tools.DescribeOutput
	if in, out, err = s.describeURI(ctx, uri); err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, errInternal
	}
	return []mcp.ResourceContents{mcp.TextResourceContents{URI: uri, MIMEType: "application/json", Text: string(b)}}, nil
}

// describeSchema is describeURI without the result.
func (s *srv) describeSchema(ctx context.Context, uri string) (tools.DescribeInput, error) {
	in, _, err := s.describeURI(ctx, uri)
	return in, err
}

// describeURI describes the collection uri names, through the Toolset (so
// redaction and exposure are the describe tool's), recording nothing. Any
// failure to find it wraps errNotFound; a non-exposed connection reads
// exactly like an unknown one. Other errors are the request's
// cancellation.
func (s *srv) describeURI(ctx context.Context, uri string) (tools.DescribeInput, tools.DescribeOutput, error) {
	in, parsed := parseSchemaURI(uri)
	if !parsed {
		return in, tools.DescribeOutput{}, errNotFound
	}
	out, err := s.ts.Describe(s.callContext(ctx, nil), in)
	if err != nil {
		return in, out, err
	}
	if out.Error != "" {
		return in, out, fmt.Errorf("%w: %s", errNotFound, out.Error)
	}
	return in, out, nil
}

// parseSchemaURI splits tobira://<connection>/<namespace>/<collection>/schema
// into a describe input, unescaping each segment. It accepts only what the
// template matches. An empty namespace means the connection's default.
func parseSchemaURI(uri string) (tools.DescribeInput, bool) {
	rest, ok := strings.CutPrefix(uri, "tobira://")
	if !ok || !schemaURIPattern.MatchString(uri) {
		return tools.DescribeInput{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[3] != "schema" {
		return tools.DescribeInput{}, false
	}
	for i := range 3 {
		v, err := url.PathUnescape(parts[i])
		if err != nil {
			return tools.DescribeInput{}, false
		}
		parts[i] = v
	}
	if parts[0] == "" || parts[2] == "" {
		return tools.DescribeInput{}, false
	}
	return tools.DescribeInput{Connection: parts[0], Namespace: parts[1], Collection: parts[2]}, true
}
