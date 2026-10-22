package dynamodb

import (
	"errors"
	"fmt"
	"strings"
)

// tokKind is the kind of a PartiQL token.
type tokKind int

const (
	tokWord        tokKind = iota // keyword or bare identifier
	tokQuotedIdent                // "identifier" ("" escapes a quote)
	tokString                     // 'text' ('' escapes a quote)
	tokIon                        // `ion literal`
	tokNumber                     // 12, 1.5, 2e10
	tokParam                      // ?
	tokPunct                      // operators and punctuation
)

// token is one lexed PartiQL token. text is the word, the punctuation, or
// the unquoted content of a quoted identifier or string.
type token struct {
	kind tokKind
	text string
	pos  int
}

func (t token) isWord(w string) bool { return t.kind == tokWord && strings.EqualFold(t.text, w) }

// twoCharPunct are the operators lexed as one token.
var twoCharPunct = map[string]bool{"<=": true, ">=": true, "<>": true, "!=": true, "||": true, "<<": true, ">>": true}

// lex splits a PartiQL statement into tokens. Comments (-- to the end of
// the line, /* … */) are skipped. It fails closed: an unterminated string,
// identifier, Ion literal or comment, a block comment containing "/*"
// (which another parser might read as nested), and any byte outside ASCII
// that is not inside a quoted token are errors.
func lex(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			end := strings.IndexAny(s[i:], "\n\r")
			if end < 0 {
				i = len(s)
			} else {
				i += end
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated comment at offset %d", i)
			}
			if strings.Contains(s[i+2:i+2+end], "/*") {
				return nil, fmt.Errorf("nested comment at offset %d", i)
			}
			i += 2 + end + 2
		case c == '\'' || c == '"':
			text, n, err := quoted(s[i:], c)
			if err != nil {
				return nil, fmt.Errorf("%w at offset %d", err, i)
			}
			kind := tokString
			if c == '"' {
				kind = tokQuotedIdent
			}
			out = append(out, token{kind: kind, text: text, pos: i})
			i += n
		case c == '`':
			end := strings.IndexByte(s[i+1:], '`')
			if end < 0 {
				return nil, fmt.Errorf("unterminated Ion literal at offset %d", i)
			}
			out = append(out, token{kind: tokIon, text: s[i+1 : i+1+end], pos: i})
			i += end + 2
		case isWordStart(c):
			j := i + 1
			for j < len(s) && isWordPart(s[j]) {
				j++
			}
			out = append(out, token{kind: tokWord, text: s[i:j], pos: i})
			i = j
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			j := i + 1
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' ||
				(s[j] == 'e' || s[j] == 'E') ||
				(s[j] == '+' || s[j] == '-') && (s[j-1] == 'e' || s[j-1] == 'E')) {
				j++
			}
			out = append(out, token{kind: tokNumber, text: s[i:j], pos: i})
			i = j
		case c == '?':
			out = append(out, token{kind: tokParam, text: "?", pos: i})
			i++
		case c >= 0x80 || c < 0x20:
			return nil, fmt.Errorf("unexpected byte 0x%02x at offset %d", c, i)
		default:
			if i+1 < len(s) && twoCharPunct[s[i:i+2]] {
				out = append(out, token{kind: tokPunct, text: s[i : i+2], pos: i})
				i += 2
				continue
			}
			out = append(out, token{kind: tokPunct, text: s[i : i+1], pos: i})
			i++
		}
	}
	return out, nil
}

// quoted reads a quoted token starting at s[0] == q, where a doubled quote
// escapes one. It returns the content and the bytes consumed.
func quoted(s string, q byte) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != q {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == q {
			b.WriteByte(q)
			i++
			continue
		}
		return b.String(), i + 1, nil
	}
	if q == '"' {
		return "", 0, errors.New("unterminated quoted identifier")
	}
	return "", 0, errors.New("unterminated string")
}

func isWordStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isWordPart(c byte) bool  { return isWordStart(c) || c >= '0' && c <= '9' || c == '$' }

// shape is what selectShape reads from a SELECT: the table, the index
// (FROM "table"."index") and the projected attribute names when every
// projection item is a plain name (nil for * or any expression).
type shape struct {
	table, index string
	columns      []string
}

// selectShape reads the projection and the FROM target of a SELECT. It
// returns the zero shape for anything else.
func selectShape(toks []token) shape {
	if len(toks) == 0 || !toks[0].isWord("SELECT") {
		return shape{}
	}
	depth, from := 0, -1
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		if t.kind == tokPunct {
			switch t.text {
			case "(", "[", "{", "<<":
				depth++
			case ")", "]", "}", ">>":
				depth--
			}
		}
		if depth == 0 && t.isWord("FROM") {
			from = i
			break
		}
	}
	if from < 0 || from+1 >= len(toks) {
		return shape{}
	}
	var sh shape
	name := func(t token) (string, bool) {
		return t.text, t.kind == tokWord || t.kind == tokQuotedIdent
	}
	var ok bool
	if sh.table, ok = name(toks[from+1]); !ok {
		return shape{}
	}
	if from+3 < len(toks) && toks[from+2].kind == tokPunct && toks[from+2].text == "." {
		if sh.index, ok = name(toks[from+3]); !ok {
			return shape{}
		}
	}
	proj := toks[1:from]
	if len(proj) == 1 && proj[0].kind == tokPunct && proj[0].text == "*" {
		return sh
	}
	if len(proj)%2 == 0 {
		return sh // empty, or a trailing comma
	}
	var cols []string
	for i, t := range proj {
		if i%2 == 1 {
			if t.kind != tokPunct || t.text != "," {
				return sh
			}
			continue
		}
		n, ok := name(t)
		if !ok {
			return sh
		}
		cols = append(cols, n)
	}
	sh.columns = cols
	return sh
}
