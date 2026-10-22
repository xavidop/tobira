package tools

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/xavidop/tobira/driver"
)

// The rendering of statement errors when rows may not be sent for the call.
// It fails closed: a driver error is reduced to its kind, a fixed text for
// that kind and its native code. Only pure syntax errors keep some text,
// the part before the first quote-like character, because they cite the
// model's own query and help it fix the statement. Error messages are an
// exfiltration channel otherwise: postgres "operator does not exist: <v>"
// echoes a value without any quote.

// codeRe matches the error codes left intact by maskDigits: "SQLSTATE 42601",
// "(SQLSTATE 42601)" and mysql's "Error 1064 (42000)".
var codeRe = regexp.MustCompile(`\(?SQLSTATE [0-9A-Z]{5}\)?|Error [0-9]{3,5} \([0-9A-Z]{5}\)`)

// digitsRe matches a run of four or more digits.
var digitsRe = regexp.MustCompile(`[0-9]{4,}`)

// sqlStateRe and errorNumberRe are the native code forms appended to a
// reduced error; any other Code is left out.
var (
	sqlStateRe    = regexp.MustCompile(`^[0-9A-Z]{5}$`)
	errorNumberRe = regexp.MustCompile(`^[0-9]{1,6}$`)
)

// extraQuotes are quote characters added to unicode.Quotation_Mark in case
// the table lacks any of them.
var extraQuotes = []rune{'«', '»', '‹', '›', '「', '」', '『', '』'}

// kindTexts are the fixed texts a reduced error carries per kind.
var kindTexts = map[error]string{
	driver.ErrNotFound:    "object not found; use describe/list tools to check names",
	driver.ErrSyntax:      "syntax error",
	driver.ErrPermission:  "permission denied",
	driver.ErrReadOnly:    "refused: only read-only statements may run here",
	driver.ErrUnsupported: "not supported by this database",
	driver.ErrConnection:  "connection failed",
	driver.ErrConflict:    "conflict; retry",
	driver.ErrLimit:       "over a size or cost limit; narrow the query (a LIMIT, fewer columns)",
}

func isQuote(r rune) bool {
	return r == '\'' || r == '"' || r == '`' || unicode.Is(unicode.Quotation_Mark, r) || slices.Contains(extraQuotes, r)
}

// scrubError cuts s at its first quote-like character (ASCII and Unicode
// quotation marks, guillemets, CJK brackets, or the start of a $tag$
// quote): everything from there to the end of the message, not just of the
// line, becomes "…", since a quoted value can hold a newline. Nothing from
// the dropped part is kept, codes included. In the kept part, runs of four
// or more digits become "#", except inside error codes.
func scrubError(s string) string {
	cut := -1
	for i, r := range s {
		if isQuote(r) {
			cut = i
			break
		}
		if r == '$' {
			if _, ok := dollarTag(s[i:]); ok {
				cut = i
				break
			}
		}
	}
	if cut < 0 {
		return maskDigits(s)
	}
	return maskDigits(s[:cut]) + "…"
}

// maskDigits replaces runs of 4+ digits with "#", leaving error codes alone.
func maskDigits(s string) string {
	var sb strings.Builder
	last := 0
	for _, m := range codeRe.FindAllStringIndex(s, -1) {
		sb.WriteString(digitsRe.ReplaceAllString(s[last:m[0]], "#"))
		sb.WriteString(s[m[0]:m[1]])
		last = m[1]
	}
	sb.WriteString(digitsRe.ReplaceAllString(s[last:], "#"))
	return sb.String()
}

// dollarTag returns the PostgreSQL dollar-quote opener at the start of s:
// "$$" or "$tag$", where tag starts with a letter or underscore. "$1" is a
// parameter, not a quote.
func dollarTag(s string) (string, bool) {
	for j := 1; j < len(s); j++ {
		c := s[j]
		switch {
		case c == '$':
			return s[:j+1], true
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80:
		case c >= '0' && c <= '9' && j > 1:
		default:
			return "", false
		}
	}
	return "", false
}

// scrubDriverError renders a statement error for a call whose rows may not
// be sent: "<kind>: <fixed text> (SQLSTATE xxxxx)" or "(error NNNN)", with
// the code read from driver.Error.Code only (never parsed from the
// message). A pure syntax error (isSyntaxOnly) keeps instead the redacted
// message before its first quote. Errors that are not *driver.Error become
// "error: query failed".
func scrubDriverError(err error, redact func(string) string) string {
	var de *driver.Error
	if !errors.As(err, &de) {
		return "error: query failed"
	}
	kind := "error"
	if de.Kind != nil {
		kind = de.Kind.Error()
	}
	code := ""
	switch {
	case (de.Driver == "mysql" || de.Driver == "sqlite") && errorNumberRe.MatchString(de.Code):
		code = " (error " + de.Code + ")"
	case de.Driver != "mysql" && de.Driver != "sqlite" && sqlStateRe.MatchString(de.Code):
		code = " (SQLSTATE " + de.Code + ")"
	case de.Driver != "mysql" && de.Driver != "sqlite" && errorNumberRe.MatchString(de.Code):
		code = " (error " + de.Code + ")"
	}
	if msg, ok := syntaxText(de); ok {
		return kind + ": " + scrubError(redact(msg)) + code
	}
	text, ok := kindTexts[driver.KindOf(de)]
	if !ok {
		text = "query failed"
	}
	return kind + ": " + text + code
}

// syntaxText returns the message of a pure syntax error, the only errors
// that keep text: pg 42601, mysql 1064 and 1149, and sqlite messages that
// start with `near "`, "incomplete input" or "syntax error" (after the
// "SQL logic error: " prefix of the sqlite library).
func syntaxText(de *driver.Error) (string, bool) {
	if de.Err == nil {
		return "", false
	}
	msg := de.Err.Error()
	switch de.Driver {
	case "mysql":
		return msg, de.Code == "1064" || de.Code == "1149"
	case "sqlite":
		m := strings.TrimPrefix(msg, "SQL logic error: ")
		ok := strings.HasPrefix(m, `near "`) || strings.HasPrefix(m, "incomplete input") || strings.HasPrefix(m, "syntax error")
		return m, ok
	}
	return msg, de.Code == "42601"
}
