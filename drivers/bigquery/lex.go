package bigquery

import (
	"errors"
	"strings"
)

// tokKind classifies a GoogleSQL token.
type tokKind uint8

const (
	tokWord   tokKind = iota // keyword or unquoted name
	tokIdent                 // `quoted name`
	tokString                // string or bytes literal, any quoting
	tokNumber                // numeric literal
	tokParam                 // @name or @@name
	tokPunct                 // any other single byte
)

type token struct {
	kind tokKind
	text string
	pos  int
}

// lexResult is a lexed text: its tokens, without whitespace and comments,
// and whether a comment selects legacy SQL.
type lexResult struct {
	toks      []token
	legacySQL bool
}

var (
	errUnterminated = errors.New("unterminated string, quoted name or comment")
	errNUL          = errors.New("NUL byte in the statement")
)

// lex splits s by GoogleSQL's lexical rules
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/lexical):
//
//   - comments: "--" and "#" to the end of the line, "/* … */" (not
//     nesting);
//   - strings: '…', "…", ”'…”' and """…""", with an optional r, b, rb or
//     br prefix (any case); a backslash pairs with the next byte in every
//     form, raw strings included, as the server finds the end of a string;
//   - quoted names: `…`, with backslash escapes;
//   - words: ASCII letters, digits and '_'; parameters @name and @@name.
//
// It reads in the direction that refuses rather than accepts: a line
// comment also ends at a carriage return, any control byte is whitespace,
// and a byte >= 0x80 outside quotes is punctuation, so a server that
// splits text more finely never sees a word the guard did not. Text it
// cannot lex (an unterminated string, name or comment, or a NUL byte) is
// an error.
func lex(s string) (lexResult, error) {
	var r lexResult
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == 0:
			return lexResult{}, errNUL
		case c <= ' ' || c == 0x7f:
			i++
		case c == '#' || (c == '-' && i+1 < len(s) && s[i+1] == '-'):
			start := i + 1
			if c == '-' {
				start++
			}
			end := strings.IndexAny(s[start:], "\r\n")
			if end < 0 {
				end = len(s) - start
			}
			if isLegacyDirective(s[start : start+end]) {
				r.legacySQL = true
			}
			i = start + end
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return lexResult{}, errUnterminated
			}
			if isLegacyDirective(s[i+2 : i+2+end]) {
				r.legacySQL = true
			}
			i += 2 + end + 2
		case c == '\'' || c == '"':
			end, err := stringEnd(s, i)
			if err != nil {
				return lexResult{}, err
			}
			r.toks = append(r.toks, token{tokString, s[i:end], i})
			i = end
		case c == '`':
			end, err := quotedEnd(s, i+1, '`')
			if err != nil {
				return lexResult{}, err
			}
			r.toks = append(r.toks, token{tokIdent, s[i:end], i})
			i = end
		case isWordStart(c):
			j := i
			for j < len(s) && isWordByte(s[j]) {
				j++
			}
			if j < len(s) && (s[j] == '\'' || s[j] == '"') && isStringPrefix(s[i:j]) {
				end, err := stringEnd(s, j)
				if err != nil {
					return lexResult{}, err
				}
				r.toks = append(r.toks, token{tokString, s[i:end], i})
				i = end
				continue
			}
			r.toks = append(r.toks, token{tokWord, s[i:j], i})
			i = j
		case isDigit(c) || (c == '.' && i+1 < len(s) && isDigit(s[i+1])):
			j := i + 1
			for j < len(s) && (isWordByte(s[j]) || s[j] == '.' ||
				((s[j] == '+' || s[j] == '-') && (s[j-1] == 'e' || s[j-1] == 'E'))) {
				j++
			}
			r.toks = append(r.toks, token{tokNumber, s[i:j], i})
			i = j
		case c == '@':
			j := i + 1
			if j < len(s) && s[j] == '@' {
				j++
			}
			k := j
			for k < len(s) && isWordByte(s[k]) {
				k++
			}
			if k == j {
				r.toks = append(r.toks, token{tokPunct, "@", i})
				i++
				continue
			}
			r.toks = append(r.toks, token{tokParam, s[i:k], i})
			i = k
		default:
			r.toks = append(r.toks, token{tokPunct, s[i : i+1], i})
			i++
		}
	}
	return r, nil
}

// stringEnd returns the index just past the string literal whose opening
// quote is at s[i]: single or triple quoted.
func stringEnd(s string, i int) (int, error) {
	q := s[i]
	if i+2 < len(s) && s[i+1] == q && s[i+2] == q {
		triple := strings.Repeat(string(q), 3)
		for j := i + 3; j < len(s); j++ {
			if s[j] == '\\' {
				j++
				continue
			}
			if strings.HasPrefix(s[j:], triple) {
				return j + 3, nil
			}
		}
		return 0, errUnterminated
	}
	return quotedEnd(s, i+1, q)
}

// quotedEnd returns the index just past the closing quote of a body that
// starts at s[j], a backslash escaping the next byte.
func quotedEnd(s string, j int, q byte) (int, error) {
	for ; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case q:
			return j + 1, nil
		}
	}
	return 0, errUnterminated
}

func isStringPrefix(w string) bool {
	switch strings.ToLower(w) {
	case "r", "b", "rb", "br":
		return true
	}
	return false
}

// isLegacyDirective reports whether a comment body is the #legacySQL
// directive, which switches the query to legacy SQL and its other lexical
// rules.
func isLegacyDirective(body string) bool {
	body = strings.TrimLeft(body, " \t")
	return len(body) >= len("legacysql") && strings.EqualFold(body[:len("legacysql")], "legacysql")
}

func isDigit(c byte) bool     { return c >= '0' && c <= '9' }
func isWordStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isWordByte(c byte) bool  { return isWordStart(c) || isDigit(c) }
