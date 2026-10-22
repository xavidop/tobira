package weaviate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Raw GraphQL. Query accepts one read operation, { Get { Class(…) { … } } }
// or { Aggregate { Class(…) { … } } }, optionally spelled "query [Name]
// { … }". The text is lexed and parsed here, fail-closed: anything outside
// that subset (mutations, subscriptions, several operations, fragments
// definitions or spreads, variables, directives, Explore, introspection) is
// refused before a byte is sent. The validated text is then sent as is.

type tokKind uint8

const (
	tokPunct tokKind = iota
	tokName
	tokNumber
	tokString
)

type token struct {
	kind tokKind
	text string // the source text (strings keep their quotes)
	pos  int
}

// lexGraphQL splits text into tokens. Whitespace, line terminators,
// commas, comments and the byte-order mark are dropped; any character the
// GraphQL grammar does not allow is an error.
func lexGraphQL(text string) ([]token, error) {
	var toks []token
	i := 0
	isNameStart := func(c byte) bool { return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	for i < len(text) {
		c := text[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case strings.HasPrefix(text[i:], "\ufeff"):
			i += len("\ufeff")
		case c == '#':
			for i < len(text) && text[i] != '\n' && text[i] != '\r' {
				i++
			}
		case strings.HasPrefix(text[i:], "..."):
			toks = append(toks, token{tokPunct, "...", i})
			i += 3
		case strings.IndexByte("!$&():=@[]{|}", c) >= 0:
			toks = append(toks, token{tokPunct, string(c), i})
			i++
		case isNameStart(c):
			j := i + 1
			for j < len(text) && (isNameStart(text[j]) || isDigit(text[j])) {
				j++
			}
			toks = append(toks, token{tokName, text[i:j], i})
			i = j
		case c == '-' || isDigit(c):
			j, err := lexNumber(text, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tokNumber, text[i:j], i})
			i = j
		case strings.HasPrefix(text[i:], `"""`):
			end := blockStringEnd(text, i+3)
			if end < 0 {
				return nil, fmt.Errorf("unterminated block string at offset %d", i)
			}
			toks = append(toks, token{tokString, text[i:end], i})
			i = end
		case c == '"':
			end, err := stringEnd(text, i+1)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tokString, text[i:end], i})
			i = end
		default:
			return nil, fmt.Errorf("unexpected character %q at offset %d", rune(c), i)
		}
	}
	return toks, nil
}

// lexNumber reads an IntValue or FloatValue starting at i and returns its
// end. A number may not be followed by a name start or a dot.
func lexNumber(text string, i int) (int, error) {
	bad := func() (int, error) { return 0, fmt.Errorf("invalid number at offset %d", i) }
	digitsFrom := func(j int) int {
		for j < len(text) && text[j] >= '0' && text[j] <= '9' {
			j++
		}
		return j
	}
	j := i
	if text[j] == '-' {
		j++
	}
	k := digitsFrom(j)
	if k == j || text[j] == '0' && k > j+1 {
		return bad()
	}
	j = k
	if j < len(text) && text[j] == '.' {
		k = digitsFrom(j + 1)
		if k == j+1 {
			return bad()
		}
		j = k
	}
	if j < len(text) && (text[j] == 'e' || text[j] == 'E') {
		j++
		if j < len(text) && (text[j] == '+' || text[j] == '-') {
			j++
		}
		k = digitsFrom(j)
		if k == j {
			return bad()
		}
		j = k
	}
	if j < len(text) && (text[j] == '.' || text[j] == '_' || text[j] >= 'A' && text[j] <= 'Z' || text[j] >= 'a' && text[j] <= 'z') {
		return bad()
	}
	return j, nil
}

// stringEnd returns the end (after the closing quote) of a string whose
// body starts at i.
func stringEnd(text string, i int) (int, error) {
	for i < len(text) {
		c := text[i]
		switch {
		case c == '"':
			return i + 1, nil
		case c == '\\':
			if i+1 >= len(text) {
				return 0, errors.New("unterminated string")
			}
			switch text[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				if i+6 > len(text) || !isHex(text[i+2:i+6]) {
					return 0, fmt.Errorf("invalid unicode escape at offset %d", i)
				}
				i += 6
			default:
				return 0, fmt.Errorf("invalid escape at offset %d", i)
			}
		case c == '\n' || c == '\r':
			return 0, fmt.Errorf("line break in a string at offset %d", i)
		case c < 0x20 && c != '\t':
			return 0, fmt.Errorf("control character in a string at offset %d", i)
		default:
			i++
		}
	}
	return 0, errors.New("unterminated string")
}

// blockStringEnd returns the end (after the closing """) of a block string
// whose body starts at i, or -1.
func blockStringEnd(text string, i int) int {
	for i < len(text) {
		switch {
		case strings.HasPrefix(text[i:], `\"""`):
			i += 4
		case strings.HasPrefix(text[i:], `"""`):
			return i + 3
		default:
			i++
		}
	}
	return -1
}

func isHex(s string) bool {
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// gqlField is a field of a selection set, or an inline fragment (name "",
// on set).
type gqlField struct {
	alias, name string
	on          string // inline fragment type condition
	args        []gqlArg
	sel         []*gqlField
}

// key is the field's name in the response: its alias, else its name.
func (f *gqlField) key() string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

type gqlArg struct {
	name  string
	value *gqlValue
}

// gqlValue is an argument value: a scalar or enum token, a list or an
// input object.
type gqlValue struct {
	tok    *token
	items  []*gqlValue
	fields []gqlArg
	object bool
}

// gqlDoc is a parsed read: the operation field (Get or Aggregate) and the
// one collection under it.
type gqlDoc struct {
	op    string
	class *gqlField
}

// gqlRefusal is a parse failure; mutation says the text asked for a write
// (a mutation or subscription), which a writable connection reports as
// ErrUnsupported rather than ErrSyntax.
type gqlRefusal struct {
	msg      string
	mutation bool
}

func (e *gqlRefusal) Error() string { return e.msg }

type gqlParser struct {
	toks []token
	pos  int
}

func (p *gqlParser) peek() *token {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

func (p *gqlParser) fail(format string, args ...any) error {
	return &gqlRefusal{msg: fmt.Sprintf(format, args...)}
}

func (p *gqlParser) is(kind tokKind, text string) bool {
	t := p.peek()
	return t != nil && t.kind == kind && t.text == text
}

func (p *gqlParser) expect(text string) error {
	if !p.is(tokPunct, text) {
		if t := p.peek(); t != nil {
			return p.fail("expected %q at offset %d, found %q", text, t.pos, t.text)
		}
		return p.fail("expected %q at the end of the query", text)
	}
	p.pos++
	return nil
}

func (p *gqlParser) name() (string, error) {
	t := p.peek()
	if t == nil || t.kind != tokName {
		if t == nil {
			return "", p.fail("expected a name at the end of the query")
		}
		return "", p.fail("expected a name at offset %d, found %q", t.pos, t.text)
	}
	p.pos++
	return t.text, nil
}

// parseGraphQL parses one read operation of the accepted subset.
func parseGraphQL(text string) (*gqlDoc, error) {
	toks, err := lexGraphQL(text)
	if err != nil {
		return nil, &gqlRefusal{msg: err.Error()}
	}
	p := &gqlParser{toks: toks}
	if t := p.peek(); t != nil && t.kind == tokName {
		switch t.text {
		case "query":
			p.pos++
			if t := p.peek(); t != nil && t.kind == tokName {
				p.pos++
			}
			if p.is(tokPunct, "(") {
				return nil, p.fail("variables are not supported: write the values in the query")
			}
		case "mutation", "subscription":
			return nil, &gqlRefusal{msg: t.text + " operations are refused: Query only reads (edit objects with the editor)", mutation: true}
		case "fragment":
			return nil, p.fail("fragment definitions are not supported: inline the fields")
		default:
			return nil, p.fail("expected { or query, found %q", t.text)
		}
	}
	top, err := p.selectionSet()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t != nil {
		return nil, p.fail("only one operation is allowed: unexpected %q at offset %d", t.text, t.pos)
	}
	if len(top) != 1 || top[0].name == "" {
		return nil, p.fail("the query must hold exactly one Get or Aggregate")
	}
	op := top[0]
	if op.name != "Get" && op.name != "Aggregate" {
		return nil, p.fail("only Get and Aggregate are supported, not %q", op.name)
	}
	if op.alias != "" || len(op.args) > 0 {
		return nil, p.fail("%s takes no alias or arguments", op.name)
	}
	if len(op.sel) != 1 || op.sel[0].name == "" {
		return nil, p.fail("%s must hold exactly one collection", op.name)
	}
	class := op.sel[0]
	if len(class.sel) == 0 {
		return nil, p.fail("select at least one field of %s", class.name)
	}
	return &gqlDoc{op: op.name, class: class}, nil
}

func (p *gqlParser) selectionSet() ([]*gqlField, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var out []*gqlField
	for !p.is(tokPunct, "}") {
		if p.peek() == nil {
			return nil, p.fail("unterminated selection set")
		}
		f, err := p.selection()
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	p.pos++
	if len(out) == 0 {
		return nil, p.fail("empty selection set")
	}
	return out, nil
}

func (p *gqlParser) selection() (*gqlField, error) {
	if p.is(tokPunct, "...") {
		p.pos++
		if !p.is(tokName, "on") {
			return nil, p.fail("fragment spreads are not supported: inline the fields")
		}
		p.pos++
		on, err := p.name()
		if err != nil {
			return nil, err
		}
		if p.is(tokPunct, "@") {
			return nil, p.fail("directives are not supported")
		}
		sel, err := p.selectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlField{on: on, sel: sel}, nil
	}
	f := &gqlField{}
	n, err := p.name()
	if err != nil {
		return nil, err
	}
	if p.is(tokPunct, ":") {
		p.pos++
		f.alias = n
		if n, err = p.name(); err != nil {
			return nil, err
		}
	}
	f.name = n
	if strings.HasPrefix(n, "__") {
		return nil, p.fail("introspection (%s) is not supported", n)
	}
	if p.is(tokPunct, "(") {
		if f.args, err = p.arguments(); err != nil {
			return nil, err
		}
	}
	if p.is(tokPunct, "@") {
		return nil, p.fail("directives are not supported")
	}
	if p.is(tokPunct, "{") {
		if f.sel, err = p.selectionSet(); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (p *gqlParser) arguments() ([]gqlArg, error) {
	p.pos++ // (
	var out []gqlArg
	for !p.is(tokPunct, ")") {
		a, err := p.argument()
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	p.pos++
	if len(out) == 0 {
		return nil, p.fail("empty argument list")
	}
	return out, nil
}

func (p *gqlParser) argument() (gqlArg, error) {
	n, err := p.name()
	if err != nil {
		return gqlArg{}, err
	}
	if err := p.expect(":"); err != nil {
		return gqlArg{}, err
	}
	v, err := p.value()
	if err != nil {
		return gqlArg{}, err
	}
	return gqlArg{name: n, value: v}, nil
}

func (p *gqlParser) value() (*gqlValue, error) {
	t := p.peek()
	if t == nil {
		return nil, p.fail("expected a value at the end of the query")
	}
	switch {
	case t.kind == tokPunct && t.text == "$":
		return nil, p.fail("variables are not supported: write the values in the query")
	case t.kind == tokPunct && t.text == "[":
		p.pos++
		v := &gqlValue{}
		for !p.is(tokPunct, "]") {
			if p.peek() == nil {
				return nil, p.fail("unterminated list")
			}
			it, err := p.value()
			if err != nil {
				return nil, err
			}
			v.items = append(v.items, it)
		}
		p.pos++
		return v, nil
	case t.kind == tokPunct && t.text == "{":
		p.pos++
		v := &gqlValue{object: true}
		for !p.is(tokPunct, "}") {
			if p.peek() == nil {
				return nil, p.fail("unterminated object")
			}
			a, err := p.argument()
			if err != nil {
				return nil, err
			}
			v.fields = append(v.fields, a)
		}
		p.pos++
		return v, nil
	case t.kind == tokPunct:
		return nil, p.fail("unexpected %q at offset %d", t.text, t.pos)
	}
	p.pos++
	return &gqlValue{tok: t}, nil
}

// classInfo resolves what the guard needs to know of a collection.
type classInfo interface {
	vectorizers(class string) ([]string, error)
}

// Arguments of a collection field a read-only connection runs: none calls
// a module. nearText and hybrid vectorize their query text, so they run
// only when every vectorizer of the collection is local.
var readOnlyArgs = map[string]bool{
	"where": true, "limit": true, "offset": true, "after": true, "sort": true,
	"nearVector": true, "nearObject": true, "bm25": true, "groupBy": true, "group": true,
	"autocut": true, "tenant": true, "consistencyLevel": true, "objectLimit": true,
}

// _additional fields a read-only connection returns: every one reads
// stored data. generate, rerank, answer, summary, spellCheck,
// featureProjection and the rest are refused.
var readOnlyAdditional = map[string]bool{
	"id": true, "vector": true, "vectors": true, "distance": true, "certainty": true,
	"score": true, "explainScore": true, "creationTimeUnix": true, "lastUpdateTimeUnix": true,
	"isConsistent": true, "group": true,
}

// localVectorizers run inside the operator's own deployment: a query text
// they vectorize never leaves it. Any other vectorizer, including an
// unknown or empty one, counts as a remote, metered service.
var localVectorizers = map[string]bool{
	"none": true, "text2vec-transformers": true, "text2vec-contextionary": true,
	"text2vec-model2vec": true, "text2vec-gpt4all": true, "multi2vec-clip": true,
	"multi2vec-bind": true, "img2vec-neural": true, "ref2vec-centroid": true,
}

// isLocalVectorizer reports whether a vectorizer module runs locally.
func isLocalVectorizer(name string) bool { return localVectorizers[name] }

// guardGraphQL checks a parsed read against the collection and, on a
// read-only connection, refuses anything that may call a vectorizer,
// generative, reranker or other module: those are egress and cost.
func guardGraphQL(doc *gqlDoc, readOnly bool, classes classInfo) error {
	vz, err := classes.vectorizers(doc.class.name)
	if err != nil {
		return err
	}
	if !readOnly {
		return nil
	}
	refuse := func(format string, args ...any) error {
		return driver.Wrap(driver.ErrReadOnly, Name, "query", fmt.Errorf("read-only: "+format, args...))
	}
	for _, a := range doc.class.args {
		switch {
		case readOnlyArgs[a.name]:
		case a.name == "nearText" || a.name == "hybrid":
			if remote := remoteVectorizer(vz); remote != "" {
				return refuse("%s on %s calls the %s vectorizer, a remote service; search by vector instead (Similar with ai.embedder)", a.name, doc.class.name, remote)
			}
		default:
			return refuse("argument %s is refused: it may call a module", a.name)
		}
	}
	return guardSelection(doc.class.sel, refuse)
}

// remoteVectorizer returns the first vectorizer that is not local, or a
// placeholder when the list is empty.
func remoteVectorizer(vz []string) string {
	if len(vz) == 0 {
		// No declared vectorizer: what would run is unknown (fail closed).
		return "(none declared)"
	}
	for _, v := range vz {
		if !isLocalVectorizer(v) {
			if v == "" {
				return "(unknown)"
			}
			return v
		}
	}
	return ""
}

func guardSelection(sel []*gqlField, refuse func(string, ...any) error) error {
	for _, f := range sel {
		// topOccurrences (an Aggregate statistic) is the one field whose
		// arguments only shape the read.
		if f.name != "" && len(f.args) > 0 && f.name != "topOccurrences" {
			return refuse("field %s takes arguments, which may call a module", f.name)
		}
		if f.name == "_additional" {
			for _, a := range f.sel {
				if a.name == "" || !readOnlyAdditional[a.name] {
					return refuse("_additional { %s } is refused: it calls a module", a.name+a.on)
				}
			}
		}
		if err := guardSelection(f.sel, refuse); err != nil {
			return err
		}
	}
	return nil
}
