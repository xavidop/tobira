// Package sqlscan is the SQL tokenizer shared by the SQL drivers' read-only
// guards and by driverutil/sqlcomplete. It only lexes: it splits text into
// words, quoted identifiers, strings, comments, numbers and punctuation the
// way one dialect does, and never decides what a statement does. The guards
// keep their allow-lists and refusals in their own driver.
//
// Scan never fails. Text that ends inside a string, quoted identifier,
// comment or dollar quote yields one final Unterminated token, so a guard
// can refuse it (fail closed) or treat it as running to the end.
//
// The predefined dialects leave PrefixedStrings and UnicodeEscapes off. With
// them off, N'…', x'…', _utf8'…' and U&"…" scan as a Word, then Punct "&"
// for U&, then the quoted token: the same text, split differently. The
// guards were written and tested against that split. Merging the prefix would change their
// decisions without making them safer: MySQL's DESCRIBE check would start
// accepting DESCRIBE t x'ab', and PostgreSQL's ROLLBACK … TO check relies on
// the separate U word. The postgres guard decodes U&"…" UESCAPE itself.
package sqlscan

import "strings"

// Kind is the kind of a Token.
type Kind uint8

const (
	// Word is a bare identifier or keyword.
	Word Kind = iota
	// QuotedIdent is a quoted identifier: "…", `…` or […], by dialect.
	QuotedIdent
	// String is a string literal: '…', "…" (MySQL), E'…', $tag$…$tag$ and
	// the prefixed forms, by dialect.
	String
	// Comment is a line comment (without its newline) or a block comment.
	Comment
	// ExecComment is a block comment the server runs as SQL (/*! … */).
	ExecComment
	// Semicolon is one ';'.
	Semicolon
	// Punct is any other single byte.
	Punct
	// Number is a numeric literal.
	Number
	// Space is a run of whitespace.
	Space
	// Unterminated is a string, quoted identifier, comment or dollar quote
	// missing its terminator. It runs to the end of the text, so it is always
	// the last token.
	Unterminated
)

var kindNames = [...]string{
	Word: "Word", QuotedIdent: "QuotedIdent", String: "String", Comment: "Comment",
	ExecComment: "ExecComment", Semicolon: "Semicolon", Punct: "Punct", Number: "Number",
	Space: "Space", Unterminated: "Unterminated",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "Kind(?)"
}

// Token is one lexical unit: the half-open byte range [Start, End) of the
// scanned text. Tokens are never empty and cover the text contiguously.
type Token struct {
	Kind       Kind
	Start, End int
}

// Scan splits s into tokens under dialect d and calls fn with each one in
// order, stopping early when fn returns false.
func Scan(d Dialect, s string, fn func(Token) bool) {
	for i := 0; i < len(s); {
		kind, end := d.next(s, i)
		if !fn(Token{Kind: kind, Start: i, End: end}) {
			return
		}
		i = end
	}
}

// bom is the UTF-8 byte order mark.
const bom = "\xEF\xBB\xBF"

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func (d Dialect) isSpaceAt(s string, i int) (int, bool) {
	switch {
	case isSpace(s[i]):
		return 1, true
	case d.BOMSpace && strings.HasPrefix(s[i:], bom):
		return len(bom), true
	}
	return 0, false
}

func (d Dialect) wordByte(c byte) bool {
	return isASCIILetter(c) || isDigit(c) || c == '_' || d.IdentDollar && c == '$' || d.IdentHighBit && c >= 0x80
}

func (d Dialect) wordStart(c byte) bool {
	return isASCIILetter(c) || c == '_' || d.IdentHighBit && c >= 0x80 || d.IdentDollarStart && c == '$'
}

// next returns the kind and end of the token starting at s[i].
func (d Dialect) next(s string, i int) (Kind, int) {
	n := len(s)
	c := s[i]
	if w, ok := d.isSpaceAt(s, i); ok {
		j := i + w
		for j < n {
			w, ok := d.isSpaceAt(s, j)
			if !ok {
				break
			}
			j += w
		}
		return Space, j
	}
	switch {
	case c == '-' && i+1 < n && s[i+1] == '-' && (!d.DashCommentSpace || i+2 == n || s[i+2] <= ' ' || s[i+2] == 0x7f),
		c == '#' && d.HashComments:
		if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
			return Comment, i + j
		}
		return Comment, n
	case c == '/' && i+1 < n && s[i+1] == '*':
		return d.blockComment(s, i)
	case c == '\'':
		return quoted(s, i, '\'', d.BackslashEscapes, String)
	case c == '"' && d.DoubleQuoteStrings:
		return quoted(s, i, '"', d.BackslashEscapes, String)
	case c == '"':
		return quoted(s, i, '"', d.IdentBackslashEscapes, QuotedIdent)
	case c == '`' && d.Backticks:
		return quoted(s, i, '`', d.IdentBackslashEscapes, QuotedIdent)
	case c == '[' && d.Brackets && d.BracketDoubling:
		return bracketDoubled(s, i)
	case c == '[' && d.Brackets:
		return quoted(s, i, ']', false, QuotedIdent)
	case c == ';':
		return Semicolon, i + 1
	case isDigit(c):
		return Number, d.numberEnd(s, i)
	}
	if c == '$' && d.DollarQuotes {
		if kind, end, ok := d.dollar(s, i); ok {
			return kind, end
		}
	}
	if d.wordStart(c) {
		return d.word(s, i)
	}
	return Punct, i + 1
}

// quoted scans the quoted run opened at s[i] and closed by closer. A doubled
// closer (except ']') continues the run; with backslash a backslash escapes
// the next byte.
func quoted(s string, i int, closer byte, backslash bool, kind Kind) (Kind, int) {
	for j := i + 1; j < len(s); j++ {
		switch c := s[j]; {
		case backslash && c == '\\':
			j++
		case c == closer:
			if closer != ']' && j+1 < len(s) && s[j+1] == closer {
				j++
				continue
			}
			return kind, j + 1
		}
	}
	return Unterminated, len(s)
}

// blockComment scans the comment opened by "/*" at s[i].
func (d Dialect) blockComment(s string, i int) (Kind, int) {
	depth := 1
	for j := i + 2; j < len(s); j++ {
		switch {
		case d.NestedComments && s[j] == '/' && j+1 < len(s) && s[j+1] == '*':
			depth++
			j++
		case s[j] == '*' && j+1 < len(s) && s[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				if d.ExecComments && isExecBody(s[i+2:j-1]) {
					return ExecComment, j + 1
				}
				return Comment, j + 1
			}
		}
	}
	return Unterminated, len(s)
}

// isExecBody reports whether a block comment's body makes MySQL or MariaDB
// run it: it opens with "!" or "M!", or holds such a comment.
func isExecBody(body string) bool {
	return strings.HasPrefix(body, "!") || strings.HasPrefix(body, "M!") ||
		strings.Contains(body, "/*!") || strings.Contains(body, "/*M!")
}

// dollar scans the dollar quote opened at s[i]. ok is false when s[i] opens
// no tag ($1, a lone $).
func (d Dialect) dollar(s string, i int) (kind Kind, end int, ok bool) {
	j := i + 1
	for j < len(s) && (isASCIILetter(s[j]) || s[j] == '_' || d.DollarTagHighBit && s[j] >= 0x80 || j > i+1 && isDigit(s[j])) {
		j++
	}
	if j >= len(s) || s[j] != '$' {
		return 0, 0, false
	}
	tag := s[i : j+1]
	if k := strings.Index(s[j+1:], tag); k >= 0 {
		return String, j + 1 + k + len(tag), true
	}
	return Unterminated, len(s), true
}

func (d Dialect) numberEnd(s string, i int) int {
	j := i + 1
	for j < len(s) {
		c := s[j]
		switch {
		case isDigit(c), d.NumberIdentChars && d.wordByte(c):
			j++
		case c == '.' && (d.NumberDot == DotAlways || d.NumberDot == DotBeforeDigit && j+1 < len(s) && isDigit(s[j+1])):
			j++
		default:
			return j
		}
	}
	return j
}

// word scans the word at s[i], or the string or quoted identifier it
// prefixes (E'…', U&"…", N'…', _utf8'…').
func (d Dialect) word(s string, i int) (Kind, int) {
	j := i + 1
	for j < len(s) && d.wordByte(s[j]) {
		j++
	}
	if j == len(s) {
		return Word, j
	}
	w, next := s[i:j], s[j]
	switch {
	case d.EStrings && (w == "e" || w == "E") && next == '\'':
		return quoted(s, j, '\'', true, String)
	case d.UnicodeEscapes && (w == "u" || w == "U") && next == '&' && j+1 < len(s) && s[j+1] == '\'':
		return quoted(s, j+1, '\'', false, String)
	case d.UnicodeEscapes && (w == "u" || w == "U") && next == '&' && j+1 < len(s) && s[j+1] == '"':
		return quoted(s, j+1, '"', false, QuotedIdent)
	case d.PrefixedStrings && isStringPrefix(w) && (next == '\'' || next == '"' && d.DoubleQuoteStrings):
		return quoted(s, j, next, d.BackslashEscapes, String)
	}
	return Word, j
}

// isStringPrefix reports whether w can prefix a string: N, X or B (national,
// hex and bit literals) or a charset introducer such as _utf8mb4.
func isStringPrefix(w string) bool {
	switch w {
	case "n", "N", "x", "X", "b", "B":
		return true
	}
	return len(w) > 1 && w[0] == '_'
}
