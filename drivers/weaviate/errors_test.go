package weaviate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/weaviate/weaviate-go-client/v5/weaviate/fault"
	"github.com/weaviate/weaviate/entities/models"

	"github.com/xavidop/tobira/driver"
)

func peopleClassOneVector() *models.Class {
	return &models.Class{Class: "P", VectorIndexType: "hnsw", Vectorizer: "none"}
}

func TestMapError(t *testing.T) {
	status := func(code int, body string) error {
		return &fault.WeaviateClientError{IsUnexpectedStatusCode: true, StatusCode: code, Msg: body}
	}
	for _, tt := range []struct {
		err  error
		kind error
		msg  string
	}{
		{status(401, `{"code":401,"message":"unauthorized: invalid api key"}`), driver.ErrAuth, "unauthorized: invalid api key"},
		{status(403, `{"error":[{"message":"forbidden"}]}`), driver.ErrPermission, "forbidden"},
		{status(404, ``), driver.ErrNotFound, "not found"},
		{status(422, `{"error":[{"message":"id '00000000-0000-0000-0000-000000000001' already exists"}]}`), driver.ErrConflict, "already exists"},
		{status(422, `{"error":[{"message":"invalid object: no such prop"}]}`), driver.ErrSyntax, "invalid object: no such prop"},
		{status(400, `bad`), driver.ErrSyntax, "bad"},
		{status(503, `down`), driver.ErrConnection, "down"},
		{status(500, `boom`), nil, "boom"},
		{&fault.WeaviateClientError{StatusCode: -1, DerivedFromError: &net.OpError{Op: "dial", Err: errors.New("refused")}}, driver.ErrConnection, "refused"},
	} {
		err := mapError(context.Background(), "query", tt.err)
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Fatalf("mapError(%v) = %v, want a *driver.Error", tt.err, err)
		}
		if tt.kind != nil && !errors.Is(err, tt.kind) || tt.kind == nil && driver.KindOf(err) != nil {
			t.Errorf("mapError(%v) kind = %v, want %v", tt.err, driver.KindOf(err), tt.kind)
		}
		if got := de.Err.Error(); got != tt.msg && !containsFold(got, tt.msg) {
			t.Errorf("mapError(%v) message = %q, want %q", tt.err, got, tt.msg)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mapError(ctx, "query", &fault.WeaviateClientError{StatusCode: -1, DerivedFromError: fmt.Errorf("Post: %w", context.Canceled)})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestGraphQLErrorKinds(t *testing.T) {
	for _, tt := range []struct {
		msg  string
		kind error
	}{
		{`Cannot query field "nope" on type "Article".`, driver.ErrSyntax},
		{`Syntax Error GraphQL request (1:3) Unexpected Name "x"`, driver.ErrSyntax},
		{`Unknown argument "foo" on field "Article"`, driver.ErrSyntax},
		{`explorer: list class: search: object search at index article: no such prop`, nil},
	} {
		err := graphQLError("query", []string{tt.msg})
		if tt.kind != nil && !errors.Is(err, tt.kind) || tt.kind == nil && driver.KindOf(err) != nil {
			t.Errorf("graphQLError(%q) kind = %v, want %v", tt.msg, driver.KindOf(err), tt.kind)
		}
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func TestWrongVectorSizeIsSyntax(t *testing.T) {
	err := statusError("update", 500, []byte(`{"error":[{"message":"new node has a vector with length 1. Existing nodes have vectors with length 4: vector dimensions do not match the index dimensions"}]}`))
	if !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("wrong vector size = %v", err)
	}
	if err := statusError("update", 500, []byte(`boom`)); driver.KindOf(err) != nil {
		t.Errorf("plain 500 = %v", err)
	}
}
