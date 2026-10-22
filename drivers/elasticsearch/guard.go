package elasticsearch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// request is a parsed native query, in one of two forms:
//
//   - the search form, {"index": "x", ...search body}: a _search on x;
//   - the raw form, {"method": "GET", "path": "/x/_count", "body": …}: any
//     REST call, limited on a read-only path to the read allow-list.
type request struct {
	raw bool

	// The search form.
	index string
	body  map[string]json.RawMessage // the search body, without "index"

	// The raw form.
	method  string
	path    string   // without the query string; validated, never escaped
	segs    []string // path segments
	params  url.Values
	rawBody []byte // JSON, or NDJSON when ndjson; nil without a body
	ndjson  bool
}

// errRefused builds the guard's refusal: ErrReadOnly on a read-only path,
// so a text the guard cannot read is refused rather than let through
// (fail closed), and kind otherwise.
func errRefused(name string, readOnly bool, kind error, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if readOnly {
		return driver.Wrap(driver.ErrReadOnly, name, "query", errors.New("refused: "+msg))
	}
	return driver.Wrap(kind, name, "query", errors.New(msg))
}

// parseRequest parses and checks a native query; see request. With
// readOnly, every refusal is ErrReadOnly: a raw request outside the read
// allow-list, any key containing "script" (anywhere, in any case), an
// inference key, a system or hidden index target (unless allowSystem), a
// duplicate key, or text that is not one JSON object. Nothing is sent
// before it returns.
func parseRequest(text string, readOnly bool) (request, error) {
	return parseRequestAs(Name, text, readOnly, false)
}

func parseRequestAs(name, text string, readOnly, allowSystem bool) (request, error) {
	refuse := func(kind error, format string, args ...any) (request, error) {
		return request{}, errRefused(name, readOnly, kind, format, args...)
	}
	data := []byte(strings.TrimSpace(text))
	sc, err := scanJSONFull(data)
	if err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	if len(data) == 0 || data[0] != '{' {
		return refuse(driver.ErrSyntax, `a query is a JSON object: {"index":"x","query":{…}} or {"method":"GET","path":"/x/_search","body":{…}}`)
	}
	if readOnly {
		if reason := sc.readOnlyReason(allowSystem); reason != "" {
			return refuse(driver.ErrReadOnly, "%s", reason)
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return refuse(driver.ErrSyntax, "invalid JSON: %v", err)
	}
	_, hasMethod := top["method"]
	_, hasPath := top["path"]
	if hasMethod || hasPath {
		return parseRaw(name, top, readOnly, allowSystem)
	}
	var index string
	if err := json.Unmarshal(top["index"], &index); err != nil || index == "" {
		return refuse(driver.ErrSyntax, `the search form needs "index", a non-empty string`)
	}
	if !validTarget(index) {
		return refuse(driver.ErrSyntax, "invalid index %q", index)
	}
	if readOnly && !allowSystem && systemTarget(index) {
		return refuse(driver.ErrReadOnly, "%s", systemReason(index))
	}
	delete(top, "index")
	return request{index: index, body: top}, nil
}

// parseRaw checks the raw form.
func parseRaw(name string, top map[string]json.RawMessage, readOnly, allowSystem bool) (request, error) {
	refuse := func(kind error, format string, args ...any) (request, error) {
		return request{}, errRefused(name, readOnly, kind, format, args...)
	}
	for k := range top {
		if k != "method" && k != "path" && k != "body" {
			return refuse(driver.ErrSyntax, "unknown key %q in a raw request (method, path and body are accepted)", k)
		}
	}
	var method, full string
	if json.Unmarshal(top["method"], &method) != nil || json.Unmarshal(top["path"], &full) != nil || full == "" {
		return refuse(driver.ErrSyntax, `a raw request needs "method" and "path" strings`)
	}
	method = strings.ToUpper(method)
	if !slices.Contains([]string{"GET", "POST", "PUT", "DELETE", "HEAD"}, method) {
		return refuse(driver.ErrSyntax, "unknown method %q", method)
	}
	r := request{raw: true, method: method}
	path, query, _ := strings.Cut(full, "?")
	segs, err := splitPath(path)
	if err != nil {
		return refuse(driver.ErrSyntax, "%v", err)
	}
	params, err := url.ParseQuery(query)
	if err != nil || strings.ContainsAny(query, "#") {
		return refuse(driver.ErrSyntax, "invalid query string")
	}
	r.path, r.segs, r.params = path, segs, params
	if body, ok := top["body"]; ok && !bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		r.rawBody, r.ndjson, err = rawBody(body)
		if err != nil {
			return refuse(driver.ErrSyntax, "%v", err)
		}
	}
	if readOnly {
		if !readEndpoint(method, segs) {
			return refuse(driver.ErrReadOnly, "%s %s is not a read endpoint", method, path)
		}
		for k, vs := range params {
			if !readParams[k] {
				return refuse(driver.ErrReadOnly, "query parameter %q is not on the read allow-list", k)
			}
			if k == "expand_wildcards" && !allowSystem {
				for _, v := range vs {
					if hiddenWildcards(v) {
						return refuse(driver.ErrReadOnly, "%s", wildcardReason(v))
					}
				}
			}
		}
		if !allowSystem {
			for _, seg := range segs {
				if systemTarget(seg) {
					return refuse(driver.ErrReadOnly, "%s", systemReason(seg))
				}
			}
		}
	}
	return r, nil
}

// rawBody returns a body as sent: an object as JSON, an array of objects
// as NDJSON (for _msearch and _bulk).
func rawBody(body json.RawMessage) ([]byte, bool, error) {
	body = bytes.TrimSpace(body)
	switch body[0] {
	case '{':
		var buf bytes.Buffer
		if err := json.Compact(&buf, body); err != nil {
			return nil, false, err
		}
		return buf.Bytes(), false, nil
	case '[':
		var lines []json.RawMessage
		if err := json.Unmarshal(body, &lines); err != nil {
			return nil, false, err
		}
		var buf bytes.Buffer
		for _, l := range lines {
			if l = bytes.TrimSpace(l); len(l) == 0 || l[0] != '{' {
				return nil, false, errors.New("an array body holds JSON objects, one per NDJSON line")
			}
			if err := json.Compact(&buf, l); err != nil {
				return nil, false, err
			}
			buf.WriteByte('\n')
		}
		return buf.Bytes(), true, nil
	}
	return nil, false, errors.New("the body is a JSON object, or an array of objects for an NDJSON endpoint")
}

// segmentRE is what one path segment may hold: index names and patterns
// (letters, digits, - _ . + * , and : for a remote cluster), endpoint
// names and document ids of the same characters. Anything that would need
// escaping is refused, so the path the guard reads is the path sent.
var segmentRE = regexp.MustCompile(`^[A-Za-z0-9_.+*,:\-]+$`)

// splitPath validates an absolute path and returns its segments ("/" has
// none). Empty, "." and ".." segments are refused.
func splitPath(p string) ([]string, error) {
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("the path %q must start with /", p)
	}
	if p == "/" {
		return nil, nil
	}
	segs := strings.Split(p[1:], "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || !segmentRE.MatchString(s) {
			return nil, fmt.Errorf("invalid path %q: segments are letters, digits and - _ . + * , : only", p)
		}
	}
	return segs, nil
}

// targetRE is one index name or pattern in a search target.
var targetRE = regexp.MustCompile(`^-?[A-Za-z0-9_.+*:\-]+$`)

// validTarget reports whether s is a comma-separated list of index names or
// patterns (with _all allowed), as one path segment.
func validTarget(s string) bool {
	for _, part := range strings.Split(s, ",") {
		if part == "." || part == ".." || !targetRE.MatchString(part) {
			return false
		}
		if strings.HasPrefix(part, "_") && part != "_all" {
			return false
		}
	}
	return true
}

// readEndpoint is the read-only allow-list: the endpoints that only read,
// each with the methods that keep it a read.
func readEndpoint(method string, segs []string) bool {
	get := method == "GET"
	getPost := get || method == "POST"
	if len(segs) == 0 {
		return get // cluster info
	}
	// An optional leading target: an index name, pattern or list.
	rest := segs
	if !strings.HasPrefix(segs[0], "_") || segs[0] == "_all" {
		if !validTarget(segs[0]) {
			return false
		}
		rest = segs[1:]
		if len(rest) == 0 {
			return false // GET /x is index metadata; DELETE /x drops it
		}
		switch {
		case len(rest) == 2 && (rest[0] == "_doc" || rest[0] == "_source"):
			return get // one document
		case len(rest) == 3 && rest[0] == "_mapping" && rest[1] == "field":
			return get
		}
	} else {
		switch segs[0] {
		case "_cat":
			return get
		case "_cluster":
			return get && len(segs) >= 2 && segs[1] == "health" && len(segs) <= 3 && (len(segs) == 2 || validTarget(segs[2]))
		}
		if len(segs) == 3 && segs[0] == "_mapping" && segs[1] == "field" {
			return get
		}
	}
	switch {
	case len(rest) == 1:
		switch rest[0] {
		case "_search", "_msearch", "_count", "_field_caps", "_mget":
			return getPost
		case "_mapping":
			return get // PUT or POST _mapping changes the mapping
		}
	case len(rest) == 2 && rest[0] == "_validate" && rest[1] == "query":
		return getPost
	}
	return false
}

// readParams are the query parameters a read-only raw request may carry.
// Unknown ones are refused: source= could carry a body past the script
// check, and a parameter tobira does not know could change what runs.
var readParams = map[string]bool{
	"q": true, "df": true, "analyzer": true, "analyze_wildcard": true, "default_operator": true, "lenient": true,
	"size": true, "from": true, "sort": true, "_source": true, "_source_includes": true, "_source_excludes": true,
	"stored_fields": true, "docvalue_fields": true, "fields": true, "version": true, "seq_no_primary_term": true,
	"explain": true, "rewrite": true, "all_shards": true, "track_total_hits": true, "track_scores": true,
	"typed_keys": true, "rest_total_hits_as_int": true, "search_type": true, "preference": true, "routing": true,
	"timeout": true, "terminate_after": true, "request_cache": true, "allow_no_indices": true,
	"expand_wildcards": true, "ignore_unavailable": true, "ignore_throttled": true, "allow_partial_search_results": true,
	"batched_reduce_size": true, "pre_filter_shard_size": true, "max_concurrent_shard_requests": true,
	"max_concurrent_searches": true, "ccs_minimize_roundtrips": true, "include_named_queries_score": true,
	"min_score": true, "realtime": true,
	"format": true, "h": true, "v": true, "s": true, "help": true, "bytes": true, "time": true, "pri": true,
	"health": true, "local": true, "master_timeout": true, "include_defaults": true, "flat_settings": true,
	"level": true, "include_unmapped": true,
	"pretty": true, "human": true, "filter_path": true, "error_trace": true,
}

// scanJSON checks that data is exactly one JSON value with no duplicate
// key in any object (Go keeps the last of two equal keys, a server might
// read the first), and returns the first key that contains "script" in any
// case, or "".
func scanJSON(data []byte) (string, error) {
	sc, err := scanJSONFull(data)
	return sc.scriptKey, err
}

// jsonScan is what scanJSONFull found that a read-only path refuses.
type jsonScan struct {
	scriptKey    string // a key containing "script"
	inferenceKey string // a key that runs an ML model or inference endpoint
	systemIndex  string // an index, _index or indices value naming a system or dot index
	wildcards    string // an expand_wildcards value reaching hidden indexes
}

// inferenceKeys run an ML model or a configured inference endpoint, which
// may be a paid third party that receives the query or document text:
// egress and cost, so a read-only path refuses them. sparse_vector is
// caught by its inference_id; with an explicit query_vector it runs no
// model.
var inferenceKeys = map[string]bool{
	"query_vector_builder": true, "inference_id": true, "model_id": true, "semantic": true,
	"text_expansion": true, "text_similarity_reranker": true,
}

// targetKeys hold index names inside a body: _msearch headers, _mget
// docs, terms lookups.
var targetKeys = map[string]bool{"index": true, "_index": true, "indices": true}

// systemTarget reports whether a target names a system or dot-prefixed
// index or pattern. An exclusion ("-.old") only narrows, so it passes.
func systemTarget(s string) bool {
	for _, part := range strings.Split(s, ",") {
		if strings.HasPrefix(strings.TrimSpace(part), ".") {
			return true
		}
	}
	return false
}

// hiddenWildcards reports whether an expand_wildcards value reaches
// hidden indexes.
func hiddenWildcards(v string) bool {
	for _, part := range strings.Split(v, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p == "all" || p == "hidden" {
			return true
		}
	}
	return false
}

func systemReason(target string) string {
	return fmt.Sprintf("the request reads the system or hidden index %q; set allow_system_indices=1 on the connection to allow it", target)
}

func wildcardReason(v string) string {
	return fmt.Sprintf("expand_wildcards=%s reaches hidden indexes; set allow_system_indices=1 on the connection to allow it", v)
}

// readOnlyReason is why a read-only path refuses the scanned text, or "".
func (s jsonScan) readOnlyReason(allowSystem bool) string {
	switch {
	case s.scriptKey != "":
		return fmt.Sprintf("the request holds a script (key %q), and scripts are refused", s.scriptKey)
	case s.inferenceKey != "":
		return fmt.Sprintf("the request runs inference (key %q): model and inference-endpoint calls are refused", s.inferenceKey)
	case allowSystem:
		return ""
	case s.systemIndex != "":
		return systemReason(s.systemIndex)
	case s.wildcards != "":
		return wildcardReason(s.wildcards)
	}
	return ""
}

// scanJSONFull is scanJSON, also recording inference keys, system index
// targets and hidden wildcards anywhere in the text.
func scanJSONFull(data []byte) (jsonScan, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var sc jsonScan
	var stack []*jsonFrame
	// valueKey is the key a value belongs to: the object's current key,
	// or the key that opened the array.
	valueKey := func() string {
		if n := len(stack); n > 0 {
			return stack[n-1].key
		}
		return ""
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return jsonScan{}, fmt.Errorf("invalid JSON: %v", err)
		}
		var top *jsonFrame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.keys != nil && top.expectKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				markValue(stack)
				continue
			}
			k, _ := tok.(string)
			if top.keys[k] {
				return jsonScan{}, fmt.Errorf("duplicate key %q", k)
			}
			top.keys[k] = true
			lk := strings.ToLower(k)
			if sc.scriptKey == "" && strings.Contains(lk, "script") {
				sc.scriptKey = k
			}
			if sc.inferenceKey == "" && inferenceKeys[lk] {
				sc.inferenceKey = k
			}
			top.key = k
			top.expectKey = false
			continue
		}
		if s, ok := tok.(string); ok {
			switch k := valueKey(); {
			case targetKeys[k] && sc.systemIndex == "" && systemTarget(s):
				sc.systemIndex = s
			case k == "expand_wildcards" && sc.wildcards == "" && hiddenWildcards(s):
				sc.wildcards = s
			}
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &jsonFrame{keys: map[string]bool{}, expectKey: true})
		case json.Delim('['):
			stack = append(stack, &jsonFrame{key: valueKey()})
		case json.Delim(']'):
			stack = stack[:len(stack)-1]
			markValue(stack)
		default:
			markValue(stack)
		}
		if len(stack) == 0 && dec.More() {
			return jsonScan{}, errors.New("invalid JSON: more than one value")
		}
	}
	if len(stack) != 0 {
		return jsonScan{}, errors.New("invalid JSON: unexpected end")
	}
	return sc, nil
}

// jsonFrame is one open container while scanJSON reads: an object keeps
// its keys and its current key, an array the key it is the value of.
type jsonFrame struct {
	keys      map[string]bool
	key       string
	expectKey bool
}

// markValue records that the innermost container got a value: an object
// then expects its next key.
func markValue(stack []*jsonFrame) {
	if n := len(stack); n > 0 && stack[n-1].keys != nil {
		stack[n-1].expectKey = true
	}
}
