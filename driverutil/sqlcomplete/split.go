package sqlcomplete

import "strings"

// Span is one statement of a text: half-open byte offsets [Start, End).
type Span struct{ Start, End int }

// Split splits text into its statements at the semicolons outside strings,
// quoted identifiers, comments and (postgres) dollar quotes, as the dialect's
// tokenizer reads them. Each span is trimmed of surrounding whitespace and
// excludes its semicolon; a statement holding nothing but comments is
// dropped. Under Dialect.Oracle a PL/SQL unit keeps its semicolons and a
// line holding only "/" also ends a statement (see Dialect.Oracle). Split
// is a user-interface aid: drivers still enforce single statements and
// read-only on their own.
func Split(d Dialect, text string) []Span {
	out := []Span{}
	start, end := -1, -1 // first and past-the-last byte of the statement
	code := false        // the statement has a token other than a comment
	block := false       // the statement is a PL/SQL unit (Oracle)
	lead := true         // every code token so far is a word (Oracle)
	var words []string   // those words, upper case (Oracle)
	flush := func() {
		if code {
			out = append(out, Span{start, end})
		}
		start, end, code, block, lead, words = -1, -1, false, false, true, nil
	}
	for _, t := range tokenize(text, d) {
		if d.Oracle && t.kind == tkPunct && t.text == "/" && aloneOnLine(text, t.start, t.end) {
			flush()
			continue
		}
		if t.kind == tkPunct && t.text == ";" && !block {
			flush()
			continue
		}
		if start < 0 {
			start = t.start
		}
		end = t.end
		if t.kind == tkComment {
			continue
		}
		code = true
		if d.Oracle && lead && !block {
			if t.kind != tkWord {
				lead = false
				continue
			}
			words = append(words, strings.ToUpper(t.text))
			block = plsqlUnit(words)
		}
	}
	flush()
	return out
}

// plsqlUnit reports whether a statement whose leading words are words
// starts a PL/SQL unit: BEGIN, DECLARE, or CREATE [OR REPLACE]
// [EDITIONABLE|NONEDITIONABLE] FUNCTION, PROCEDURE, PACKAGE, TRIGGER or
// TYPE.
func plsqlUnit(words []string) bool {
	switch words[0] {
	case "BEGIN", "DECLARE":
		return true
	case "CREATE":
	default:
		return false
	}
	for _, w := range words[1:] {
		switch w {
		case "OR", "REPLACE", "EDITIONABLE", "NONEDITIONABLE":
			continue
		case "FUNCTION", "PROCEDURE", "PACKAGE", "TRIGGER", "TYPE":
			return true
		}
		return false
	}
	return false
}

// aloneOnLine reports whether text[start:end] is the only thing on its
// line but spaces, tabs and carriage returns.
func aloneOnLine(text string, start, end int) bool {
	for i := start - 1; i >= 0 && text[i] != '\n'; i-- {
		if text[i] != ' ' && text[i] != '\t' && text[i] != '\r' {
			return false
		}
	}
	for i := end; i < len(text) && text[i] != '\n'; i++ {
		if text[i] != ' ' && text[i] != '\t' && text[i] != '\r' {
			return false
		}
	}
	return true
}
