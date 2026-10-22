package qdrant

import (
	"strings"
	"testing"

	pb "github.com/qdrant/go-client/qdrant"
)

// TestCapQuery: search and query offsets, and every prefetch limit at any
// depth, are cut to the caps with a warning.
func TestCapQuery(t *testing.T) {
	req, err := parseRequest(`{"op":"query","collection":"c","offset":"999999999","limit":"5",
		"prefetch":[{"limit":"99999999","prefetch":[{"limit":"5000"},{"limit":"3"}]}]}`, true)
	if err != nil {
		t.Fatal(err)
	}
	q := req.msg.(*pb.QueryPoints)
	warns := capRequest(req.msg)
	if q.GetOffset() != maxOffset || q.GetPrefetch()[0].GetLimit() != maxK ||
		q.GetPrefetch()[0].GetPrefetch()[0].GetLimit() != maxK || q.GetPrefetch()[0].GetPrefetch()[1].GetLimit() != 3 {
		t.Errorf("capped query = %v", q)
	}
	if len(warns) != 3 || !strings.Contains(strings.Join(warns, " "), "offset") {
		t.Errorf("warnings = %q", warns)
	}
	req, err = parseRequest(`{"op":"search","collection":"c","vector":[1],"limit":"5","offset":"20000"}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if capRequest(req.msg); req.msg.(*pb.SearchPoints).GetOffset() != maxOffset {
		t.Errorf("search offset = %d", req.msg.(*pb.SearchPoints).GetOffset())
	}
	req, _ = parseRequest(`{"op":"query","collection":"c","offset":"10","prefetch":[{"limit":"7"}]}`, true)
	if w := capRequest(req.msg); len(w) != 0 {
		t.Errorf("in-bounds request warned: %q", w)
	}
}
