package elasticsearch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestHTTPError(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		kind   error
		code   string
		msg    string
	}{
		{404, `{"error":{"root_cause":[],"type":"index_not_found_exception","reason":"no such index [nope]"},"status":404}`, driver.ErrNotFound, "index_not_found_exception", "no such index [nope]"},
		{401, `{"error":{"type":"security_exception","reason":"unable to authenticate user [elastic]"},"status":401}`, driver.ErrAuth, "security_exception", "unable to authenticate"},
		{403, `{"error":{"type":"security_exception","reason":"action [indices:data/write/delete] is unauthorized"},"status":403}`, driver.ErrPermission, "security_exception", "unauthorized"},
		{409, `{"error":{"type":"version_conflict_engine_exception","reason":"[1]: version conflict"},"status":409}`, driver.ErrConflict, "version_conflict_engine_exception", "version conflict"},
		{400, `{"error":{"root_cause":[{"type":"parsing_exception","reason":"unknown query [nope]"}],"type":"search_phase_execution_exception","reason":"all shards failed"},"status":400}`, driver.ErrSyntax, "parsing_exception", "unknown query [nope]"},
		{400, `{"error":{"type":"illegal_argument_exception","reason":"bad"},"status":400}`, driver.ErrSyntax, "illegal_argument_exception", "bad"},
		{404, `{"_index":"x","_id":"1","result":"not_found"}`, driver.ErrNotFound, "", "not found"},
		{503, `{"error":{"type":"cluster_block_exception","reason":"blocked"},"status":503}`, driver.ErrConnection, "cluster_block_exception", "blocked"},
		{405, `{"error":"Incorrect HTTP method for uri [/x/_delete_by_query] and method [GET], allowed: [POST]","status":405}`, driver.ErrUnsupported, "", "Incorrect HTTP method"},
		{500, `<html>oops</html>`, nil, "", "HTTP 500"},
		{404, `{"error":{"type":"search_context_missing_exception","reason":"No search context found for id [1]"},"status":404}`, driver.ErrNotFound, "search_context_missing_exception", "point in time expired"},
	} {
		err := httpError(Name, "query", tc.status, []byte(tc.body))
		var de *driver.Error
		if !errors.As(err, &de) {
			t.Fatalf("httpError(%d) = %T", tc.status, err)
		}
		if tc.kind != nil && !errors.Is(err, tc.kind) || tc.kind == nil && de.Kind != nil {
			t.Errorf("httpError(%d, %s): kind %v, want %v", tc.status, tc.body, de.Kind, tc.kind)
		}
		if de.Code != tc.code || de.Driver != Name || de.Op != "query" {
			t.Errorf("httpError(%d): code %q driver %q op %q", tc.status, de.Code, de.Driver, de.Op)
		}
		if !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("httpError(%d): message %q lacks %q", tc.status, err.Error(), tc.msg)
		}
	}
}

func TestMapErrorContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mapError(ctx, Name, "query", fmt.Errorf("net/http: request canceled"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled call: want context.Canceled, got %v", err)
	}
	err = mapError(context.Background(), Name, "ping", errors.New(unknownProductMsg))
	if !errors.Is(err, driver.ErrConnection) || !strings.Contains(err.Error(), "opensearch://") {
		t.Errorf("the product check: want ErrConnection with a hint, got %v", err)
	}
	err = mapError(context.Background(), Name, "ping", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"))
	if !errors.Is(err, driver.ErrConnection) {
		t.Errorf("a refused dial: want ErrConnection, got %v", err)
	}
	pass := &driver.Error{Kind: driver.ErrAuth, Driver: Name, Op: "x", Err: errors.New("y")}
	if got := mapError(context.Background(), Name, "z", pass); got != error(pass) {
		t.Errorf("a driver.Error is not passed through: %v", got)
	}
}
