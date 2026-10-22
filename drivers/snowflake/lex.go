package snowflake

import (
	"errors"
	"strings"
)

// tokKind classifies a Snowflake SQL token.
type tokKind uint8

const (
	tokWord   tokKind = iota // keyword or unquoted name (letters, digits, '_', '$')
	tokQuoted                // "quoted name"
	tokString                // '…' or $$…$$
	tokNumber                // numeric literal
	tokVar                   // $name or $1: a session variable or stage column
	tokPunct                 // any other single byte
)

var tokKindNames = [...]string{
	tokWord: "word", tokQuoted: "quoted", tokString: "string", tokNumber: "number", tokVar: "var", tokPunct: "punct",
}

func (k tokKind) String() string { return tokKindNames[k] }

// token is one significant token: whitespace and comments are dropped.
type token struct {
	kind tokKind
	text string // the raw text
	pos  int    // its byte offset
}

var (
	errUnterminated = errors.New("unterminated string, quoted name or comment")
	errNUL          = errors.New("NUL byte in the statement")
	errNested       = errors.New("a comment opener inside a block comment")
)

// lex splits s by Snowflake's lexical rules
// (https://docs.snowflake.com/en/sql-reference/constructs and the string
// literal and identifier pages):
//
//   - comments: "--" and "//" to the end of the line, "/* … */";
//   - strings: '…', where a backslash escapes the next byte and ” is a
//     quote, and $$…$$, which holds anything up to the next $$;
//   - quoted names: "…", where "" is a quote;
//   - words: an ASCII letter or '_', then letters, digits, '_' and '$'
//     (SYSTEM$WAIT is one word); $name and $1 are variables;
//   - numbers: digits, an optional fraction and an exponent (see
//     numberEnd); 0x1F is the number 0 and the word x1F.
//
// It reads in the direction that refuses rather than accepts: a line
// comment also ends at a carriage return, any control byte is whitespace,
// a byte >= 0x80 outside quotes is punctuation, and a block comment that
// holds another "/*" is an error, whether or not the server nests them. So
// a server that splits text more finely never sees a word the guard did
// not. Text it cannot lex (an unterminated string, name, comment or $$
// string, or a NUL byte) is an error.
func lex(s string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == 0:
			return nil, errNUL
		case c <= ' ' || c == 0x7f:
			i++
		case (c == '-' || c == '/') && i+1 < len(s) && s[i+1] == c:
			end := strings.IndexAny(s[i+2:], "\r\n")
			if end < 0 {
				return toks, nil
			}
			i += 2 + end
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, errUnterminated
			}
			if strings.Contains(s[i+2:i+2+end], "/*") {
				return nil, errNested
			}
			i += 2 + end + 2
		case c == '\'':
			end, err := stringEnd(s, i+1)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tokString, s[i:end], i})
			i = end
		case c == '"':
			end, err := quotedEnd(s, i+1)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tokQuoted, s[i:end], i})
			i = end
		case c == '$' && i+1 < len(s) && s[i+1] == '$':
			end := strings.Index(s[i+2:], "$$")
			if end < 0 {
				return nil, errUnterminated
			}
			toks = append(toks, token{tokString, s[i : i+2+end+2], i})
			i += 2 + end + 2
		case c == '$' && i+1 < len(s) && isWordByte(s[i+1]) && s[i+1] != '$':
			j := i + 1
			for j < len(s) && isWordByte(s[j]) && s[j] != '$' {
				j++
			}
			toks = append(toks, token{tokVar, s[i:j], i})
			i = j
		case isWordStart(c):
			j := i
			for j < len(s) && isWordByte(s[j]) {
				j++
			}
			toks = append(toks, token{tokWord, s[i:j], i})
			i = j
		case isDigit(c) || (c == '.' && i+1 < len(s) && isDigit(s[i+1])):
			j := numberEnd(s, i)
			toks = append(toks, token{tokNumber, s[i:j], i})
			i = j
		default:
			toks = append(toks, token{tokPunct, s[i : i+1], i})
			i++
		}
	}
	return toks, nil
}

// numberEnd returns the index just past the number at s[i]: digits, an
// optional '.' and digits, and an exponent only when a digit, or a sign and
// a digit, follows the e. Nothing else belongs to it: letters after it
// (1DELETE, 0xe, 1e->>) start a separate token, so a keyword or the flow
// operator glued to a digit stays visible.
func numberEnd(s string, i int) int {
	j := i
	for j < len(s) && isDigit(s[j]) {
		j++
	}
	if j < len(s) && s[j] == '.' {
		j++
		for j < len(s) && isDigit(s[j]) {
			j++
		}
	}
	if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
		k := j + 1
		if k < len(s) && (s[k] == '+' || s[k] == '-') {
			k++
		}
		if k < len(s) && isDigit(s[k]) {
			for j = k; j < len(s) && isDigit(s[j]); j++ {
			}
		}
	}
	return j
}

// stringEnd returns the index just past the '…' string whose body starts
// at s[j]: a backslash escapes the next byte and ” is a quote.
func stringEnd(s string, j int) (int, error) {
	for ; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '\'':
			if j+1 < len(s) && s[j+1] == '\'' {
				j++
				continue
			}
			return j + 1, nil
		}
	}
	return 0, errUnterminated
}

// quotedEnd returns the index just past the "…" name whose body starts at
// s[j]: "" is a quote, and a backslash is an ordinary byte.
func quotedEnd(s string, j int) (int, error) {
	for ; j < len(s); j++ {
		if s[j] == '"' {
			if j+1 < len(s) && s[j+1] == '"' {
				j++
				continue
			}
			return j + 1, nil
		}
	}
	return 0, errUnterminated
}

// unquote returns the name a "…" token stands for.
func unquote(quoted string) string {
	return strings.ReplaceAll(quoted[1:len(quoted)-1], `""`, `"`)
}

func isDigit(c byte) bool     { return c >= '0' && c <= '9' }
func isWordStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isWordByte(c byte) bool  { return isWordStart(c) || isDigit(c) || c == '$' }
