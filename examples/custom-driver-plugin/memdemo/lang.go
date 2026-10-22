package memdemo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// statement is one parsed query.
type statement struct {
	verb  string // "select", "delete", "reset" or "sleep"
	coll  string
	where *condition    // nil = every row
	sleep time.Duration // for sleep
}

// maxSleep caps the sleep statement.
const maxSleep = 60 * time.Second

// condition is the WHERE clause: field = value.
type condition struct {
	field string
	value driver.Value
}

// writes reports whether the statement changes data.
func (s statement) writes() bool { return s.verb != "select" && s.verb != "sleep" }

// syntaxError is the error for a query the language does not accept.
func syntaxError(format string, args ...any) error {
	return driver.Wrap(driver.ErrSyntax, Name, "query", fmt.Errorf(format, args...))
}

// token is one lexeme. quoted is set for a string literal, whose text is
// the unquoted string.
type token struct {
	text   string
	quoted bool
}

// lex splits a query into words, string literals and the symbols * and =.
func lex(text string) ([]token, error) {
	var out []token
	for i := 0; i < len(text); {
		switch c := text[i]; c {
		case ' ', '\t', '\n', '\r':
			i++
		case '*', '=', ';':
			out = append(out, token{text: string(c)})
			i++
		case '\'':
			var b strings.Builder
			i++
			for {
				if i >= len(text) {
					return nil, syntaxError("unterminated string")
				}
				if text[i] == '\'' {
					if i+1 < len(text) && text[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(text[i])
				i++
			}
			out = append(out, token{text: b.String(), quoted: true})
		default:
			j := i
			for j < len(text) && !strings.ContainsRune(" \t\n\r*=;'", rune(text[j])) {
				j++
			}
			out = append(out, token{text: text[i:j]})
			i = j
		}
	}
	return out, nil
}

// parse parses one statement, binding $N to params.
func parse(text string, params []driver.Value) (statement, error) {
	toks, err := lex(text)
	if err != nil {
		return statement{}, err
	}
	if n := len(toks); n > 0 && toks[n-1].text == ";" && !toks[n-1].quoted {
		toks = toks[:n-1]
	}
	p := &parser{toks: toks, params: params}
	var st statement
	switch verb := strings.ToLower(p.word()); verb {
	case "select":
		if err := p.expect("*", "from"); err != nil {
			return st, err
		}
		st.verb = verb
	case "delete":
		if err := p.expect("from"); err != nil {
			return st, err
		}
		st.verb = verb
	case "reset":
		st.verb = verb
	case "sleep":
		st.verb = verb
		return st, p.duration(&st)
	case "":
		return st, syntaxError("empty query")
	default:
		return st, syntaxError("unknown statement %q: want select, delete, reset or sleep", verb)
	}
	if st.coll = p.word(); st.coll == "" {
		return st, syntaxError("%s: missing collection name", st.verb)
	}
	if st.verb != "reset" && strings.EqualFold(p.peek(), "where") {
		p.pos++
		if st.where, err = p.condition(); err != nil {
			return st, err
		}
	}
	if p.pos < len(p.toks) {
		return st, syntaxError("unexpected %q", p.toks[p.pos].text)
	}
	return st, nil
}

// duration parses the rest of a sleep statement: one Go duration, at
// most maxSleep.
func (p *parser) duration(st *statement) error {
	w := p.word()
	d, err := time.ParseDuration(w)
	if err != nil || d < 0 || d > maxSleep {
		return syntaxError("sleep: want a duration from 0s to %s, got %q", maxSleep, w)
	}
	if p.pos < len(p.toks) {
		return syntaxError("unexpected %q", p.toks[p.pos].text)
	}
	st.sleep = d
	return nil
}

// pause is the sleep statement: it waits d, or until ctx ends.
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return driver.Wrap(nil, Name, "query", ctx.Err()) // errors.Is(err, context.Canceled) holds
	}
}

type parser struct {
	toks   []token
	pos    int
	params []driver.Value
}

// peek returns the next unquoted word, or "".
func (p *parser) peek() string {
	if p.pos >= len(p.toks) || p.toks[p.pos].quoted {
		return ""
	}
	return p.toks[p.pos].text
}

// word consumes the next unquoted word, or returns "".
func (p *parser) word() string {
	w := p.peek()
	if w != "" {
		p.pos++
	}
	return w
}

// expect consumes the given words (case-insensitive).
func (p *parser) expect(words ...string) error {
	for _, w := range words {
		if got := p.word(); !strings.EqualFold(got, w) {
			return syntaxError("want %q, got %q", w, got)
		}
	}
	return nil
}

func (p *parser) condition() (*condition, error) {
	field := p.word()
	if field == "" {
		return nil, syntaxError("where: missing field")
	}
	if err := p.expect("="); err != nil {
		return nil, err
	}
	if p.pos >= len(p.toks) {
		return nil, syntaxError("where %s =: missing value", field)
	}
	v, err := p.literal(p.toks[p.pos])
	if err != nil {
		return nil, err
	}
	p.pos++
	return &condition{field: field, value: v}, nil
}

// literal turns a token into a value.
func (p *parser) literal(t token) (driver.Value, error) {
	if t.quoted {
		return driver.String(t.text), nil
	}
	s := t.text
	switch strings.ToLower(s) {
	case "true":
		return driver.Bool(true), nil
	case "false":
		return driver.Bool(false), nil
	case "null":
		return driver.Null(), nil
	}
	if rest, ok := strings.CutPrefix(s, "$"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n < 1 || n > len(p.params) {
			return driver.Value{}, syntaxError("parameter %s: %d parameters given", s, len(p.params))
		}
		return p.params[n-1], nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return driver.Int(n), nil
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return driver.Decimal(s), nil
	}
	return driver.Value{}, syntaxError("bad value %q: quote strings with '", s)
}

// result is what a statement produced.
type result struct {
	cols     []driver.Column
	rows     []driver.Row
	affected int64 // -1 for a select
}

// exec runs st against d. A write changes d in place, so the caller passes
// a copy it can throw away on error.
func exec(d data, st statement) (result, error) {
	ref := driver.CollectionRef{Namespace: Namespace, Name: st.coll}
	t, err := d.table("query", ref)
	if err != nil {
		return result{}, err
	}
	match := func(driver.Row) bool { return true }
	if st.where != nil {
		i, ok := t.col(st.where.field)
		if !ok {
			return result{}, driver.Wrap(driver.ErrNotFound, Name, "query", fmt.Errorf("field %q", st.where.field))
		}
		match = func(r driver.Row) bool { return equal(r[i], st.where.value) }
	}
	switch st.verb {
	case "select":
		res := result{cols: resultColumns(t, ref), affected: -1}
		for _, r := range t.rows {
			if match(r) {
				res.rows = append(res.rows, r)
			}
		}
		return res, nil
	case "delete":
		kept := t.rows[:0:0]
		for _, r := range t.rows {
			if !match(r) {
				kept = append(kept, r)
			}
		}
		n := int64(len(t.rows) - len(kept))
		t.rows = kept
		return result{affected: n}, nil
	case "reset":
		fresh, ok := seed()[st.coll]
		if !ok {
			return result{}, driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("no seed data"))
		}
		d[st.coll] = fresh
		return result{affected: int64(len(fresh.rows))}, nil
	}
	return result{}, syntaxError("unknown statement %q", st.verb)
}

// resultColumns describes t's columns as result columns from ref.
func resultColumns(t *table, ref driver.CollectionRef) []driver.Column {
	cols := make([]driver.Column, len(t.cols))
	for i, c := range t.cols {
		c.Source = &ref
		cols[i] = c
	}
	return cols
}
