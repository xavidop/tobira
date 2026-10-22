package oracle

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// The read-only path's first layer is a lexical allow-list, checked before
// any text reaches the server. It is needed because Oracle commits
// implicitly before DDL: a CREATE, DROP or GRANT sent inside the SET
// TRANSACTION READ ONLY transaction (the second layer) ends that transaction
// and runs. PL/SQL can commit too, and a WITH FUNCTION clause can declare an
// autonomous transaction inside a SELECT. The check fails closed: text it
// cannot lex, or a statement it does not recognise, is refused.

// token is one significant lexical unit (spaces and comments are dropped).
type token struct {
	kind sqlscan.Kind
	text string // upper-cased word, upper-cased name of a quoted identifier, raw text otherwise
	pos  int
}

// lexResult is the token stream of a text, plus why the text cannot be
// judged safely ("" when it can).
type lexResult struct {
	toks []token
	bad  string
	// nonASCII is the first token outside strings, quoted identifiers and
	// comments that holds a non-ASCII character, "" when there is none.
	// The read-only paths refuse it: how the server treats U+00A0 and the
	// other Unicode spaces and punctuation there was not verified.
	nonASCII string
}

// numberForm is the shape of an Oracle numeric literal: digits, an optional
// fraction, an optional exponent and an optional BINARY_FLOAT/DOUBLE suffix.
// The scanner lets a number run on through word bytes, so 1delete is one
// token; the server reads it as 1d followed by the word elete. Anything not
// of this shape is refused.
var numberForm = regexp.MustCompile(`^[0-9]+(\.[0-9]*)?([eE][+-]?[0-9]+)?[fFdD]?$`)

// lex scans text with sqlscan.ScanOracle and reports what makes it unsafe to
// judge: a NUL byte, text that is not valid UTF-8, a q-quote with a
// non-ASCII delimiter (scanned as unterminated), an unterminated string, identifier or comment, a line
// comment holding a carriage return (whether the server ends the comment
// there was not verified), or a malformed number.
func lex(text string) lexResult {
	var r lexResult
	if strings.IndexByte(text, 0) >= 0 {
		r.bad = "the text holds a NUL byte"
		return r
	}
	if !utf8.ValidString(text) { // the server could split invalid bytes differently
		r.bad = "the text is not valid UTF-8"
		return r
	}
	sqlscan.ScanOracle(text, func(t sqlscan.Token) bool {
		s := text[t.Start:t.End]
		if r.nonASCII == "" && hasNonASCII(s) {
			switch t.Kind {
			case sqlscan.String, sqlscan.QuotedIdent, sqlscan.Comment:
			default:
				r.nonASCII = s
			}
		}
		switch t.Kind {
		case sqlscan.Space:
		case sqlscan.Comment:
			if strings.HasPrefix(s, "--") && strings.IndexByte(s, '\r') >= 0 {
				r.bad = "a line comment holds a carriage return"
				return false
			}
		case sqlscan.Unterminated:
			r.bad = "unterminated string, identifier or comment"
			return false
		case sqlscan.Word:
			r.toks = append(r.toks, token{kind: t.Kind, text: strings.ToUpper(s), pos: t.Start})
		case sqlscan.QuotedIdent:
			r.toks = append(r.toks, token{kind: t.Kind, text: strings.ToUpper(s[1 : len(s)-1]), pos: t.Start})
		case sqlscan.Number:
			if !numberForm.MatchString(s) {
				r.bad = "malformed number " + quoteShort(s)
				return false
			}
			r.toks = append(r.toks, token{kind: t.Kind, text: s, pos: t.Start})
		default:
			r.toks = append(r.toks, token{kind: t.Kind, text: s, pos: t.Start})
		}
		return true
	})
	return r
}

func hasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// nonASCIIReason is the refusal for a non-ASCII character outside quotes.
func nonASCIIReason(tok string) string {
	return "a non-ASCII character outside strings, quoted identifiers and comments (" + quoteShort(tok) +
		") is not allowed: quote non-ASCII names and use ASCII spaces"
}

func quoteShort(s string) string {
	if len(s) > 32 {
		s = s[:32] + "…"
	}
	return fmt.Sprintf("%q", s)
}

// readDenied are the words refused anywhere (outside quotes) on a read-only
// path: statements that write, end or start a transaction, run PL/SQL or
// change the session, and the clauses that write or lock from inside a
// query (INTO, FOR UPDATE, NEXTVAL, WITH FUNCTION).
var readDenied = wordSet(`INSERT UPDATE DELETE MERGE UPSERT INTO CREATE ALTER DROP TRUNCATE RENAME GRANT REVOKE
COMMENT AUDIT NOAUDIT ANALYZE ASSOCIATE DISASSOCIATE PURGE FLASHBACK COMMIT ROLLBACK SAVEPOINT LOCK CALL EXEC
EXECUTE BEGIN DECLARE FUNCTION PROCEDURE PRAGMA EXPLAIN NEXTVAL ADMINISTER SHUTDOWN STARTUP`)

// explainAllowed are the readDenied words a planned (never run) DML
// statement needs.
var explainAllowed = wordSet(`INSERT UPDATE DELETE MERGE INTO NEXTVAL`)

// sideEffectNames are object names refused on a read-only path whether
// quoted or not: the PL/SQL packages that reach outside the transaction
// (DBMS_PIPE, DBMS_LOCK, DBMS_SCHEDULER…), the network and file packages
// (UTL_HTTP, UTL_FILE, UTL_INADDR…) and the URI types that fetch URLs.
var sideEffectPrefixes = []string{"DBMS_", "UTL_", "OWA_", "APEX_"}

var sideEffectNames = wordSet(`HTP HTF HTTPURITYPE DBURITYPE XDBURITYPE URIFACTORY NEXTVAL`)

func wordSet(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

// sideEffectName reports whether name is refused as a function or package
// name on a read-only path.
func sideEffectName(name string) bool {
	if sideEffectNames[name] {
		return true
	}
	for _, p := range sideEffectPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// statementShape checks what every guarded statement must be: one statement,
// optionally ended by a single ';'. It returns the tokens without that ';'.
func statementShape(lr lexResult) ([]token, string) {
	if lr.bad != "" {
		return nil, lr.bad
	}
	toks := lr.toks
	if len(toks) > 0 && toks[len(toks)-1].kind == sqlscan.Semicolon {
		toks = toks[:len(toks)-1]
	}
	if len(toks) == 0 {
		return nil, "empty statement"
	}
	for _, t := range toks {
		if t.kind == sqlscan.Semicolon {
			return nil, "only one statement is allowed"
		}
	}
	return toks, ""
}

// guardTokens checks tokens against a denied-word set, the side-effect
// names and database links.
func guardTokens(toks []token, denied map[string]bool, allowNextval bool) string {
	refused := func(name string) bool { return sideEffectName(name) && (!allowNextval || name != "NEXTVAL") }
	for _, t := range toks {
		switch t.kind {
		case sqlscan.Word:
			if denied[t.text] {
				return t.text + " is not allowed"
			}
			if refused(t.text) {
				return t.text + " is not allowed: it can act outside the transaction"
			}
		case sqlscan.QuotedIdent:
			if refused(t.text) {
				return `"` + t.text + `" is not allowed: it can act outside the transaction`
			}
		case sqlscan.Punct:
			if t.text == "@" {
				return "a database link (@) is not allowed"
			}
		}
	}
	return ""
}

// startsQuery reports whether toks open a query: SELECT, WITH or '('.
func startsQuery(toks []token) bool {
	if len(toks) == 0 {
		return false
	}
	t := toks[0]
	return t.kind == sqlscan.Word && (t.text == "SELECT" || t.text == "WITH") || t.kind == sqlscan.Punct && t.text == "("
}

// readOnlyReason is why text is not a single read-only query, or "".
func readOnlyReason(text string) string {
	lr := lex(text)
	if lr.bad != "" {
		return lr.bad
	}
	if lr.nonASCII != "" {
		return nonASCIIReason(lr.nonASCII)
	}
	if why := guardTokens(lr.toks, readDenied, false); why != "" {
		return why
	}
	toks, bad := statementShape(lr)
	if bad != "" {
		return bad
	}
	if !startsQuery(toks) {
		return "only a SELECT or WITH query is allowed"
	}
	return ""
}

// checkReadOnly is the read-only allow-list: exactly one SELECT, WITH or
// parenthesised query, optionally ended by ';', with only ASCII outside
// strings, quoted identifiers and comments, none of readDenied outside quotes, no side-effect package or URI type (quoted or not), and
// no database link. Anything else is ErrReadOnly.
func checkReadOnly(text string) error {
	if why := readOnlyReason(text); why != "" {
		return driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("refused on a read-only path: "+why))
	}
	return nil
}

// isQueryText reports whether text opens with SELECT, WITH or '(' and so
// returns rows; other statements run through Exec.
func isQueryText(text string) bool {
	return startsQuery(lex(text).toks)
}

// explainTarget checks the statement Explain plans: one SELECT, WITH, '(',
// INSERT, UPDATE, DELETE or MERGE statement with the read-only path's
// refusals except the DML words themselves. EXPLAIN PLAN never runs it.
func explainTarget(text string) error {
	lr := lex(text)
	if lr.bad == "" && lr.nonASCII != "" {
		return errors.New(nonASCIIReason(lr.nonASCII))
	}
	toks, bad := statementShape(lr)
	if bad != "" {
		return errors.New(bad)
	}
	first := toks[0]
	dml := first.kind == sqlscan.Word && (first.text == "INSERT" || first.text == "UPDATE" || first.text == "DELETE" || first.text == "MERGE")
	if !startsQuery(toks) && !dml {
		return errors.New("only SELECT, WITH, INSERT, UPDATE, DELETE and MERGE can be explained")
	}
	denied := map[string]bool{}
	for w := range readDenied {
		if !explainAllowed[w] {
			denied[w] = true
		}
	}
	if why := guardTokens(toks, denied, true); why != "" {
		return errors.New(why)
	}
	return nil
}

// txDenied are the words refused anywhere in a writable transaction's
// statement: PL/SQL, which can COMMIT, and the procedure calls.
var txDenied = wordSet(`BEGIN DECLARE FUNCTION PROCEDURE PRAGMA CALL EXEC EXECUTE COMMIT`)

// checkTxStatement checks a statement run inside a writable transaction: it
// must not end the transaction. Oracle commits implicitly before DDL, and
// COMMIT, ROLLBACK, SET TRANSACTION, PL/SQL and procedure calls end it or
// may. So one statement opening with SELECT, WITH, '(', INSERT, UPDATE,
// DELETE, MERGE, LOCK, SAVEPOINT or ROLLBACK TO is accepted, without
// PL/SQL anywhere. The driver also checks after each statement that the
// transaction is still the same one.
func checkTxStatement(text string) error {
	refuse := func(why string) error {
		return driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("refused in a transaction: "+why+"; use Commit or Rollback, or run it outside the transaction"))
	}
	toks, bad := statementShape(lex(text))
	if bad != "" {
		return refuse(bad)
	}
	first := toks[0]
	ok := startsQuery(toks)
	if first.kind == sqlscan.Word {
		switch first.text {
		case "INSERT", "UPDATE", "DELETE", "MERGE", "LOCK", "SAVEPOINT":
			ok = true
		case "ROLLBACK":
			ok = len(toks) > 1 && toks[1].kind == sqlscan.Word && toks[1].text == "TO"
		}
	}
	if !ok {
		return refuse(first.text + " would end the transaction")
	}
	for _, t := range toks {
		if t.kind == sqlscan.Word && txDenied[t.text] {
			return refuse(t.text + " is not allowed")
		}
	}
	return nil
}

// checkRawFilter checks Filter.Raw, the native WHERE body of a Browse: it
// must lex cleanly, keep its parentheses balanced and hold no ';', so it
// cannot close the generated WHERE clause or add a statement. On a read-only
// call the generated SELECT also goes through checkReadOnly.
func checkRawFilter(raw string) error {
	bad := func(reason string) error {
		return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid raw filter: "+reason))
	}
	lr := lex(raw)
	if lr.bad != "" {
		return bad(lr.bad)
	}
	depth := 0
	for _, t := range lr.toks {
		switch {
		case t.kind == sqlscan.Semicolon:
			return bad("';' is not allowed")
		case t.kind != sqlscan.Punct:
		case t.text == "(":
			depth++
		case t.text == ")":
			if depth == 0 {
				return bad("unbalanced parentheses")
			}
			depth--
		}
	}
	if depth != 0 {
		return bad("unbalanced parentheses")
	}
	return nil
}

// checkSchemaName refuses a namespace that cannot be a quoted Oracle
// identifier: empty, holding '"' or NUL, or not valid UTF-8. The namespace is
// put into ALTER SESSION SET CURRENT_SCHEMA = "<ns>", which runs outside the
// read-only allow-list, so a '"' must never reach it.
func checkSchemaName(op, ns string) error {
	switch {
	case ns == "":
		return driver.Wrap(driver.ErrSyntax, Name, op, errors.New("invalid namespace: empty"))
	case strings.ContainsAny(ns, "\"\x00"):
		return driver.Wrap(driver.ErrSyntax, Name, op, errors.New(`invalid namespace: an Oracle name cannot hold '"' or NUL`))
	case !utf8.ValidString(ns):
		return driver.Wrap(driver.ErrSyntax, Name, op, errors.New("invalid namespace: not valid UTF-8"))
	}
	return nil
}
