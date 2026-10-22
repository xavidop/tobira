package milvus

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"

	"github.com/xavidop/tobira/driver"
)

// maxFilter caps a boolean expression's length.
const maxFilter = 64 << 10

// filterFunctions is the read-only allow-list of functions a filter may
// call: the Milvus 2.6 expression built-ins, which all read the row they
// test. Matching ignores case, as Milvus does.
var filterFunctions = map[string]bool{
	"json_contains": true, "json_contains_all": true, "json_contains_any": true,
	"array_contains": true, "array_contains_all": true, "array_contains_any": true, "array_length": true,
	"text_match": true, "phrase_match": true, "random_sample": true,
	"st_equals": true, "st_touches": true, "st_overlaps": true, "st_crosses": true, "st_contains": true,
	"st_intersects": true, "st_within": true, "st_dwithin": true, "st_isvalid": true,
}

// identifier is the alphabet of field names, dynamic keys the driver
// builds into expressions, and template parameter names.
var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkFilter lexes a Milvus boolean expression and refuses what the guard
// cannot read: an unknown character, an unterminated string, a bad
// template variable, or unbalanced brackets, in every mode; on a read-only
// request also a call of a function outside filterFunctions. A filter can
// only read (Milvus has no expression that writes), so this is defence in
// depth: an expression the guard cannot classify never reaches the server
// on a read-only connection (fail closed). Refusals are ErrReadOnly when
// readOnly, ErrSyntax otherwise.
func checkFilter(text string, readOnly bool) error {
	fail := func(format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		if readOnly {
			return driver.Wrap(driver.ErrReadOnly, Name, "filter", errors.New("read-only: the filter is refused: "+msg))
		}
		return driver.Wrap(driver.ErrSyntax, Name, "filter", errors.New(msg))
	}
	if len(text) > maxFilter {
		return fail("the filter is longer than %d bytes", maxFilter)
	}
	if !utf8.ValidString(text) {
		return fail("the filter is not valid UTF-8")
	}
	var stack []byte
	lastIdent := ""
	for i := 0; i < len(text); {
		ch := text[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			i++
			continue
		case isIdentStart(ch) || ch == '$':
			j := i + 1
			for j < len(text) && isIdentPart(text[j]) {
				j++
			}
			word := text[i:j]
			if ch == '$' && word != dynamicField {
				return fail("unknown name %q (only $meta starts with $)", word)
			}
			i = j
			lastIdent = word
			continue
		case ch >= '0' && ch <= '9' || ch == '.' && i+1 < len(text) && text[i+1] >= '0' && text[i+1] <= '9':
			j := scanNumber(text, i)
			if j < 0 {
				return fail("bad number at offset %d", i)
			}
			i = j
		case ch == '"' || ch == '\'':
			j := i + 1
			for ; j < len(text) && text[j] != ch; j++ {
				switch text[j] {
				case '\\':
					j++
				case '\n', '\r':
					return fail("a string literal spans lines")
				}
			}
			if j >= len(text) {
				return fail("unterminated string literal")
			}
			i = j + 1
		case ch == '{':
			end := strings.IndexByte(text[i:], '}')
			if end < 0 || !identifier.MatchString(text[i+1:i+end]) {
				return fail("a template variable is {name}")
			}
			i += end + 1
		case ch == '(' || ch == '[':
			if ch == '(' && lastIdent != "" && !isKeyword(lastIdent) {
				if readOnly && !filterFunctions[strings.ToLower(lastIdent)] {
					return fail("function %q is not on the read-only allow-list", lastIdent)
				}
			}
			stack = append(stack, ch)
			i++
		case ch == ')' || ch == ']':
			open := byte('(')
			if ch == ']' {
				open = '['
			}
			if len(stack) == 0 || stack[len(stack)-1] != open {
				return fail("unbalanced %q at offset %d", ch, i)
			}
			stack = stack[:len(stack)-1]
			i++
		default:
			n := operatorLen(text[i:])
			if n == 0 {
				return fail("unexpected character %q at offset %d", ch, i)
			}
			i += n
		}
		lastIdent = ""
	}
	if len(stack) > 0 {
		return fail("unbalanced %q", stack[len(stack)-1])
	}
	return nil
}

func isIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isIdentPart(c byte) bool  { return isIdentStart(c) || c >= '0' && c <= '9' }

// isKeyword reports the operator words that may precede a parenthesis
// without being a function call: "a and (b)", "not (a)", "id in (…)",
// "exists (…)". Other words (like, is, null, true…) never do, so they
// count as calls and must be on the allow-list.
func isKeyword(w string) bool {
	switch strings.ToLower(w) {
	case "and", "or", "not", "in", "exists":
		return true
	}
	return false
}

// scanNumber returns the end of the number at text[i:], or -1.
func scanNumber(text string, i int) int {
	j := i
	if strings.HasPrefix(text[i:], "0x") || strings.HasPrefix(text[i:], "0X") {
		j += 2
		start := j
		for j < len(text) && strings.IndexByte("0123456789abcdefABCDEF", text[j]) >= 0 {
			j++
		}
		if j == start {
			return -1
		}
	} else {
		for j < len(text) && text[j] >= '0' && text[j] <= '9' {
			j++
		}
		if j < len(text) && text[j] == '.' {
			j++
			for j < len(text) && text[j] >= '0' && text[j] <= '9' {
				j++
			}
		}
		if j < len(text) && (text[j] == 'e' || text[j] == 'E') {
			j++
			if j < len(text) && (text[j] == '+' || text[j] == '-') {
				j++
			}
			start := j
			for j < len(text) && text[j] >= '0' && text[j] <= '9' {
				j++
			}
			if j == start {
				return -1
			}
		}
	}
	if j < len(text) && isIdentPart(text[j]) {
		return -1
	}
	return j
}

// operatorLen is the length of the operator or separator at the start of
// s, or 0.
func operatorLen(s string) int {
	for _, op := range []string{"==", "!=", "<=", ">=", "&&", "||", "**", "<<", ">>"} {
		if strings.HasPrefix(s, op) {
			return 2
		}
	}
	if strings.IndexByte("<>+-*/%!&|^~,", s[0]) >= 0 {
		return 1
	}
	return 0
}

// exprParams are the template values of an expression, by name.
type exprParams map[string]*schemapb.TemplateValue

// add stores v under a fresh driver-reserved name and returns its
// template reference.
func (p exprParams) add(v *schemapb.TemplateValue) string {
	name := "tobira_f" + strconv.Itoa(len(p))
	for _, taken := p[name]; taken; _, taken = p[name] {
		name += "_"
	}
	p[name] = v
	return "{" + name + "}"
}

// andExpr joins the non-empty parts with "and", each in parentheses.
func andExpr(parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, "("+p+")")
		}
	}
	return strings.Join(out, " and ")
}

// buildFilter translates a driver filter to a Milvus expression, adding
// its values to params as template values: a value never becomes
// expression text, so no Browse value can change the expression. Field
// names are static fields or dynamic keys (written $meta["key"]); a key
// outside the identifier alphabet is refused rather than escaped. Raw is a
// native expression, checked by checkFilter and ANDed with the
// conditions.
func buildFilter(f driver.Filter, s *collSchema, readOnly bool, params exprParams) (string, error) {
	var parts []string
	if f.Raw != "" {
		if err := checkFilter(f.Raw, readOnly); err != nil {
			return "", err
		}
		parts = append(parts, f.Raw)
	}
	for _, c := range f.Conditions {
		e, err := condition(c, s, params)
		if err != nil {
			return "", err
		}
		parts = append(parts, e)
	}
	return andExpr(parts...), nil
}

// fieldRef is how an expression names a result column: a static field by
// name, a dynamic key as $meta["key"]. field is nil for a dynamic key.
func fieldRef(s *collSchema, col string) (string, *fieldInfo, error) {
	if f, ok := s.field(col); ok {
		return f.name, f, nil
	}
	key, _ := s.dynamicKey(col)
	if !s.dynamic {
		return "", nil, driver.Wrap(driver.ErrNotFound, Name, "filter", fmt.Errorf("collection %s has no field %q", s.name, col))
	}
	if !identifier.MatchString(key) {
		return "", nil, driver.Wrap(driver.ErrUnsupported, Name, "filter", fmt.Errorf("dynamic key %q cannot be filtered here (letters, digits and underscores only): use a raw filter", key))
	}
	return dynamicField + `["` + key + `"]`, nil, nil
}

// condition translates one condition.
func condition(c driver.Condition, s *collSchema, params exprParams) (string, error) {
	unsupported := func(why string) (string, error) {
		return "", driver.Wrap(driver.ErrUnsupported, Name, "filter", fmt.Errorf("%s %s: %s", c.Field, c.Op, why))
	}
	ref, f, err := fieldRef(s, c.Field)
	if err != nil {
		return "", err
	}
	if f != nil && f.isVector() {
		return unsupported("vectors cannot be filtered")
	}
	switch c.Op {
	case driver.OpIsNull:
		if f == nil {
			return "not exists " + ref, nil
		}
		return ref + " is null", nil
	case driver.OpNotNull:
		if f == nil {
			return "exists " + ref, nil
		}
		return ref + " is not null", nil
	case driver.OpEq, driver.OpNe, driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
		if c.Value.Type() == driver.TypeList {
			return unsupported("compare with a single value")
		}
		tv, err := templateValue(c.Value)
		if err != nil {
			return "", err
		}
		op := map[driver.Op]string{driver.OpEq: "==", driver.OpNe: "!=", driver.OpLt: "<", driver.OpLe: "<=", driver.OpGt: ">", driver.OpGe: ">="}[c.Op]
		return ref + " " + op + " " + params.add(tv), nil
	case driver.OpIn:
		if c.Value.Type() != driver.TypeList {
			return unsupported("in takes a list")
		}
		tv, err := templateValue(c.Value)
		if err != nil {
			return "", err
		}
		return ref + " in " + params.add(tv), nil
	case driver.OpContains, driver.OpPrefix:
		if f != nil && f.dataType != schemapb.DataType_VarChar && f.dataType != schemapb.DataType_String {
			return unsupported("substring matches need a text field")
		}
		text, ok := c.Value.AsText()
		if !ok || c.Value.Type() != driver.TypeString {
			return unsupported("substring matches take text")
		}
		pattern, ok := likePattern(c.Op == driver.OpPrefix, text)
		if !ok {
			return unsupported(`milvus LIKE cannot match "%" or "\" literally: use a raw filter`)
		}
		return ref + " like " + params.add(&schemapb.TemplateValue{Val: &schemapb.TemplateValue_StringVal{StringVal: pattern}}), nil
	}
	return unsupported("unknown operator")
}

// likePattern is a LIKE pattern matching s as a prefix or a substring.
// Milvus LIKE escapes "_" as "\_", but has no escape for "%", and a
// backslash is literal or an escape depending on what follows it; ok is
// false for a value holding either, which the pattern could not match
// exactly. (Checked against the pinned server by TestFilterValuesAreData.)
func likePattern(prefix bool, s string) (string, bool) {
	if strings.ContainsAny(s, `%\`) {
		return "", false
	}
	esc := strings.ReplaceAll(s, "_", `\_`)
	if prefix {
		return esc + "%", true
	}
	return "%" + esc + "%", true
}
