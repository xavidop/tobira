package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Explain validates a search and shows how the server rewrites its query
// (_validate/query with explain and rewrite), which never runs it. With
// Analyze it runs the search with the profile API and returns the profile
// as JSON: a search only reads, so Analyze is allowed on a read-only path
// too. The search form and raw _search and _count requests can be
// explained; anything else is ErrUnsupported. The read-only guard runs
// first, as in Query.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	req, err := parseRequestAs(c.name(), q.Text, q.ReadOnly || c.readOnly, c.allowSystem)
	if err != nil {
		return nil, err
	}
	target, body, err := c.explainTarget(op, req)
	if err != nil {
		return nil, err
	}
	if opts.Analyze {
		return c.profile(ctx, op, req, target, body)
	}
	query := body["query"]
	if len(query) == 0 {
		query = json.RawMessage(`{"match_all":{}}`)
	}
	vb, _ := json.Marshal(map[string]json.RawMessage{"query": query})
	p := pathOf("_validate", "query")
	if target != "" {
		p = pathOf(target, "_validate", "query")
	}
	data, err := c.do(ctx, op, http.MethodPost, p, url.Values{"explain": {"true"}, "rewrite": {"true"}}, vb)
	if err != nil {
		return nil, err
	}
	var r struct {
		Valid        bool `json:"valid"`
		Explanations []struct {
			Index       string `json:"index"`
			Valid       bool   `json:"valid"`
			Explanation string `json:"explanation"`
			Error       string `json:"error"`
		} `json:"explanations"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, c.errorf(nil, op, "unexpected _validate response")
	}
	var b strings.Builder
	for _, e := range r.Explanations {
		switch {
		case !e.Valid:
			fmt.Fprintf(&b, "%s: invalid: %s\n", e.Index, e.Error)
		default:
			fmt.Fprintf(&b, "%s: %s\n", e.Index, e.Explanation)
		}
	}
	if !r.Valid {
		msg := strings.TrimSpace(b.String())
		if msg == "" {
			msg = r.Error
		}
		return nil, c.errorf(driver.ErrSyntax, op, "invalid query: %s", msg)
	}
	if b.Len() == 0 {
		b.WriteString("valid\n")
	}
	return &driver.Plan{Format: "text", Text: b.String()}, nil
}

// explainTarget returns the index and search body a request explains.
func (c *conn) explainTarget(op string, req request) (string, map[string]json.RawMessage, error) {
	if !req.raw {
		return req.index, req.body, nil
	}
	segs := req.segs
	target := ""
	if len(segs) == 2 && !strings.HasPrefix(segs[0], "_") || len(segs) == 2 && segs[0] == "_all" {
		target, segs = segs[0], segs[1:]
	}
	if len(segs) != 1 || (segs[0] != "_search" && segs[0] != "_count") || req.ndjson {
		return "", nil, c.errorf(driver.ErrUnsupported, op, "only searches and counts can be explained, not %s %s", req.method, req.path)
	}
	body := map[string]json.RawMessage{}
	if len(req.rawBody) > 0 {
		if err := json.Unmarshal(req.rawBody, &body); err != nil {
			return "", nil, c.errorf(driver.ErrSyntax, op, "invalid body: %v", err)
		}
	}
	return target, body, nil
}

// profile runs a search with "profile": true and returns the profile.
func (c *conn) profile(ctx context.Context, op string, req request, target string, body map[string]json.RawMessage) (*driver.Plan, error) {
	if req.raw && req.segs[len(req.segs)-1] != "_search" {
		return nil, c.errorf(driver.ErrUnsupported, op, "only a search can be profiled")
	}
	pb := make(map[string]json.RawMessage, len(body)+1)
	for k, v := range body {
		pb[k] = v
	}
	pb["profile"] = json.RawMessage("true")
	if _, ok := pb["size"]; !ok {
		pb["size"] = json.RawMessage("10")
	}
	data, _ := json.Marshal(pb)
	p := pathOf("_search")
	if target != "" {
		p = pathOf(target, "_search")
	}
	raw, err := c.do(ctx, op, http.MethodPost, p, nil, data)
	if err != nil {
		return nil, err
	}
	var r struct {
		Profile json.RawMessage `json:"profile"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Profile) == 0 {
		return nil, c.errorf(nil, op, "the response has no profile")
	}
	var out bytes.Buffer
	if err := json.Indent(&out, r.Profile, "", "  "); err != nil {
		return nil, c.errorf(nil, op, "unexpected profile")
	}
	return &driver.Plan{Format: "json", Text: out.String()}, nil
}
