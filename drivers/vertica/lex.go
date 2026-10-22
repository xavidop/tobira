package vertica

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// tokKind classifies a Vertica SQL token.
type tokKind uint8

const (
	tokWord   tokKind = iota // keyword or unquoted name (letters, digits, '_', '$')
	tokQuoted                // "quoted name"
	tokString                // '…', E'…', $$…$$ or $tag$…$tag$
	tokNumber                // numeric literal, 0x hex included
	tokPunct                 // any other single byte
)

var tokKindNames = [...]string{
	tokWord: "word", tokQuoted: "quoted", tokString: "string", tokNumber: "number", tokPunct: "punct",
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
	errUTF8         = errors.New("the statement is not valid UTF-8")
	errNested       = errors.New("a comment opener inside a block comment")
	errLoneCR       = errors.New("a carriage return inside a -- comment")
	errGlued        = errors.New("a number glued to a word (write a space between them)")
)

// lex splits s by Vertica's lexical rules, which follow PostgreSQL's:
//
//   - comments: "--" to the end of the line, "/* … */";
//   - strings: '…' where ” is a quote; E'…' where a backslash also escapes
//     the next byte; $$…$$ and $tag$…$tag$, which hold anything up to the
//     closing tag;
//   - quoted names: "…", where "" is a quote;
//   - words: an ASCII letter or '_', then letters, digits, '_' and '$';
//   - numbers: digits, an optional fraction and exponent, or 0x and hex
//     digits (see numberEnd).
//
// It reads in the direction that refuses rather than accepts. It is an
// error, never a guess, when the text is not valid UTF-8, holds a NUL, an
// unterminated string, name or comment, a block comment that holds another
// "/*" (Vertica nests them; the guard does not try to), a carriage return
// inside a "--" comment that is not part of CRLF (whether it ends the
// comment is the server's call), or a number glued to a word ("1delete",
// "0x1insert": where the server ends the number decides which keyword it
// sees). Any byte >= 0x80 outside quotes is punctuation, so a server that
// treats some code point as a separator never sees a word the guard did
// not.
func lex(s string) ([]token, error) {
	if !utf8.ValidString(s) {
		return nil, errUTF8
	}
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == 0:
			return nil, errNUL
		case c <= ' ' || c == 0x7f:
			i++
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			j := i + 2
			for j < len(s) && s[j] != '\n' {
				if s[j] == 0 {
					return nil, errNUL
				}
				if s[j] == '\r' && (j+1 == len(s) || s[j+1] != '\n') {
					return nil, errLoneCR
				}
				j++
			}
			i = j
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, errUnterminated
			}
			body := s[i+2 : i+2+end]
			if strings.Contains(body, "/*") {
				return nil, errNested
			}
			if strings.IndexByte(body, 0) >= 0 {
				return nil, errNUL
			}
			i += 2 + end + 2
		case c == '\'':
			end, err := stringEnd(s, i+1, false)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tokString, s[i:end], i})
			i = end
		case (c == 'E' || c == 'e') && i+1 < len(s) && s[i+1] == '\'':
			end, err := stringEnd(s, i+2, true)
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
		case c == '$' && dollarTag(s, i) != "":
			tag := dollarTag(s, i)
			end := strings.Index(s[i+len(tag):], tag)
			if end < 0 {
				return nil, errUnterminated
			}
			j := i + len(tag) + end + len(tag)
			if strings.IndexByte(s[i:j], 0) >= 0 {
				return nil, errNUL
			}
			toks = append(toks, token{tokString, s[i:j], i})
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
			if j < len(s) && isWordByte(s[j]) {
				return nil, errGlued
			}
			toks = append(toks, token{tokNumber, s[i:j], i})
			i = j
		default:
			toks = append(toks, token{tokPunct, s[i : i+1], i})
			i++
		}
	}
	return toks, nil
}

// numberEnd returns the index just past the number at s[i]: 0x (or 0X)
// and hex digits, or digits, an optional '.' and digits, and an exponent
// only when a digit, or a sign and a digit, follows the e. lex refuses a
// word byte right after it.
func numberEnd(s string, i int) int {
	if s[i] == '0' && i+2 < len(s) && (s[i+1] == 'x' || s[i+1] == 'X') && isHex(s[i+2]) {
		j := i + 2
		for j < len(s) && isHex(s[j]) {
			j++
		}
		return j
	}
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

// stringEnd returns the index just past the string whose body starts at
// s[j]: ” is a quote, and with escapes (E'…') a backslash escapes the
// next byte.
func stringEnd(s string, j int, escapes bool) (int, error) {
	for ; j < len(s); j++ {
		switch s[j] {
		case 0:
			return 0, errNUL
		case '\\':
			if escapes {
				j++
			}
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
// s[j]: "" is a quote.
func quotedEnd(s string, j int) (int, error) {
	for ; j < len(s); j++ {
		switch s[j] {
		case 0:
			return 0, errNUL
		case '"':
			if j+1 < len(s) && s[j+1] == '"' {
				j++
				continue
			}
			return j + 1, nil
		}
	}
	return 0, errUnterminated
}

// dollarTag returns the dollar-quote opener at s[i] ("$$" or "$tag$", the
// tag an ASCII letter or '_' then letters, digits and '_'), or "". A '$'
// that continues a word (a$b) never reaches here.
func dollarTag(s string, i int) string {
	j := i + 1
	if j < len(s) && s[j] == '$' {
		return "$$"
	}
	if j >= len(s) || !isWordStart(s[j]) {
		return ""
	}
	for j < len(s) && (isWordStart(s[j]) || isDigit(s[j])) {
		j++
	}
	if j < len(s) && s[j] == '$' {
		return s[i : j+1]
	}
	return ""
}

// unquote returns the name a "…" token stands for.
func unquote(quoted string) string {
	return strings.ReplaceAll(quoted[1:len(quoted)-1], `""`, `"`)
}

func isDigit(c byte) bool     { return c >= '0' && c <= '9' }
func isHex(c byte) bool       { return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
func isWordStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isWordByte(c byte) bool  { return isWordStart(c) || isDigit(c) || c == '$' }
