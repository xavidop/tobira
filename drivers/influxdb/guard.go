package influxdb

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// The read-only guard. InfluxDB 3's query API plans no DML or DDL, so the
// server itself never writes; the allow-list below is a second, fail-closed
// layer checked before any text is sent. A statement must be exactly one
// SELECT, WITH … SELECT (SQL only), SHOW, or EXPLAIN of a query, with no
// INTO anywhere and, in SQL, no unquoted write keyword. Text the
// lexer cannot read the way the server does (an unterminated string or
// comment, a NUL byte, a nested block comment, a dollar-quoted string, a
// backtick identifier, non-ASCII text outside a string or quoted
// identifier) is refused rather than guessed at.

type tokKind uint8

const (
	tokWord   tokKind = iota // keyword, unquoted identifier, number or placeholder
	tokQuoted                // "quoted identifier"
	tokString                // 'string literal'
	tokRegex                 // InfluxQL /regex/
	tokSemi                  // ;
	tokPunct                 // any other symbol
)

type token struct {
	kind tokKind
	text string
}

// lexResult is what lex found.
type lexResult struct {
	tokens   []token
	comments bool // the text holds at least one comment
}

// errLex is a lexing failure; checkStatement reports it as ErrReadOnly.
var errLex = errors.New("cannot be read safely")

// lex splits text into tokens following the server's lexical rules for
// lang. For InfluxQL, regex says whether a '/' after =~, !~, FROM or a comma
// starts a regex literal; checkStatement lexes InfluxQL both ways because
// that choice depends on the grammar.
func lex(text string, lang language, regex bool) (lexResult, error) {
	var res lexResult
	if strings.IndexByte(text, 0) >= 0 {
		return res, fmt.Errorf("%w: NUL byte", errLex)
	}
	s := text
	i := 0
	add := func(k tokKind, t string) { res.tokens = append(res.tokens, token{k, t}) }
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && strings.HasPrefix(s[i:], "--"):
			res.comments = true
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				end = len(s) - i
			}
			if !isASCII(s[i : i+end]) {
				return res, fmt.Errorf("%w: non-ASCII text in a comment", errLex)
			}
			i += end
		case c == '/' && lang == langInfluxQL && regex && regexContext(res.tokens):
			end, err := scanQuoted(s, i, '/', true, false)
			if err != nil {
				return res, fmt.Errorf("%w: unterminated regex", errLex)
			}
			add(tokRegex, s[i:end])
			i = end
		case c == '/' && strings.HasPrefix(s[i:], "/*"):
			res.comments = true
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return res, fmt.Errorf("%w: unterminated comment", errLex)
			}
			if strings.Contains(s[i+2:i+2+end], "/*") {
				return res, fmt.Errorf("%w: nested block comment", errLex)
			}
			if !isASCII(s[i+2 : i+2+end]) {
				return res, fmt.Errorf("%w: non-ASCII text in a comment", errLex)
			}
			i += 2 + end + 2
		case c == '\'':
			// InfluxQL strings take backslash escapes; plain SQL strings do not.
			end, err := scanQuoted(s, i, '\'', lang == langInfluxQL, lang == langSQL)
			if err != nil {
				return res, fmt.Errorf("%w: unterminated string", errLex)
			}
			add(tokString, s[i:end])
			i = end
		case c == '"':
			end, err := scanQuoted(s, i, '"', lang == langInfluxQL, lang == langSQL)
			if err != nil {
				return res, fmt.Errorf("%w: unterminated quoted identifier", errLex)
			}
			add(tokQuoted, s[i:end])
			i = end
		case c >= 0x80:
			// The server splits words on Unicode spaces such as U+00A0;
			// rather than model Unicode, refuse it outside literals.
			return res, fmt.Errorf("%w: non-ASCII text outside a string or quoted identifier (double-quote such names)", errLex)
		case c == '`':
			return res, fmt.Errorf("%w: backtick identifiers are not supported, use double quotes", errLex)
		case c == '$' && lang == langSQL && dollarQuote(s[i+1:]):
			return res, fmt.Errorf("%w: dollar-quoted strings are not supported", errLex)
		case c == ';':
			add(tokSemi, ";")
			i++
		case (c == '=' || c == '!') && i+1 < len(s) && s[i+1] == '~':
			add(tokPunct, s[i:i+2])
			i += 2
		case isWordByte(c) || c == '$':
			start := i
			i++
			for i < len(s) && isWordByte(s[i]) {
				i++
			}
			w := s[start:i]
			if lang == langSQL && i < len(s) && s[i] == '\'' && (w == "E" || w == "e") {
				// E'…': a PostgreSQL-style escape string, backslash escapes.
				end, err := scanQuoted(s, i, '\'', true, true)
				if err != nil {
					return res, fmt.Errorf("%w: unterminated string", errLex)
				}
				add(tokString, s[start:end])
				i = end
				continue
			}
			add(tokWord, w)
		default:
			add(tokPunct, s[i:i+1])
			i++
		}
	}
	return res, nil
}

// scanQuoted returns the index just past the literal that opens at s[i]
// with quote q. backslash makes '\' escape the next byte; doubled makes a
// doubled quote stand for one.
func scanQuoted(s string, i int, q byte, backslash, doubled bool) (int, error) {
	for j := i + 1; j < len(s); j++ {
		switch {
		case backslash && s[j] == '\\':
			j++
		case s[j] == q:
			if doubled && j+1 < len(s) && s[j+1] == q {
				j++
				continue
			}
			return j + 1, nil
		}
	}
	return 0, errLex
}

// dollarQuote reports whether rest (the text after a '$') opens a
// dollar-quoted string as sqlparser reads one: "$$…" or "$tag$…", where the
// tag is any run of word characters, digits and non-ASCII included
// ("$1$", "$é$"). A placeholder such as $1 or $name, not closed by a '$',
// is not one.
func dollarQuote(rest string) bool {
	for j := 0; j < len(rest); j++ {
		switch {
		case rest[j] == '$':
			return true
		case !isWordByte(rest[j]) && rest[j] < 0x80:
			return false
		}
	}
	return false
}

// regexContext reports whether an InfluxQL '/' here may start a regex:
// after =~, !~, a comma or '(' (a call such as count(/x/)), or after
// SELECT, DISTINCT, FROM or BY (GROUP BY /x/). Listing a position too many
// only adds refusals, since both readings must pass.
func regexContext(toks []token) bool {
	if len(toks) == 0 {
		return false
	}
	last := toks[len(toks)-1]
	switch {
	case last.kind == tokPunct && (last.text == "=~" || last.text == "!~" || last.text == "," || last.text == "("):
		return true
	case last.kind == tokWord:
		switch strings.ToUpper(last.text) {
		case "SELECT", "DISTINCT", "FROM", "BY":
			return true
		}
	}
	return false
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// isWordByte reports whether c can be part of an unquoted word: ASCII
// letters, digits and '_'. Non-ASCII text is only allowed inside literals.
func isWordByte(c byte) bool { return isLetter(c) || c >= '0' && c <= '9' || c == '_' }

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// checkStatement enforces the allow-list on one statement in lang. A refusal
// is ErrReadOnly; text with no statement at all is ErrSyntax.
func checkStatement(text string, lang language) error {
	modes := []bool{false}
	if lang == langInfluxQL {
		modes = []bool{true, false} // a '/' may start a regex or divide
	}
	for _, regex := range modes {
		res, err := lex(text, lang, regex)
		if err != nil {
			return refuse(err.Error())
		}
		if err := checkTokens(res.tokens, lang); err != nil {
			return err
		}
	}
	return nil
}

// writeWords are SQL keywords that start a write. sqlparser accepts DML as
// the body of a CTE or a derived table, so in SQL they are refused anywhere
// unquoted; a column with such a name must be double-quoted.
var writeWords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "COPY": true,
	"CREATE": true, "DROP": true, "ALTER": true, "TRUNCATE": true,
}

// checkTokens applies the allow-list to a lexed statement in lang.
func checkTokens(toks []token, lang language) error {
	for len(toks) > 0 && toks[len(toks)-1].kind == tokSemi {
		toks = toks[:len(toks)-1] // trailing semicolons
	}
	if len(toks) == 0 {
		return driver.Wrap(driver.ErrSyntax, Name, "query", errors.New("empty statement"))
	}
	for _, t := range toks {
		switch {
		case t.kind == tokSemi:
			return refuse("only one statement is allowed")
		case t.kind == tokWord && strings.EqualFold(t.text, "INTO"):
			return refuse("INTO is not allowed")
		}
	}
	switch strings.ToUpper(word(toks[0])) {
	case "SHOW":
		return nil
	case "SELECT", "WITH":
		return checkQuery(toks, lang)
	case "EXPLAIN":
		rest := toks[1:]
		for len(rest) > 0 {
			switch strings.ToUpper(word(rest[0])) {
			case "ANALYZE", "VERBOSE":
				rest = rest[1:]
				continue
			case "FORMAT":
				if len(rest) < 2 || word(rest[1]) == "" {
					return refuse("EXPLAIN FORMAT needs a format name")
				}
				rest = rest[2:]
				continue
			}
			break
		}
		if len(rest) == 0 {
			return refuse("only EXPLAIN of a query is allowed")
		}
		switch strings.ToUpper(word(rest[0])) {
		case "SELECT", "WITH":
			return checkQuery(rest, lang)
		}
		return refuse("only EXPLAIN of a query is allowed")
	}
	return refuse("only SELECT, WITH … SELECT, SHOW and EXPLAIN are allowed")
}

// checkQuery checks a query starting with SELECT or WITH. In SQL no write
// keyword may appear unquoted, and a WITH must be a list of CTEs, each a
// query in parentheses, followed by a SELECT. InfluxQL has no WITH.
func checkQuery(toks []token, lang language) error {
	if lang == langSQL {
		for _, t := range toks {
			if t.kind == tokWord && writeWords[strings.ToUpper(t.text)] {
				return refuse(strings.ToUpper(t.text) + " is not allowed (double-quote a column of that name)")
			}
		}
	}
	if !strings.EqualFold(word(toks[0]), "WITH") {
		return nil
	}
	if lang != langSQL {
		return refuse("only SELECT, SHOW and EXPLAIN are allowed")
	}
	rest, err := skipCTEs(toks[1:])
	if err != nil {
		return err
	}
	if len(rest) == 0 || !strings.EqualFold(word(rest[0]), "SELECT") {
		return refuse("the statement after WITH must be a SELECT")
	}
	return nil
}

// skipCTEs consumes "[RECURSIVE] name [(cols)] AS [NOT] [MATERIALIZED]
// (query) [, …]" and returns what follows. Each CTE body must start with
// SELECT, VALUES, WITH (checked recursively) or a parenthesis.
func skipCTEs(toks []token) ([]token, error) {
	bad := func(why string) ([]token, error) { return nil, refuse("WITH: " + why) }
	if len(toks) > 0 && strings.EqualFold(word(toks[0]), "RECURSIVE") {
		toks = toks[1:]
	}
	for {
		if len(toks) == 0 || (toks[0].kind != tokWord && toks[0].kind != tokQuoted) {
			return bad("expected a CTE name")
		}
		toks = toks[1:]
		if len(toks) > 0 && isPunct(toks[0], "(") { // column list
			_, after, ok := group(toks)
			if !ok {
				return bad("unbalanced parentheses")
			}
			toks = after
		}
		if len(toks) == 0 || !strings.EqualFold(word(toks[0]), "AS") {
			return bad("expected AS")
		}
		toks = toks[1:]
		if len(toks) > 0 && strings.EqualFold(word(toks[0]), "NOT") {
			toks = toks[1:]
		}
		if len(toks) > 0 && strings.EqualFold(word(toks[0]), "MATERIALIZED") {
			toks = toks[1:]
		}
		if len(toks) == 0 || !isPunct(toks[0], "(") {
			return bad("expected a parenthesized query")
		}
		body, after, ok := group(toks)
		if !ok {
			return bad("unbalanced parentheses")
		}
		if err := checkCTEBody(body); err != nil {
			return nil, err
		}
		toks = after
		if len(toks) > 0 && isPunct(toks[0], ",") {
			toks = toks[1:]
			continue
		}
		return toks, nil
	}
}

// checkCTEBody checks the tokens inside a CTE's parentheses.
func checkCTEBody(body []token) error {
	if len(body) == 0 {
		return refuse("WITH: empty CTE")
	}
	switch {
	case isPunct(body[0], "("):
		inner, after, ok := group(body)
		if !ok {
			return refuse("WITH: unbalanced parentheses")
		}
		if len(after) == 0 {
			return checkCTEBody(inner)
		}
		return nil // (SELECT …) UNION (SELECT …) and the like
	case strings.EqualFold(word(body[0]), "SELECT"), strings.EqualFold(word(body[0]), "VALUES"):
		return nil
	case strings.EqualFold(word(body[0]), "WITH"):
		rest, err := skipCTEs(body[1:])
		if err != nil {
			return err
		}
		if len(rest) == 0 || !strings.EqualFold(word(rest[0]), "SELECT") {
			return refuse("WITH: the statement after WITH must be a SELECT")
		}
		return nil
	}
	return refuse("WITH: a CTE must be a query")
}

// group splits toks, which start with "(", into the tokens inside the
// matching ")" and those after it. ok is false when it never closes.
func group(toks []token) (inside, after []token, ok bool) {
	depth := 0
	for i, t := range toks {
		switch {
		case isPunct(t, "("):
			depth++
		case isPunct(t, ")"):
			depth--
			if depth == 0 {
				return toks[1:i], toks[i+1:], true
			}
		}
	}
	return nil, nil, false
}

func isPunct(t token, p string) bool { return t.kind == tokPunct && t.text == p }

// word returns t's text when t is a word, else "".
func word(t token) string {
	if t.kind != tokWord {
		return ""
	}
	return t.text
}

func refuse(why string) error {
	return driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("statement refused by the read-only guard: "+why))
}

// checkRawFilter validates a native SQL filter fragment before it is ANDed
// into a generated statement: it must lex, hold no comment and no ';', and
// keep its parentheses balanced without ever closing more than it opened,
// so it cannot escape the parentheses it is wrapped in.
func checkRawFilter(raw string) error {
	bad := func(why string) error {
		return driver.Wrap(driver.ErrSyntax, Name, "filter", errors.New("raw filter "+why))
	}
	res, err := lex(raw, langSQL, false)
	if err != nil {
		return bad(err.Error())
	}
	if res.comments {
		return bad("may not contain comments")
	}
	depth := 0
	for _, t := range res.tokens {
		switch {
		case t.kind == tokSemi:
			return bad("may not contain ';'")
		case t.kind == tokPunct && t.text == "(":
			depth++
		case t.kind == tokPunct && t.text == ")":
			depth--
			if depth < 0 {
				return bad("closes a parenthesis it did not open")
			}
		}
	}
	if depth != 0 {
		return bad("has unbalanced parentheses")
	}
	return nil
}
