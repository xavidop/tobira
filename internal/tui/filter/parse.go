// Package filter is the grid's filter bar: a one-line input parsed into a
// driver.Filter.
package filter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/textutil"
)

// ParseError is a syntax or name error at a byte offset of the input.
type ParseError struct {
	Pos int // 0-based byte offset
	Msg string
}

func (e *ParseError) Error() string { return fmt.Sprintf("%s (at %d)", e.Msg, e.Pos) }

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tNumber
	tString
	tOp
)

type token struct {
	kind tokKind
	text string // identifier or operator text, number text, or the unescaped string
	pos  int
}

var numberRE = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// lexer produces tokens on demand, so an error earlier in the input is
// reported before a bad character later in it.
type lexer struct {
	s   string
	pos int
}

func (l *lexer) next() (token, error) {
	for l.pos < len(l.s) {
		r, n := utf8.DecodeRuneInString(l.s[l.pos:])
		if !unicode.IsSpace(r) {
			break
		}
		l.pos += n
	}
	start := l.pos
	if start >= len(l.s) {
		return token{kind: tEOF, pos: len(l.s)}, nil
	}
	r, n := utf8.DecodeRuneInString(l.s[start:])
	switch {
	case r == '\'':
		return l.str(start)
	case r == '-' || (r >= '0' && r <= '9'):
		return l.number(start)
	case unicode.IsLetter(r) || r == '_':
		l.pos += n
		for l.pos < len(l.s) {
			r, n := utf8.DecodeRuneInString(l.s[l.pos:])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '.' {
				break
			}
			l.pos += n
		}
		return token{kind: tIdent, text: l.s[start:l.pos], pos: start}, nil
	}
	for _, op := range []string{"!=", "<=", ">=", "^=", "=", "<", ">", "~"} {
		if strings.HasPrefix(l.s[start:], op) {
			l.pos += len(op)
			return token{kind: tOp, text: op, pos: start}, nil
		}
	}
	return token{}, &ParseError{Pos: start, Msg: fmt.Sprintf("unexpected character %q", r)}
}

func (l *lexer) str(start int) (token, error) {
	var sb strings.Builder
	for i := start + 1; i < len(l.s); i++ {
		if l.s[i] != '\'' {
			sb.WriteByte(l.s[i])
			continue
		}
		if i+1 < len(l.s) && l.s[i+1] == '\'' {
			sb.WriteByte('\'')
			i++
			continue
		}
		l.pos = i + 1
		return token{kind: tString, text: sb.String(), pos: start}, nil
	}
	return token{}, &ParseError{Pos: start, Msg: "unterminated string (close it with ')"}
}

func (l *lexer) number(start int) (token, error) {
	i := start
	if l.s[i] == '-' {
		i++
	}
	for i < len(l.s) && (l.s[i] >= '0' && l.s[i] <= '9' || l.s[i] == '.' || l.s[i] == 'e' || l.s[i] == 'E' ||
		((l.s[i] == '+' || l.s[i] == '-') && (l.s[i-1] == 'e' || l.s[i-1] == 'E'))) {
		i++
	}
	l.pos = i
	text := l.s[start:i]
	if !numberRE.MatchString(text) {
		return token{}, &ParseError{Pos: start, Msg: fmt.Sprintf("invalid number %q", text)}
	}
	return token{kind: tNumber, text: text, pos: start}, nil
}

type parser struct {
	lex  lexer
	cols []driver.Column
	tok  token
}

func (p *parser) advance() error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

func (p *parser) isKeyword(kw string) bool {
	return p.tok.kind == tIdent && strings.EqualFold(p.tok.text, kw)
}

// Parse turns the filter bar's text into a driver.Filter. Conditions are
// "field op value" joined by "and"; a leading "!" passes the rest through
// as Filter.Raw. Empty text is the zero Filter. Errors are *ParseError.
func Parse(s string, cols []driver.Column) (driver.Filter, error) {
	trimmed := strings.TrimLeftFunc(s, unicode.IsSpace)
	if strings.HasPrefix(trimmed, "!") {
		raw := strings.TrimSpace(trimmed[1:])
		if raw == "" {
			return driver.Filter{}, &ParseError{Pos: len(s) - len(trimmed) + 1, Msg: "expected a native filter after '!'"}
		}
		return driver.Filter{Raw: raw}, nil
	}
	p := &parser{lex: lexer{s: s}, cols: cols}
	if err := p.advance(); err != nil {
		return driver.Filter{}, err
	}
	if p.tok.kind == tEOF {
		return driver.Filter{}, nil
	}
	var f driver.Filter
	for {
		c, err := p.condition()
		if err != nil {
			return driver.Filter{}, err
		}
		f.Conditions = append(f.Conditions, c)
		if p.tok.kind == tEOF {
			return f, nil
		}
		if !p.isKeyword("and") {
			return driver.Filter{}, &ParseError{Pos: p.tok.pos, Msg: "expected 'and' or the end of the filter"}
		}
		if err := p.advance(); err != nil {
			return driver.Filter{}, err
		}
	}
}

func (p *parser) condition() (driver.Condition, error) {
	if p.tok.kind != tIdent {
		return driver.Condition{}, &ParseError{Pos: p.tok.pos, Msg: "expected a column name"}
	}
	field, err := p.resolve(p.tok)
	if err != nil {
		return driver.Condition{}, err
	}
	if err := p.advance(); err != nil {
		return driver.Condition{}, err
	}
	if p.isKeyword("is") {
		return p.isNull(field)
	}
	if p.tok.kind != tOp {
		return driver.Condition{}, &ParseError{Pos: p.tok.pos, Msg: "expected an operator (= != < <= > >= ~ ^= or is null)"}
	}
	op := map[string]driver.Op{
		"=": driver.OpEq, "!=": driver.OpNe, "<": driver.OpLt, "<=": driver.OpLe,
		">": driver.OpGt, ">=": driver.OpGe, "~": driver.OpContains, "^=": driver.OpPrefix,
	}[p.tok.text]
	if err := p.advance(); err != nil {
		return driver.Condition{}, err
	}
	vt := p.tok
	v, isNull, err := p.value()
	if err != nil {
		return driver.Condition{}, err
	}
	if err := p.advance(); err != nil {
		return driver.Condition{}, err
	}
	if isNull {
		switch op {
		case driver.OpEq:
			return driver.Condition{Field: field, Op: driver.OpIsNull}, nil
		case driver.OpNe:
			return driver.Condition{Field: field, Op: driver.OpNotNull}, nil
		}
		return driver.Condition{}, &ParseError{Pos: vt.pos, Msg: "null can only be compared with = or != (or use is null)"}
	}
	if (op == driver.OpContains || op == driver.OpPrefix) && vt.kind != tString {
		return driver.Condition{}, &ParseError{Pos: vt.pos, Msg: "contains/prefix need a quoted string (quote it: 'text')"}
	}
	return driver.Condition{Field: field, Op: op, Value: v}, nil
}

// isNull parses "is [not] null"; the current token is "is".
func (p *parser) isNull(field string) (driver.Condition, error) {
	if err := p.advance(); err != nil {
		return driver.Condition{}, err
	}
	op := driver.OpIsNull
	if p.isKeyword("not") {
		op = driver.OpNotNull
		if err := p.advance(); err != nil {
			return driver.Condition{}, err
		}
	}
	if !p.isKeyword("null") {
		return driver.Condition{}, &ParseError{Pos: p.tok.pos, Msg: "expected null or not null after 'is'"}
	}
	if err := p.advance(); err != nil {
		return driver.Condition{}, err
	}
	return driver.Condition{Field: field, Op: op}, nil
}

// value reads the literal at the current token without consuming it.
func (p *parser) value() (v driver.Value, isNull bool, err error) {
	t := p.tok
	switch {
	case t.kind == tString:
		return driver.String(t.text), false, nil
	case t.kind == tNumber:
		return number(t.text), false, nil
	case p.isKeyword("true"):
		return driver.Bool(true), false, nil
	case p.isKeyword("false"):
		return driver.Bool(false), false, nil
	case p.isKeyword("null"):
		return driver.Value{}, true, nil
	case t.kind == tIdent:
		return v, false, &ParseError{Pos: t.pos, Msg: fmt.Sprintf("expected a value; quote text as '%s'", t.text)}
	}
	return v, false, &ParseError{Pos: t.pos, Msg: "expected a value"}
}

// number maps a validated literal: integers to Int (Decimal when too big),
// a dotted literal to Decimal with its exact text, exponent forms to Float.
func number(text string) driver.Value {
	if strings.ContainsAny(text, "eE") {
		f, _ := strconv.ParseFloat(text, 64)
		return driver.Float(f)
	}
	if strings.Contains(text, ".") {
		return driver.Decimal(text)
	}
	if i, err := strconv.ParseInt(text, 10, 64); err == nil {
		return driver.Int(i)
	}
	return driver.Decimal(text)
}

// resolve matches a field name to a column: exactly, else by a unique
// case-insensitive match, else an error naming the closest column.
func (p *parser) resolve(t token) (string, error) {
	for _, c := range p.cols {
		if c.Name == t.text {
			return c.Name, nil
		}
	}
	var fold []string
	for _, c := range p.cols {
		if strings.EqualFold(c.Name, t.text) {
			fold = append(fold, c.Name)
		}
	}
	if len(fold) == 1 {
		return fold[0], nil
	}
	if len(fold) > 1 {
		return "", &ParseError{Pos: t.pos, Msg: fmt.Sprintf("ambiguous column %q (matches %s)", t.text, strings.Join(fold, ", "))}
	}
	msg := fmt.Sprintf("unknown column %q", t.text)
	best, bestDist := "", 4
	lower := strings.ToLower(t.text)
	for _, c := range p.cols {
		d := textutil.Levenshtein(lower, strings.ToLower(c.Name))
		if d < bestDist && d < utf8.RuneCountInString(t.text) {
			best, bestDist = c.Name, d
		}
	}
	if best != "" {
		msg += fmt.Sprintf(" (did you mean %q?)", best)
	}
	return "", &ParseError{Pos: t.pos, Msg: msg}
}
