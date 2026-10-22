package sqlcomplete

import (
	"strings"

	"github.com/xavidop/tobira/driverutil/sqlscan"
)

type tokKind int

const (
	tkWord    tokKind = iota // bare identifier or keyword
	tkQuoted                 // "quoted", `quoted` or [quoted] identifier
	tkString                 // '...', E'...', $tag$...$tag$, "..." (mysql)
	tkNumber                 // 12, 1.5, 0xFF
	tkPunct                  // any other single byte
	tkComment                // -- line, # line (mysql) or /* block */
)

// token is one lexical unit of the query text. start and end are byte offsets
// into the text; end is exclusive. open marks a string, quoted identifier or
// block comment missing its terminator, and every line comment (which runs to
// the end of the line, so the cursor at its end is still inside it).
type token struct {
	kind       tokKind
	start, end int
	text       string // word: as written; quoted: unquoted and unescaped; punct: the byte
	open       bool
}

// scanDialect returns the driverutil/sqlscan rules for d's lexical extras.
func (d Dialect) scanDialect() sqlscan.Dialect {
	if d.Lexical != nil {
		return *d.Lexical
	}
	s := sqlscan.Generic()
	s.Backticks, s.Brackets = d.Backticks, d.Brackets
	if d.PGSyntax {
		s.NestedComments, s.EStrings, s.DollarQuotes, s.DollarTagHighBit = true, true, true, true
	}
	if d.MySQLSyntax {
		s.DashCommentSpace, s.HashComments, s.DoubleQuoteStrings, s.BackslashEscapes = true, true, true, true
	}
	if d.CHSyntax {
		s.HashComments, s.NestedComments, s.BackslashEscapes, s.IdentBackslashEscapes, s.DollarQuotes = true, true, true, true, true
	}
	return s
}

// tokenize splits text into tokens with driverutil/sqlscan, the scanner the
// drivers' read-only guards also use. It never fails: unterminated
// constructs run to the end of the text and are flagged open.
func tokenize(text string, d Dialect) []token {
	var toks []token
	sd := d.scanDialect()
	scan := func(text string, fn func(sqlscan.Token) bool) { sqlscan.Scan(sd, text, fn) }
	if d.Oracle {
		scan, sd = sqlscan.ScanOracle, sqlscan.Dialect{}
	}
	scan(text, func(t sqlscan.Token) bool {
		raw := text[t.Start:t.End]
		tok := token{start: t.Start, end: t.End}
		switch t.Kind {
		case sqlscan.Space:
			return true
		case sqlscan.Comment, sqlscan.ExecComment:
			tok.kind, tok.open = tkComment, raw[0] != '/' // a line comment
		case sqlscan.String:
			tok.kind = tkString
		case sqlscan.QuotedIdent:
			tok.kind, tok.text = tkQuoted, d.unquote(raw[1:len(raw)-1], raw[0])
		case sqlscan.Number:
			tok.kind = tkNumber
		case sqlscan.Word, sqlscan.Semicolon, sqlscan.Punct:
			tok.kind, tok.text = tkWord, raw
			if t.Kind != sqlscan.Word {
				tok.kind = tkPunct
			}
		case sqlscan.Unterminated:
			tok.open = true
			switch q := raw[0]; {
			case q == '/':
				tok.kind = tkComment
			case q == '`', q == '[', q == '"' && !sd.DoubleQuoteStrings:
				tok.kind, tok.text = tkQuoted, d.unquote(raw[1:], q)
			default: // '…', "…" (mysql), E'…', $tag$…
				tok.kind = tkString
			}
		}
		toks = append(toks, tok)
		return true
	})
	return toks
}

// unquote undoes the doubled quotes in the body of an identifier quoted with
// open, and under CHSyntax its backslash escapes; [ … ] has no escapes.
func (d Dialect) unquote(body string, open byte) string {
	if open == '[' {
		return body
	}
	if d.CHSyntax && strings.IndexByte(body, '\\') >= 0 {
		var b strings.Builder
		for i := 0; i < len(body); i++ {
			if body[i] == '\\' && i+1 < len(body) {
				i++
			} else if body[i] == open && i+1 < len(body) && body[i+1] == open {
				i++
			}
			b.WriteByte(body[i])
		}
		return b.String()
	}
	q := string(open)
	return strings.ReplaceAll(body, q+q, q)
}
