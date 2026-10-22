package cosmos

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

// tokKind classifies a Cosmos SQL token.
type tokKind int

const (
	tokIdent tokKind = iota
	tokString
	tokNumber
	tokParam
	tokPunct
	tokComment // only from lexComments
)

type token struct {
	kind tokKind
	text string
}

// isWord reports whether t is the identifier w, ignoring case.
func (t token) isWord(w string) bool { return t.kind == tokIdent && strings.EqualFold(t.text, w) }

// punct2 are the two-character operators.
var punct2 = []string{">=", "<=", "!=", "<>", "||", "??", "<<", ">>"}

// lex splits Cosmos SQL into tokens. It understands '…' and "…" strings
// with backslash escapes and -- comments (Cosmos SQL has no block
// comments). Anything it cannot classify, such as an unterminated string or
// a stray character, is an error: the guard is fail-closed.
func lex(s string) ([]token, error) { return lexAll(s, false) }

// lexComments is lex, keeping each -- comment as a tokComment.
func lexComments(s string) ([]token, error) { return lexAll(s, true) }

func lexAll(s string, comments bool) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			j := i
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if comments {
				out = append(out, token{tokComment, s[j:i]})
			}
		case c == '\'' || c == '"':
			j := i + 1
			for ; j < len(s) && s[j] != c; j++ {
				if s[j] == '\\' {
					j++
				}
			}
			if j >= len(s) {
				return nil, errors.New("unterminated string literal")
			}
			out = append(out, token{tokString, s[i : j+1]})
			i = j + 1
		case c == '@':
			j := i + 1
			for j < len(s) && isIdentByte(s, j) {
				j += identWidth(s, j)
			}
			if j == i+1 {
				return nil, errors.New("@ without a parameter name")
			}
			out = append(out, token{tokParam, s[i:j]})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
				j++
				if j < len(s) && (s[j] == '+' || s[j] == '-') {
					j++
				}
				for j < len(s) && s[j] >= '0' && s[j] <= '9' {
					j++
				}
			}
			out = append(out, token{tokNumber, s[i:j]})
			i = j
		case isIdentStart(s, i):
			j := i
			for j < len(s) && isIdentByte(s, j) {
				j += identWidth(s, j)
			}
			out = append(out, token{tokIdent, s[i:j]})
			i = j
		default:
			if i+1 < len(s) && slices.Contains(punct2, s[i:i+2]) {
				out = append(out, token{tokPunct, s[i : i+2]})
				i += 2
				continue
			}
			if !strings.ContainsRune("()[]{},.:*+-/%=<>!?&|^~;", rune(c)) {
				return nil, fmt.Errorf("unexpected character %q", rune(c))
			}
			out = append(out, token{tokPunct, s[i : i+1]})
			i++
		}
	}
	return out, nil
}

func isIdentStart(s string, i int) bool {
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r == '_' || r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') || r >= utf8.RuneSelf && unicode.IsLetter(r)
}

func isIdentByte(s string, i int) bool {
	r, _ := utf8.DecodeRuneInString(s[i:])
	return isIdentStart(s, i) || r >= '0' && r <= '9' || r >= utf8.RuneSelf && unicode.IsDigit(r)
}

func identWidth(s string, i int) int {
	_, w := utf8.DecodeRuneInString(s[i:])
	return w
}

// writeVerbs open statements Cosmos SQL does not have; they get a clearer
// refusal than other text.
var writeVerbs = []string{
	"insert", "update", "delete", "upsert", "replace", "merge", "create", "drop", "alter",
	"exec", "execute", "call", "truncate", "patch", "grant", "revoke", "set",
}

// request is a parsed query: the container it reads, its Cosmos SQL text,
// its partition key (nil for a cross-partition query) and its parameters.
type request struct {
	container    string
	sql          string
	partitionKey []any
	params       []azcosmos.QueryParameter
}

// refusal builds a guard refusal: ErrReadOnly on a read-only call (the
// guard refuses what it cannot classify), else kind.
func refusal(readOnly bool, kind error, msg string) error {
	if readOnly {
		return driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New(msg))
	}
	return driver.Wrap(kind, Name, "query", errors.New(msg))
}

// parseRequest parses a query: a Cosmos SQL SELECT naming its container in
// FROM, or a JSON envelope {"container", "query", "partitionKey",
// "parameters"} (exactly those keys, container and query required).
// Query.Params bind as @p1, @p2…. On a read-only call it is the read-only
// guard: anything but one SELECT, a UDF call or an envelope it cannot read
// is ErrReadOnly. On a writable call a write verb is ErrUnsupported (Cosmos
// SQL has none: use the editor) and other malformed text ErrSyntax.
func parseRequest(text string, params []driver.Value, readOnly bool) (request, error) {
	var req request
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		env, err := parseEnvelope(trimmed, readOnly)
		if err != nil {
			return req, err
		}
		req = env
		if _, err := checkSelect(req.sql, readOnly); err != nil {
			return req, err
		}
	} else {
		toks, err := checkSelect(text, readOnly)
		if err != nil {
			return req, err
		}
		if req.container, err = fromContainer(toks); err != nil {
			return req, err
		}
		req.sql = text
	}
	for i, p := range params {
		name := "@p" + strconv.Itoa(i+1)
		if slices.ContainsFunc(req.params, func(q azcosmos.QueryParameter) bool { return q.Name == name }) {
			return req, driver.Wrap(driver.ErrSyntax, Name, "query", fmt.Errorf("parameter %s is both in the envelope and in Query.Params", name))
		}
		raw, err := rawJSON(p)
		if err != nil {
			return req, driver.Wrap(driver.ErrUnsupported, Name, "query", fmt.Errorf("parameter %s: %w", name, err))
		}
		req.params = append(req.params, azcosmos.QueryParameter{Name: name, Value: raw})
	}
	return req, nil
}

// checkSelect lexes sql and checks it is one SELECT; on a read-only call
// it also refuses UDF calls.
func checkSelect(sql string, readOnly bool) ([]token, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, refusal(readOnly, driver.ErrSyntax, "cannot read the query: "+err.Error())
	}
	if len(toks) == 0 {
		return nil, refusal(readOnly, driver.ErrSyntax, "the query is empty")
	}
	if !toks[0].isWord("select") {
		if toks[0].kind == tokIdent && slices.Contains(writeVerbs, strings.ToLower(toks[0].text)) {
			return nil, refusal(readOnly, driver.ErrUnsupported,
				"Cosmos SQL only reads: "+strings.ToUpper(toks[0].text)+" is not a statement (edit documents in the grid instead)")
		}
		return nil, refusal(readOnly, driver.ErrSyntax, "a query must be one SELECT")
	}
	if slices.ContainsFunc(toks, func(t token) bool { return t.kind == tokPunct && t.text == ";" }) {
		return nil, refusal(readOnly, driver.ErrSyntax, "a query must be one SELECT, without ';'")
	}
	if readOnly {
		if err := checkNoUDF(toks); err != nil {
			return nil, err
		}
	}
	return toks, nil
}

// checkNoUDF refuses, on a read-only call, every udf token except a
// property path step (c.udf, c["a"].udf: "." right after an identifier or
// "]"). Fail-closed: udf.f(c), udf["f"](c), (udf).f(c), [udf.f(c)],
// 1 .udf.f(c) and an alias named udf are all refused, so user JavaScript
// never runs there.
func checkNoUDF(toks []token) error {
	for i, t := range toks {
		if !t.isWord("udf") {
			continue
		}
		if i >= 2 && toks[i-1].kind == tokPunct && toks[i-1].text == "." &&
			(toks[i-2].kind == tokIdent || toks[i-2].kind == tokPunct && toks[i-2].text == "]") {
			continue // a property named udf
		}
		return driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("user-defined functions (udf…) are not run on a read-only connection"))
	}
	return nil
}

// errNoContainer is returned when a SELECT names no container.
var errNoContainer = errors.New(`name the container in FROM ("SELECT * FROM orders o"), or use {"container":…,"query":…}`)

// fromContainer returns the container named by the top-level FROM clause:
// FROM orders [AS] o, or FROM t IN orders.tags.
func fromContainer(toks []token) (string, error) {
	depth := 0
	for i, t := range toks {
		if t.kind == tokPunct {
			switch t.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			}
			continue
		}
		if depth != 0 || !t.isWord("from") {
			continue
		}
		j := i + 1
		if j+2 < len(toks) && toks[j].kind == tokIdent && toks[j+1].isWord("in") {
			j += 2 // FROM alias IN container.path
		}
		if j < len(toks) && toks[j].kind == tokIdent && !toks[j].isWord("root") {
			return toks[j].text, nil
		}
		break
	}
	return "", driver.Wrap(driver.ErrSyntax, Name, "query", errNoContainer)
}

// parseEnvelope reads {"container", "query", "partitionKey",
// "parameters"} strictly: one object, known keys once each, nothing after.
func parseEnvelope(text string, readOnly bool) (request, error) {
	var req request
	bad := func(msg string) (request, error) {
		return request{}, refusal(readOnly, driver.ErrSyntax, "query envelope: "+msg)
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return bad(`want one JSON object {"container":…,"query":…}`)
	}
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return bad("cannot read it")
		}
		key, _ := kt.(string)
		if seen[key] {
			return bad(fmt.Sprintf("key %q is given twice", key))
		}
		seen[key] = true
		switch key {
		case "container", "query":
			var s string
			if err := dec.Decode(&s); err != nil {
				return bad(fmt.Sprintf("%s must be a string", key))
			}
			if key == "container" {
				req.container = s
			} else {
				req.sql = s
			}
		case "partitionKey":
			var v any
			if err := dec.Decode(&v); err != nil {
				return bad("cannot read partitionKey")
			}
			pk, err := envelopePartitionKey(v)
			if err != nil {
				return bad(err.Error())
			}
			req.partitionKey = pk
		case "parameters":
			var m map[string]json.RawMessage
			if err := dec.Decode(&m); err != nil {
				return bad(`parameters must be an object such as {"@min":10}`)
			}
			names := make([]string, 0, len(m))
			for n := range m {
				if len(n) < 2 || n[0] != '@' {
					return bad(fmt.Sprintf("parameter %q must start with @", n))
				}
				names = append(names, n)
			}
			slices.Sort(names)
			for _, n := range names {
				req.params = append(req.params, azcosmos.QueryParameter{Name: n, Value: m[n]})
			}
		default:
			return bad(fmt.Sprintf("unknown key %q (known: container, query, partitionKey, parameters)", key))
		}
	}
	if _, err := dec.Token(); err != nil {
		return bad("cannot read it")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad("trailing data after the object")
	}
	if req.container == "" {
		return bad("container is missing or empty")
	}
	if !seen["query"] {
		return bad("query is missing")
	}
	return req, nil
}

// envelopePartitionKey reads a partitionKey value: one scalar, or a
// non-empty array of scalars for a hierarchical key.
func envelopePartitionKey(v any) ([]any, error) {
	vals, ok := v.([]any)
	if !ok {
		vals = []any{v}
	}
	if len(vals) == 0 {
		return nil, errors.New("partitionKey must not be an empty array")
	}
	out := make([]any, len(vals))
	for i, e := range vals {
		switch t := e.(type) {
		case nil, string, bool:
			out[i] = t
		case json.Number:
			f, err := t.Float64()
			if err != nil {
				return nil, fmt.Errorf("partitionKey number %s is out of range", t)
			}
			out[i] = f
		default:
			return nil, errors.New("a partitionKey value must be a string, number, boolean or null")
		}
	}
	return out, nil
}

// checkRawFilter checks a raw WHERE fragment for Browse and Similar: it
// must lex, keep its parentheses and brackets balanced (so it cannot close
// the clause it is wrapped in) and hold no ';' and no -- comment (which
// would swallow the closing parenthesis and what follows). On a read-only call it may
// not call a UDF. A failure is ErrReadOnly on a read-only call, else
// ErrSyntax.
func checkRawFilter(raw string, readOnly bool) error {
	toks, err := lexComments(raw)
	if err != nil {
		return refusal(readOnly, driver.ErrSyntax, "cannot read the raw filter: "+err.Error())
	}
	depth := 0
	for _, t := range toks {
		if t.kind == tokComment {
			return refusal(readOnly, driver.ErrSyntax, "the raw filter must not hold a -- comment")
		}
		if t.kind != tokPunct {
			continue
		}
		switch t.text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
		case ";":
			return refusal(readOnly, driver.ErrSyntax, "the raw filter must not hold ';'")
		}
		if depth < 0 {
			return refusal(readOnly, driver.ErrSyntax, "the raw filter closes a parenthesis it did not open")
		}
	}
	if depth != 0 {
		return refusal(readOnly, driver.ErrSyntax, "the raw filter leaves a parenthesis open")
	}
	if readOnly {
		return checkNoUDF(toks)
	}
	return nil
}
