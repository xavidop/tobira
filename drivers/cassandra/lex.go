package cassandra

import "strings"

// tokKind is the kind of one significant CQL token.
type tokKind uint8

const (
	tWord      tokKind = iota // an unquoted identifier or keyword
	tQuoted                   // a "quoted" identifier
	tString                   // a '…' or $$…$$ string
	tNumber                   // a number, a blob (0x…) or the start of a uuid
	tParam                    // ? or :name
	tPunct                    // any other single byte
	tSemicolon                // ;
)

// token is one significant token: comments and whitespace are dropped.
type token struct {
	kind tokKind
	text string // the raw text
	low  string // the lower-cased text, for words
}

// lexCQL splits CQL text into significant tokens, the way Cassandra's own
// lexer reads it: "--" and "//" line comments (ended by '\n' or '\r'),
// non-nesting "/* */" comments, '…' strings and "…" identifiers with
// doubled-quote escapes, and $$…$$ strings (CQL has no tagged dollar
// quotes and no backslash escapes). It fails closed: bad is non-empty, and
// says why, when the text holds something the guard cannot judge: an
// unterminated literal, identifier or comment, a NUL or other control
// byte, a byte >= 0x80 or a '$' outside a literal (Cassandra would refuse
// those anyway).
func lexCQL(text string) (toks []token, bad string) {
	i := 0
	for i < len(text) {
		c := text[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && strings.HasPrefix(text[i:], "--"), c == '/' && strings.HasPrefix(text[i:], "//"):
			// Cassandra ends a line comment at '\n' or '\r'.
			if j := strings.IndexAny(text[i:], "\n\r"); j >= 0 {
				i += j + 1
			} else {
				i = len(text)
			}
		case c == '/' && strings.HasPrefix(text[i:], "/*"):
			j := strings.Index(text[i+2:], "*/")
			if j < 0 {
				return nil, "unterminated comment"
			}
			i += 2 + j + 2
		case c == '\'' || c == '"':
			end, ok := quotedEnd(text, i, c)
			if !ok {
				if c == '"' {
					return nil, "unterminated quoted identifier"
				}
				return nil, "unterminated string"
			}
			kind := tString
			if c == '"' {
				kind = tQuoted
			}
			toks = append(toks, token{kind: kind, text: text[i:end]})
			i = end
		case c == '$':
			if !strings.HasPrefix(text[i:], "$$") {
				return nil, "a '$' outside a $$ string"
			}
			j := strings.Index(text[i+2:], "$$")
			if j < 0 {
				return nil, "unterminated $$ string"
			}
			end := i + 2 + j + 2
			toks = append(toks, token{kind: tString, text: text[i:end]})
			i = end
		case isLetter(c):
			j := i + 1
			for j < len(text) && (isLetter(text[j]) || isDigit(text[j]) || text[j] == '_') {
				j++
			}
			w := text[i:j]
			toks = append(toks, token{kind: tWord, text: w, low: strings.ToLower(w)})
			i = j
		case isDigit(c):
			j := i + 1
			for j < len(text) && (isLetter(text[j]) || isDigit(text[j]) || text[j] == '_' || text[j] == '.') {
				j++
			}
			toks = append(toks, token{kind: tNumber, text: text[i:j]})
			i = j
		case c == '?':
			toks = append(toks, token{kind: tParam, text: "?"})
			i++
		case c == ':' && i+1 < len(text) && isLetter(text[i+1]):
			j := i + 2
			for j < len(text) && (isLetter(text[j]) || isDigit(text[j]) || text[j] == '_') {
				j++
			}
			toks = append(toks, token{kind: tParam, text: text[i:j]})
			i = j
		case c == ';':
			toks = append(toks, token{kind: tSemicolon, text: ";"})
			i++
		case c < 0x20 || c == 0x7f:
			return nil, "a control character outside a literal"
		case c >= 0x80:
			return nil, "a non-ASCII character outside a literal"
		default:
			toks = append(toks, token{kind: tPunct, text: text[i : i+1]})
			i++
		}
	}
	return toks, ""
}

// quotedEnd returns the end of the literal opened by q at text[i], where a
// doubled q stands for itself.
func quotedEnd(text string, i int, q byte) (int, bool) {
	for j := i + 1; j < len(text); j++ {
		if text[j] != q {
			continue
		}
		if j+1 < len(text) && text[j+1] == q {
			j++
			continue
		}
		return j + 1, true
	}
	return 0, false
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
