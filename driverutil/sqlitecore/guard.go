package sqlitecore

import (
	"errors"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// utf8BOM is the UTF-8 byte order mark, which SQLite's tokenizer skips as
// whitespace.
const utf8BOM = "\xEF\xBB\xBF"

// The guards below lex with driverutil/sqlscan's SQLite dialect, shared
// with the other SQL drivers and with completion. Each primitive scans from a
// byte offset, as the guards walk the text.

// firstToken returns the first token of text[i:], with offsets into text.
// ok is false at the end of the text.
func firstToken(text string, i int) (tok sqlscan.Token, ok bool) {
	sqlscan.Scan(sqlscan.SQLite(), text[i:], func(t sqlscan.Token) bool {
		tok, ok = sqlscan.Token{Kind: t.Kind, Start: i + t.Start, End: i + t.End}, true
		return false
	})
	return tok, ok
}

// isBlank reports whether t is whitespace (a UTF-8 BOM included) or a
// comment; an unterminated block comment runs to the end of the text.
func isBlank(text string, t sqlscan.Token) bool {
	switch t.Kind {
	case sqlscan.Space, sqlscan.Comment:
		return true
	case sqlscan.Unterminated:
		return text[t.Start] == '/'
	}
	return false
}

// skipSpace returns the index of the first byte in text[i:] that is neither
// whitespace, a UTF-8 BOM, nor inside a comment.
func skipSpace(text string, i int) int {
	end := len(text)
	sqlscan.Scan(sqlscan.SQLite(), text[i:], func(t sqlscan.Token) bool {
		if isBlank(text[i:], t) {
			return true
		}
		end = i + t.Start
		return false
	})
	return end
}

// word reads an identifier-like word ([A-Za-z0-9_]+) at text[i:], returning
// it upper-cased and the index after it, or "" and i when there is none.
func word(text string, i int) (string, int) {
	t, ok := firstToken(text, i)
	if !ok || t.Kind != sqlscan.Word && t.Kind != sqlscan.Number {
		return "", i
	}
	return asciiUpper(text[t.Start:t.End]), t.End
}

// quotedBody returns the content of a terminated quoted token ('…', "…",
// `…` or […]) with doubled quotes undone.
func quotedBody(raw string) string {
	body := raw[1 : len(raw)-1]
	if raw[0] == '[' {
		return body
	}
	q := raw[:1]
	return strings.ReplaceAll(body, q+q, q)
}

func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// firstKeyword returns the upper-cased first keyword of text and the index
// after it.
func firstKeyword(text string) (string, int) { return word(text, skipSpace(text, 0)) }

// introspectionPragmas are the read pragmas that may take one parenthesised
// argument (a table or index name) on the read-only path.
var introspectionPragmas = map[string]bool{
	"TABLE_INFO": true, "TABLE_XINFO": true, "INDEX_LIST": true, "INDEX_INFO": true, "INDEX_XINFO": true,
	"FOREIGN_KEY_LIST": true,
}

// readPragmas are the pragmas allowed without an argument on the read-only
// path: each one only reports state when called bare.
var readPragmas = map[string]bool{
	"DATABASE_LIST": true, "TABLE_LIST": true, "COMPILE_OPTIONS": true, "COLLATION_LIST": true,
	"FUNCTION_LIST": true, "MODULE_LIST": true, "PRAGMA_LIST": true, "FOREIGN_KEY_CHECK": true,
	"QUERY_ONLY": true, "USER_VERSION": true, "SCHEMA_VERSION": true, "APPLICATION_ID": true,
	"DATA_VERSION": true, "ENCODING": true, "PAGE_SIZE": true, "PAGE_COUNT": true, "MAX_PAGE_COUNT": true,
	"FREELIST_COUNT": true, "JOURNAL_MODE": true, "AUTO_VACUUM": true, "FOREIGN_KEYS": true,
	"BUSY_TIMEOUT": true, "CACHE_SIZE": true, "SOFT_HEAP_LIMIT": true, "HARD_HEAP_LIMIT": true,
	"TEMP_STORE_DIRECTORY": true,
}

// ident reads one identifier at text[i:], bare or quoted with "…", '…', `…`
// or […], and returns it unquoted and upper-cased. A quoted name must be
// terminated and ASCII.
func ident(text string, i int) (string, int, bool) {
	t, ok := firstToken(text, i)
	if !ok {
		return "", i, false
	}
	switch t.Kind {
	case sqlscan.String, sqlscan.QuotedIdent:
		body := quotedBody(text[t.Start:t.End])
		return asciiUpper(body), t.End, isASCII(body)
	case sqlscan.Unterminated:
		if text[t.Start] != '/' {
			return "", len(text), false
		}
	case sqlscan.Word, sqlscan.Number:
		return asciiUpper(text[t.Start:t.End]), t.End, true
	}
	return "", i, false
}

// pragmaName parses "[schema.]name" after the PRAGMA keyword.
func pragmaName(text string, i int) (string, int, bool) {
	name, j, ok := ident(text, skipSpace(text, i))
	if !ok {
		return "", j, false
	}
	k := skipSpace(text, j)
	if k < len(text) && text[k] == '.' {
		return ident(text, skipSpace(text, k+1))
	}
	return name, j, true
}

// errNotReadStatement explains an ErrReadOnly from CheckReadOnly.
var errNotReadStatement = errors.New("only SELECT, WITH, VALUES, EXPLAIN and read-only PRAGMA statements are allowed on a read-only connection")

// CheckReadOnly is the read-only path's allow-list: after any number of
// EXPLAIN [QUERY PLAN] prefixes the statement must start with SELECT, WITH or
// VALUES, or be a bare read pragma (see readPragma). Anything else, including
// text it cannot parse, returns ErrReadOnly before execution.
func CheckReadOnly(name, text string) error {
	kw, i := firstKeyword(text)
	for kw == "EXPLAIN" { // SQLite applies pragma side effects at compile time
		if w, j := word(text, skipSpace(text, i)); w == "QUERY" {
			w2, j2 := word(text, skipSpace(text, j))
			if w2 != "PLAN" {
				break // not a valid prefix: kw stays EXPLAIN and is refused
			}
			i = j2
		}
		kw, i = word(text, skipSpace(text, i))
	}
	switch kw {
	case "SELECT", "WITH", "VALUES":
		return nil
	case "PRAGMA":
		if readPragma(text, i) {
			return nil
		}
	}
	return driver.Wrap(driver.ErrReadOnly, name, "query", errNotReadStatement)
}

// readPragma reports whether text[i:], following the PRAGMA keyword, is
// "[schema.]name" for a name in readPragmas or introspectionPragmas,
// optionally followed by one parenthesised name argument for an
// introspection pragma, then an optional ';' and nothing else.
func readPragma(text string, i int) bool {
	name, j, ok := pragmaName(text, i)
	if !ok || !readPragmas[name] && !introspectionPragmas[name] {
		return false
	}
	j = skipSpace(text, j)
	if j < len(text) && text[j] == '(' {
		if !introspectionPragmas[name] {
			return false
		}
		if j, ok = pragmaArg(text, skipSpace(text, j+1)); !ok {
			return false
		}
		if j = skipSpace(text, j); j >= len(text) || text[j] != ')' {
			return false
		}
		j = skipSpace(text, j+1)
	}
	if j < len(text) && text[j] == ';' {
		j = skipSpace(text, j+1)
	}
	return j == len(text)
}

// pragmaArg reads a table or index name at text[i:]: a bare ASCII word or a
// terminated quoted name ("…", '…', `…`, […]) with any content.
func pragmaArg(text string, i int) (int, bool) {
	t, ok := firstToken(text, i)
	if !ok {
		return i, false
	}
	switch t.Kind {
	case sqlscan.String, sqlscan.QuotedIdent, sqlscan.Word, sqlscan.Number:
		return t.End, true
	case sqlscan.Unterminated:
		if text[t.Start] != '/' {
			return len(text), false
		}
	}
	return i, false
}

// CheckSingle returns ErrSyntax when text holds more than one statement. It
// skips string literals ('…', "…", `…`, […]) and comments; one trailing ';'
// is allowed, followed only by comments and plain ASCII whitespace (a
// vertical tab or a BOM there counts as another statement).
func CheckSingle(name, text string) error {
	seenSemi, multi := false, false
	sqlscan.Scan(sqlscan.SQLite(), text, func(t sqlscan.Token) bool {
		raw := text[t.Start:t.End]
		oddSpace := t.Kind == sqlscan.Space && (strings.IndexByte(raw, '\v') >= 0 || strings.Contains(raw, utf8BOM))
		if isBlank(text, t) && (!seenSemi || !oddSpace) {
			return true
		}
		if seenSemi {
			multi = true
			return false
		}
		seenSemi = t.Kind == sqlscan.Semicolon
		return true
	})
	if multi {
		return driver.Wrap(driver.ErrSyntax, name, "query", errors.New("multiple statements are not supported yet"))
	}
	return nil
}

// ReturnsRows reports whether text starts with a keyword whose statements
// never change rows, so no change counting is needed. WITH is not listed: it
// can front an INSERT.
func ReturnsRows(text string) bool {
	kw, _ := firstKeyword(text)
	switch kw {
	case "SELECT", "VALUES", "EXPLAIN", "PRAGMA":
		return true
	}
	return false
}

// IsTxControl reports whether text starts a statement that ends or nests
// transactions: COMMIT, END, BEGIN and ROLLBACK (but not ROLLBACK TO a
// savepoint). Empty statements and comments before it are skipped.
func IsTxControl(text string) bool {
	i := skipSpace(text, 0)
	for i < len(text) && text[i] == ';' {
		i = skipSpace(text, i+1)
	}
	kw, j := word(text, i)
	switch kw {
	case "COMMIT", "END", "BEGIN":
		return true
	case "ROLLBACK":
		next, k := word(text, skipSpace(text, j))
		if next == "TRANSACTION" {
			next, _ = word(text, skipSpace(text, k))
		}
		return next != "TO"
	}
	return false
}

// IsWriteStatement reports whether text is an INSERT, UPDATE, DELETE or
// REPLACE, possibly behind a WITH clause.
func IsWriteStatement(text string) bool {
	switch kw, _ := firstKeyword(text); kw {
	case "INSERT", "UPDATE", "DELETE", "REPLACE":
		return true
	case "WITH":
		return hasWriteWord(text)
	}
	return false
}

// hasWriteWord reports whether text holds INSERT, UPDATE, DELETE or REPLACE
// as a whole word outside string literals, quoted identifiers and comments,
// so a column named updated_at or a string 'delete me' does not count.
func hasWriteWord(text string) bool {
	for i := skipSpace(text, 0); i < len(text); i = skipSpace(text, i) {
		switch c := text[i]; {
		case c == '"' || c == '\'' || c == '`' || c == '[':
			end := c
			if c == '[' {
				end = ']'
			}
			// A doubled quote inside a quoted run reads as two runs back to back.
			j := strings.IndexByte(text[i+1:], end)
			if j < 0 {
				return false
			}
			i += j + 2
		case isWordByte(c):
			j := i
			for j < len(text) && isWordByte(text[j]) {
				j++
			}
			switch asciiUpper(text[i:j]) {
			case "INSERT", "UPDATE", "DELETE", "REPLACE":
				return true
			}
			i = j
		default:
			i++
		}
	}
	return false
}

// isWordByte reports whether c can be part of a bare SQLite identifier or
// keyword. Bytes of multi-byte characters count, so "éupdate" is one word.
func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$' || c >= 0x80
}

// errNotExplainable explains an ErrReadOnly from CheckExplain.
var errNotExplainable = errors.New("only SELECT, WITH, VALUES, INSERT, UPDATE, DELETE, REPLACE and read-only PRAGMA statements can be explained")

// CheckExplain is Explain's allow-list: whatever the read-only path accepts,
// plus INSERT, UPDATE, DELETE and REPLACE, which EXPLAIN QUERY PLAN only
// compiles. Everything else is refused, since SQLite applies some
// statements' effects (PRAGMA assignments among them) at compile time.
func CheckExplain(name, text string) error {
	if CheckReadOnly(name, text) == nil {
		return nil
	}
	switch kw, _ := firstKeyword(text); kw {
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "WITH":
		return nil
	}
	return driver.Wrap(driver.ErrReadOnly, name, "explain", errNotExplainable)
}
