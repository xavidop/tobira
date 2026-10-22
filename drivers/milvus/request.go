package milvus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"

	"github.com/xavidop/tobira/driver"
)

// The query ops: the only requests Query sends. Each one is a read.
const (
	opQuery  = "query"
	opGet    = "get"
	opCount  = "count"
	opSearch = "search"
)

// opMembers is the allow-list: the ops Query runs and the members each one
// takes. Anything else is refused before a byte is sent.
var opMembers = map[string][]string{
	opQuery:  {"op", "collection", "database", "filter", "params", "outputFields", "limit", "offset", "partitions", "consistency"},
	opGet:    {"op", "collection", "database", "ids", "outputFields", "partitions", "consistency"},
	opCount:  {"op", "collection", "database", "filter", "params", "partitions", "consistency"},
	opSearch: {"op", "collection", "database", "filter", "params", "outputFields", "limit", "offset", "partitions", "consistency", "annsField", "vector", "text", "searchParams", "rerank"},
}

// localRerankers are the function rerankers that run inside Milvus, each
// with the params a read-only request may give it (matched ignoring
// case). Any other reranker (model rerankers call Cohere, Voyage, TEI,
// vLLM…) sends the query and the documents to a provider: egress, refused
// on a read-only request, and so is any other param, which could name an
// endpoint or a provider.
var localRerankers = map[string][]string{
	"decay": {"reranker", "function", "origin", "scale", "offset", "decay"},
	"boost": {"reranker", "filter", "weight", "random_score"},
}

// Bounds on the work one request may ask for.
const (
	defaultK = 10   // search/Similar results when none is given
	maxK     = 1000 // cap on search/Similar results and get ids
	pageSize = 1000 // entities per Query RPC when streaming
)

// request is a parsed native query.
type request struct {
	Op           string                     `json:"op"`
	Collection   string                     `json:"collection"`
	Database     string                     `json:"database"`
	Filter       string                     `json:"filter"`
	Params       map[string]json.RawMessage `json:"params"`
	OutputFields []string                   `json:"outputFields"`
	Limit        *int64                     `json:"limit"`
	Offset       *int64                     `json:"offset"`
	Partitions   []string                   `json:"partitions"`
	Consistency  string                     `json:"consistency"`
	IDs          []json.RawMessage          `json:"ids"`
	AnnsField    string                     `json:"annsField"`
	Vector       []float32                  `json:"vector"`
	Text         string                     `json:"text"`
	SearchParams map[string]json.RawMessage `json:"searchParams"`
	Rerank       *rerankSpec                `json:"rerank"`

	params      exprParams
	consistency commonpb.ConsistencyLevel
	ids         []driver.Value
}

// rerankSpec is one function reranker: the fields it reads and its
// parameters ("reranker" names it).
type rerankSpec struct {
	InputFields []string     `json:"inputFields"`
	Params      rerankParams `json:"params"`
}

// UnmarshalJSON refuses a member given twice ("params" or "inputFields"),
// which encoding/json would resolve to the last one, and any unknown
// member: the request decoder's DisallowUnknownFields does not reach a
// custom unmarshaler.
func (r *rerankSpec) UnmarshalJSON(b []byte) error {
	if t := bytes.TrimSpace(b); len(t) == 0 || t[0] != '{' {
		return errors.New(`"rerank" must be a JSON object`)
	}
	fields, err := topLevelObject(string(b))
	if err != nil {
		return fmt.Errorf("rerank: %w", err)
	}
	for k := range fields {
		if k != "inputFields" && k != "params" {
			return fmt.Errorf("rerank takes no %q (it takes inputFields, params)", k)
		}
	}
	type plain rerankSpec
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("rerank: %w", err)
	}
	*r = rerankSpec(p)
	return nil
}

// rerankParams are a reranker's params. A key given twice is refused:
// encoding/json would keep the last one, so the guard and the request
// could read different values.
type rerankParams map[string]json.RawMessage

func (p *rerankParams) UnmarshalJSON(b []byte) error {
	fields, err := topLevelObject(string(b))
	if err != nil {
		return fmt.Errorf("rerank params: %w", err)
	}
	*p = fields
	return nil
}

// check refuses params that two spellings could make ambiguous: keys
// equal but for case, and "reranker" spelt any other way.
func (p rerankParams) check() error {
	seen := map[string]string{}
	for k := range p {
		lk := strings.ToLower(k)
		if lk == "reranker" && k != "reranker" {
			return fmt.Errorf(`rerank param %q must be spelt "reranker"`, k)
		}
		if other, dup := seen[lk]; dup {
			return fmt.Errorf("rerank params %q and %q differ only in case", other, k)
		}
		seen[lk] = k
	}
	return nil
}

// checkReadOnly refuses a reranker that does not run inside Milvus, or a
// param outside its read-only allow-list; a boost filter passes the
// filter guard.
func (r *rerankSpec) checkReadOnly() error {
	name := r.reranker()
	allowed, ok := localRerankers[name]
	if !ok {
		return fmt.Errorf("read-only: the %q reranker calls an outside model provider (egress); only decay and boost rerankers run in read-only mode", name)
	}
	for k, raw := range r.Params {
		if !slices.Contains(allowed, strings.ToLower(k)) {
			return fmt.Errorf("read-only: rerank param %q is not one the %s reranker takes in read-only mode (%s)", k, name, strings.Join(allowed, ", "))
		}
		if strings.EqualFold(k, "filter") {
			var f string
			if err := json.Unmarshal(raw, &f); err != nil {
				return errors.New("read-only: the boost filter must be a string")
			}
			if err := checkFilter(f, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// reranker is the rerank function's kind, lower-cased.
func (r *rerankSpec) reranker() string {
	var name string
	if raw, ok := r.Params["reranker"]; ok {
		_ = json.Unmarshal(raw, &name)
	}
	return strings.ToLower(name)
}

// parseRequest parses a native query: one JSON object with an "op"
// (query, get, count or search; inferred when absent: search with a vector
// or text, get with ids, else query), a "collection" and the op's own
// members. With readOnly every refusal is ErrReadOnly, so a text the guard
// cannot read is never let through (fail closed); otherwise a disallowed
// op is ErrUnsupported (Query never writes: edits go through the Mutator)
// and a malformed text ErrSyntax.
func parseRequest(text string, readOnly bool) (*request, error) {
	refuse := func(kind error, format string, args ...any) (*request, error) {
		msg := fmt.Sprintf(format, args...)
		if readOnly && kind != driver.ErrReadOnly {
			kind, msg = driver.ErrReadOnly, "read-only: "+msg
		}
		return nil, driver.Wrap(kind, Name, "query", errors.New(msg))
	}
	fields, err := topLevelObject(text)
	if err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	op := ""
	if raw, ok := fields["op"]; ok {
		if err := json.Unmarshal(raw, &op); err != nil || bytes.TrimSpace(raw)[0] != '"' {
			return refuse(driver.ErrSyntax, `"op" must be a string`)
		}
	} else {
		_, vec := fields["vector"]
		_, txt := fields["text"]
		_, ids := fields["ids"]
		switch {
		case vec || txt:
			op = opSearch
		case ids:
			op = opGet
		default:
			op = opQuery
		}
	}
	allowed, ok := opMembers[op]
	if !ok {
		if readOnly {
			return refuse(driver.ErrReadOnly, "read-only: op %q is refused; only query, get, count and search run", op)
		}
		return refuse(driver.ErrUnsupported, "op %q is not a query op: only query, get, count and search run (edit entities with the editor)", op)
	}
	for k := range fields {
		if !slices.Contains(allowed, k) {
			return refuse(driver.ErrSyntax, "the %s op takes no %q (it takes %s)", op, k, strings.Join(allowed[1:], ", "))
		}
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	r := &request{}
	if err := dec.Decode(r); err != nil {
		return refuse(driver.ErrSyntax, "invalid %s request: %v", op, err)
	}
	r.Op = op
	if err := r.validate(); err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	if r.Rerank != nil && readOnly {
		if err := r.Rerank.checkReadOnly(); err != nil {
			if errors.Is(err, driver.ErrReadOnly) {
				return nil, err
			}
			return refuse(driver.ErrReadOnly, "%v", err)
		}
	}
	if err := checkFilter(r.Filter, readOnly); err != nil {
		return nil, err
	}
	return r, nil
}

// validate checks the members' values and fills the derived fields.
func (r *request) validate() error {
	if r.Collection == "" {
		return errors.New(`the request needs a "collection"`)
	}
	if r.Database != "" && !dbName.MatchString(r.Database) {
		return errors.New(`"database" is a database name`)
	}
	if r.Limit != nil && *r.Limit < 0 {
		return errors.New(`"limit" must be 0 or more`)
	}
	if r.Offset != nil && *r.Offset < 0 {
		return errors.New(`"offset" must be 0 or more`)
	}
	switch strings.ToLower(r.Consistency) {
	case "", "strong":
		r.consistency = commonpb.ConsistencyLevel_Strong
	case "bounded":
		r.consistency = commonpb.ConsistencyLevel_Bounded
	case "session":
		r.consistency = commonpb.ConsistencyLevel_Session
	case "eventually":
		r.consistency = commonpb.ConsistencyLevel_Eventually
	default:
		return errors.New(`"consistency" is strong, bounded, session or eventually`)
	}
	r.params = exprParams{}
	for name, raw := range r.Params {
		if !identifier.MatchString(name) || strings.HasPrefix(name, "tobira_") {
			return fmt.Errorf("parameter name %q must be letters, digits and underscores, not starting with tobira_", name)
		}
		tv, err := jsonTemplateValue(raw)
		if err != nil {
			return fmt.Errorf("parameter %q: %v", name, err)
		}
		r.params[name] = tv
	}
	switch r.Op {
	case opGet:
		if len(r.IDs) == 0 {
			return errors.New(`get needs "ids"`)
		}
		if len(r.IDs) > maxK {
			return fmt.Errorf("get takes at most %d ids", maxK)
		}
		kind := driver.TypeNull
		for _, raw := range r.IDs {
			v, err := idValue(raw)
			if err != nil {
				return err
			}
			if kind != driver.TypeNull && v.Type() != kind {
				return errors.New(`"ids" are all integers or all strings`)
			}
			kind = v.Type()
			r.ids = append(r.ids, v)
		}
	case opSearch:
		if r.Vector == nil && r.Text == "" {
			return errors.New(`search needs a "vector" or a "text"`)
		}
		if r.Vector != nil && r.Text != "" {
			return errors.New(`give a "vector" or a "text", not both`)
		}
		if r.Vector != nil && len(r.Vector) == 0 {
			return errors.New(`"vector" is empty`)
		}
		if r.Rerank != nil {
			if err := r.Rerank.Params.check(); err != nil {
				return err
			}
			if r.Rerank.reranker() == "" {
				return errors.New(`"rerank" needs "params" with a "reranker"`)
			}
		}
	}
	return nil
}

// idValue reads one get id: an integer or a string.
func idValue(raw json.RawMessage) (driver.Value, error) {
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return driver.Null(), err
		}
		return driver.String(s), nil
	}
	n, err := strconv.ParseInt(string(t), 10, 64)
	if err != nil {
		return driver.Null(), errors.New(`"ids" are integers or strings`)
	}
	return driver.Int(n), nil
}

// addPositional binds Query.Params as {p1}, {p2}…: a name the request's own
// "params" already uses is refused.
func (r *request) addPositional(vals []driver.Value) error {
	for i, v := range vals {
		name := "p" + strconv.Itoa(i+1)
		if _, dup := r.params[name]; dup {
			return driver.Wrap(driver.ErrSyntax, Name, "query", fmt.Errorf("parameter %q is given twice (Query.Params binds {p1}, {p2}…)", name))
		}
		tv, err := templateValue(v)
		if err != nil {
			return err
		}
		r.params[name] = tv
	}
	return nil
}

// topLevelObject decodes text as exactly one JSON object and returns its
// members. A duplicate key is an error: encoding/json would keep the last
// one, so {"op":"query","op":"delete"} must not be read as either.
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
