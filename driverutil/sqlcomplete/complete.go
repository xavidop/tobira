// Package sqlcomplete is a dependency-free SQL completion engine shared by
// the SQL drivers. It tokenizes the query text (strings, quoted identifiers,
// comments and, for postgres, dollar quotes), works out what the cursor is
// completing, and asks a Catalog for namespaces, collections and fields.
//
// Contexts, decided by the token before the one being typed:
//
//   - after FROM, JOIN, INTO, UPDATE or TABLE (and after a comma in a FROM
//     list): collections of the request namespace, plus namespaces as "ns.";
//   - after "<name>.": the fields of the collection or alias <name>, or the
//     collections of the namespace <name>;
//   - after SELECT, WHERE, BY, ON, SET, AND, OR, an operator or "(" ... : the
//     fields of every collection the statement names, plus keywords;
//   - anywhere else: keywords.
//
// Collections are collected from the whole statement around the cursor
// (FROM/JOIN/UPDATE/INTO/TABLE name [AS] alias), without regard to
// sub-select scoping. The package only reads the Catalog and never writes.
//
// An Insert text replaces the identifier under the cursor, the byte range
// [TokenStart, TokenEnd) (an empty range at the cursor when none is being
// typed); Insert is empty when the Label can be inserted as-is.
package sqlcomplete

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

// MaxResults caps the completions Complete returns.
const MaxResults = 200

// Catalog is the schema a completion reads. Names are returned as stored; a
// namespace of "" means the connection's default. Fields reports a missing
// collection with driver.ErrNotFound (such references are skipped).
type Catalog interface {
	Namespaces(ctx context.Context) ([]string, error)
	Collections(ctx context.Context, ns string) ([]string, error)
	Fields(ctx context.Context, ns, coll string) ([]string, error)
}

type ctxKind int

const (
	ctxKeywords ctxKind = iota
	ctxCollections
	ctxFields
	ctxQualified // after "name."
	ctxInsertCols
	ctxNone
)

var (
	collectionKeywords = setOf("FROM JOIN INTO UPDATE TABLE")
	fieldKeywords      = setOf("SELECT WHERE BY ON SET AND OR HAVING DISTINCT WHEN THEN ELSE NOT RETURNING CASE USING")
	aliasStopWords     = setOf(`WHERE JOIN INNER LEFT RIGHT FULL OUTER CROSS NATURAL ON USING GROUP ORDER HAVING LIMIT
OFFSET UNION INTERSECT EXCEPT SET VALUES SELECT RETURNING WINDOW FETCH FOR FROM AS INTO`)
	fieldPunct = setOf("= < > + - / | ! % ^ &")
)

func setOf(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// request is the analysed cursor position.
type request struct {
	text   string
	cursor int
	toks   []token // code tokens of the statement, without the typed one
	prev   []token // statement tokens before the typed one
	prefix string  // what has been typed of the current identifier, unquoted
	quote  byte    // the quote character the typed identifier opened with, or 0
	start  int     // offset where the typed identifier starts
	end    int     // offset where it ends (after the cursor when typing mid-identifier)
}

// Complete returns the completions at req.Cursor for dialect d, reading
// names from cat. Results are filtered by the typed prefix (case-insensitive),
// ordered by exact match, then kind, then alphabetically, and capped at
// MaxResults. It returns nil when the cursor is inside a string or comment.
func Complete(ctx context.Context, cat Catalog, d Dialect, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, ok := analyse(req, d)
	if !ok {
		return nil, nil
	}
	kind, qual := classify(r.prev)

	var cands []candidate
	var err error
	switch kind {
	case ctxNone:
		return nil, nil
	case ctxKeywords:
		cands = keywordCands(d, 0)
	case ctxCollections:
		cands, err = collectionCands(ctx, cat, req.Namespace, true)
	case ctxFields:
		cands, err = fieldCands(ctx, cat, req.Namespace, parseRefs(r.toks))
		cands = append(cands, keywordCands(d, 1)...)
	case ctxInsertCols:
		cands, err = fieldCands(ctx, cat, req.Namespace, qual)
		cands = append(cands, keywordCands(d, 1)...)
	case ctxQualified:
		cands, err = qualifiedCands(ctx, cat, req.Namespace, qual, parseRefs(r.toks))
	}
	if err != nil {
		return nil, err
	}
	return finish(cands, r, d), nil
}

// TokenStart returns the offset where the identifier being typed at cursor
// starts (including an opening quote), or cursor when none is being typed.
// It is where a completion's Insert text begins to replace.
func TokenStart(text string, cursor int, d Dialect) int {
	r, ok := analyse(driver.CompletionRequest{Text: text, Cursor: cursor}, d)
	if !ok {
		return clampCursor(text, cursor)
	}
	return r.start
}

// TokenEnd returns the offset where the identifier under cursor ends, or
// cursor when none is being typed. A quoted identifier ends after its closing
// quote. The TUI replaces [TokenStart, TokenEnd) with the chosen Insert (or
// Label).
func TokenEnd(text string, cursor int, d Dialect) int {
	r, ok := analyse(driver.CompletionRequest{Text: text, Cursor: cursor}, d)
	if !ok {
		return clampCursor(text, cursor)
	}
	return r.end
}

func clampCursor(text string, cur int) int {
	cur = max(0, min(cur, len(text)))
	for cur > 0 && cur < len(text) && !utf8.RuneStart(text[cur]) {
		cur--
	}
	return cur
}

// analyse tokenizes the text and locates the cursor. ok is false when the
// cursor sits inside a string, comment or number, where nothing completes.
func analyse(req driver.CompletionRequest, d Dialect) (request, bool) {
	text := req.Text
	cur := clampCursor(text, req.Cursor)
	r := request{text: text, cursor: cur, start: cur, end: cur}

	var code []token
	typed := -1
	for _, t := range tokenize(text, d) {
		inside := t.start < cur && (cur < t.end || (t.open && cur <= t.end))
		switch t.kind {
		case tkComment, tkString, tkNumber:
			if inside || (t.kind == tkNumber && t.start < cur && cur <= t.end) {
				return r, false
			}
		case tkWord:
			if t.start < cur && cur <= t.end {
				typed = len(code)
			}
		case tkQuoted:
			if inside {
				typed = len(code)
			}
		}
		if t.kind != tkComment {
			code = append(code, t)
		}
	}

	if typed >= 0 {
		t := code[typed]
		r.start, r.end = t.start, t.end
		if t.kind == tkWord {
			r.prefix = text[t.start:cur]
		} else {
			r.quote = text[t.start]
			raw := text[t.start+1 : cur]
			switch r.quote {
			case '[':
			case '`':
				raw = strings.ReplaceAll(raw, "``", "`")
			default:
				raw = strings.ReplaceAll(raw, `""`, `"`)
			}
			r.prefix = raw
		}
		code = slices.Delete(code, typed, typed+1)
	}

	// Statement bounds: between the surrounding semicolons.
	split := 0
	for split < len(code) && code[split].end <= r.start {
		split++
	}
	lo, hi := 0, len(code)
	for i := split - 1; i >= 0; i-- {
		if isPunct(code[i], ";") {
			lo = i + 1
			break
		}
	}
	for i := split; i < len(code); i++ {
		if isPunct(code[i], ";") {
			hi = i
			break
		}
	}
	r.toks, r.prev = code[lo:hi], code[lo:split]
	return r, true
}

func isPunct(t token, p string) bool { return t.kind == tkPunct && t.text == p }

// kw returns the upper-cased bare word of t, or "" for any other token.
func kw(t token) string {
	if t.kind == tkWord {
		return strings.ToUpper(t.text)
	}
	return ""
}

func isName(t token) bool { return t.kind == tkWord || t.kind == tkQuoted }

// ident returns the identifier value of a name token.
func ident(t token) string { return t.text }

// ref is a collection named by the statement, optionally aliased.
type ref struct{ ns, name, alias string }

// classify decides what is being completed from the tokens before the cursor.
// For ctxQualified and ctxInsertCols the returned refs say which collection
// (or namespace, in name) the qualifier names.
func classify(prev []token) (ctxKind, []ref) {
	fn := functionalFroms(prev)
	if len(prev) == 0 {
		return ctxKeywords, nil
	}
	last := prev[len(prev)-1]
	switch {
	case isPunct(last, "."):
		if len(prev) < 2 || !isName(prev[len(prev)-2]) {
			return ctxNone, nil
		}
		q := ref{name: ident(prev[len(prev)-2])}
		if len(prev) >= 4 && isPunct(prev[len(prev)-3], ".") && isName(prev[len(prev)-4]) {
			q.ns = ident(prev[len(prev)-4])
		}
		return ctxQualified, []ref{q}
	case isPunct(last, ":"):
		return ctxNone, nil // ::cast or :param
	case isPunct(last, ","):
		return classifyComma(prev, fn), nil
	case isPunct(last, "("):
		return classifyParen(prev)
	case last.kind == tkPunct && fieldPunct[last.text]:
		return ctxFields, nil
	}
	k := kw(last)
	switch {
	case k == "FROM" && fn[len(prev)-1]:
		return ctxFields, nil
	case collectionKeywords[k]:
		return ctxCollections, nil
	case fieldKeywords[k]:
		return ctxFields, nil
	}
	return ctxKeywords, nil
}

// classifyComma walks back to the clause keyword the comma belongs to,
// skipping balanced parentheses.
func classifyComma(prev []token, fn []bool) ctxKind {
	depth := 0
	for i := len(prev) - 2; i >= 0; i-- {
		t := prev[i]
		switch {
		case isPunct(t, ")"):
			depth++
		case isPunct(t, "("):
			if depth == 0 {
				return ctxFields
			}
			depth--
		case depth == 0 && kw(t) == "FROM" && fn[i]:
			continue
		case depth == 0 && collectionKeywords[kw(t)]:
			return ctxCollections
		case depth == 0 && fieldKeywords[kw(t)]:
			return ctxFields
		}
	}
	return ctxFields
}

// classifyParen handles "(": the column list of INSERT INTO t ( completes
// that table's fields, VALUES ( completes nothing but keywords.
func classifyParen(prev []token) (ctxKind, []ref) {
	n := len(prev)
	if n >= 2 && kw(prev[n-2]) == "VALUES" {
		return ctxKeywords, nil
	}
	if n >= 3 && isName(prev[n-2]) {
		r := ref{name: ident(prev[n-2])}
		before := prev[n-3]
		if isPunct(before, ".") && n >= 4 && isName(prev[n-4]) {
			r.ns = ident(prev[n-4])
			before = token{}
			if n >= 5 {
				before = prev[n-5]
			}
		}
		if k := kw(before); k == "INTO" || k == "TABLE" {
			return ctxInsertCols, []ref{r}
		}
	}
	return ctxFields, nil
}

// parseRefs extracts the collections a statement names.
func parseRefs(toks []token) []ref {
	var refs []ref
	fn := functionalFroms(toks)
	for i := 0; i < len(toks); i++ {
		k := kw(toks[i])
		if !collectionKeywords[k] || (k == "FROM" && fn[i]) {
			continue
		}
		j := i + 1
		for {
			r, next, ok := readRef(toks, j)
			if !ok {
				break
			}
			refs = append(refs, r)
			j = next
			if k == "FROM" && j < len(toks) && isPunct(toks[j], ",") {
				j++
				continue
			}
			break
		}
		i = max(i, j-1)
	}
	return refs
}

var fromFunctions = setOf("EXTRACT SUBSTRING TRIM OVERLAY")

// functionalFroms marks, in one pass, the FROM tokens that belong to an
// expression rather than naming tables: IS [NOT] DISTINCT FROM, and the FROM
// inside EXTRACT(...), SUBSTRING(...), TRIM(...) and OVERLAY(...).
func functionalFroms(toks []token) []bool {
	out := make([]bool, len(toks))
	var stack []string // the function name before each open parenthesis
	for i, t := range toks {
		switch {
		case isPunct(t, "("):
			name := ""
			if i > 0 {
				name = kw(toks[i-1])
			}
			stack = append(stack, name)
		case isPunct(t, ")"):
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case kw(t) == "FROM":
			out[i] = (i > 0 && kw(toks[i-1]) == "DISTINCT") ||
				(len(stack) > 0 && fromFunctions[stack[len(stack)-1]])
		}
	}
	return out
}

// readRef reads "[ns.]name [[AS] alias]" at toks[j:].
func readRef(toks []token, j int) (r ref, next int, ok bool) {
	if j >= len(toks) || !isName(toks[j]) || aliasStopWords[kw(toks[j])] {
		return ref{}, j, false
	}
	r.name = ident(toks[j])
	j++
	if j < len(toks) && isPunct(toks[j], ".") {
		if j+1 >= len(toks) || !isName(toks[j+1]) {
			return ref{}, j, false // "ns." still being typed
		}
		r.ns, r.name = r.name, ident(toks[j+1])
		j += 2
	}
	if j < len(toks) && kw(toks[j]) == "AS" && j+1 < len(toks) && isName(toks[j+1]) {
		r.alias = ident(toks[j+1])
		return r, j + 2, true
	}
	if j < len(toks) && isName(toks[j]) && !aliasStopWords[kw(toks[j])] {
		r.alias = ident(toks[j])
		j++
	}
	return r, j, true
}

type candidate struct {
	label, kind, detail string
	rank                int    // lower sorts first within equal match quality
	lower               string // strings.ToLower(label), set by finish
}

func keywordCands(d Dialect, rank int) []candidate {
	out := make([]candidate, len(d.Keywords))
	for i, k := range d.Keywords {
		out[i] = candidate{label: k, kind: "keyword", rank: rank + 10}
	}
	return out
}

func collectionCands(ctx context.Context, cat Catalog, ns string, withNamespaces bool) ([]candidate, error) {
	colls, err := cat.Collections(ctx, ns)
	if err != nil {
		return nil, err
	}
	out := make([]candidate, 0, len(colls))
	for _, c := range colls {
		out = append(out, candidate{label: c, kind: "collection"})
	}
	if withNamespaces {
		nss, err := cat.Namespaces(ctx)
		if err != nil {
			return nil, err
		}
		for _, n := range nss {
			out = append(out, candidate{label: n, kind: "namespace", rank: 1})
		}
	}
	return out, nil
}

// resolve finds the catalog spelling of a referenced collection: an exact
// match first, then a case-insensitive one, else the name as written.
func resolve(ctx context.Context, cat Catalog, defNS string, r ref) (ns, name string, err error) {
	ns = cmp.Or(r.ns, defNS)
	colls, err := cat.Collections(ctx, ns)
	if err != nil {
		return "", "", err
	}
	if slices.Contains(colls, r.name) {
		return ns, r.name, nil
	}
	if i := slices.IndexFunc(colls, func(c string) bool { return strings.EqualFold(c, r.name) }); i >= 0 {
		return ns, colls[i], nil
	}
	return ns, r.name, nil
}

// fieldCands lists the fields of refs, merging a field shared by several
// collections into one candidate whose Detail names them all.
func fieldCands(ctx context.Context, cat Catalog, defNS string, refs []ref) ([]candidate, error) {
	var out []candidate
	index := map[string]int{}
	seen := map[ref]bool{}
	for _, r := range refs {
		ns, name, err := resolve(ctx, cat, defNS, r)
		if err != nil {
			return nil, err
		}
		key := ref{ns: ns, name: name}
		if seen[key] {
			continue
		}
		seen[key] = true
		fields, err := cat.Fields(ctx, ns, name)
		if errors.Is(err, driver.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, f := range fields {
			if i, ok := index[f]; ok {
				out[i].detail += ", " + name
				continue
			}
			index[f] = len(out)
			out = append(out, candidate{label: f, kind: "field", detail: name})
		}
	}
	return out, nil
}

// qualifiedCands completes after "name.": an alias or a statement collection
// gives its fields, a namespace gives its collections, anything else is taken
// as a collection of the default namespace.
func qualifiedCands(ctx context.Context, cat Catalog, defNS string, q, refs []ref) ([]candidate, error) {
	target := q[0]
	if target.ns != "" { // ns.coll.
		return fieldCands(ctx, cat, defNS, q)
	}
	for _, r := range refs {
		if r.alias != "" && strings.EqualFold(r.alias, target.name) {
			return fieldCands(ctx, cat, defNS, []ref{{ns: r.ns, name: r.name}})
		}
	}
	for _, r := range refs {
		if strings.EqualFold(r.name, target.name) {
			return fieldCands(ctx, cat, defNS, []ref{{ns: r.ns, name: r.name}})
		}
	}
	nss, err := cat.Namespaces(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nss {
		if strings.EqualFold(n, target.name) {
			return collectionCands(ctx, cat, n, false)
		}
	}
	return fieldCands(ctx, cat, defNS, []ref{{name: target.name}})
}

// finish filters by prefix, orders, caps and renders the candidates.
func finish(cands []candidate, r request, d Dialect) []driver.Completion {
	prefix := strings.ToLower(r.prefix)
	kept := cands[:0:0]
	for _, c := range cands {
		c.lower = strings.ToLower(c.label)
		if strings.HasPrefix(c.lower, prefix) {
			kept = append(kept, c)
		}
	}
	slices.SortStableFunc(kept, func(a, b candidate) int {
		ea, eb := a.lower == prefix, b.lower == prefix
		if ea != eb {
			if ea {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.rank, b.rank), cmp.Compare(a.lower, b.lower), cmp.Compare(a.label, b.label))
	})
	if len(kept) > MaxResults {
		kept = kept[:MaxResults]
	}
	out := make([]driver.Completion, len(kept))
	for i, c := range kept {
		out[i] = driver.Completion{Label: c.label, Kind: c.kind, Detail: c.detail, Insert: render(c, r, d)}
	}
	return out
}

// render builds the Insert text: identifiers are quoted when the dialect
// needs it or when the user opened a quote; namespaces get a trailing dot.
func render(c candidate, r request, d Dialect) string {
	if c.kind == "keyword" {
		return ""
	}
	text := c.label
	if r.quote != 0 || d.needsQuote(c.label) {
		q := r.quote
		if q == 0 {
			q = d.IdentQuote
		}
		if q == 0 {
			q = '"'
		}
		text = d.quote(c.label, q)
	}
	if c.kind == "namespace" {
		return text + "."
	}
	if text == c.label {
		return ""
	}
	return text
}
