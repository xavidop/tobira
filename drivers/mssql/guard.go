package mssql

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// The read-only path's first layer is a lexical allow-list, checked before
// any text reaches the server. SQL Server has no read-only transaction, and
// a batch may hold several statements with no ';' between them
// ("SELECT 1 DELETE FROM t" is two statements), so the check cannot rely on
// statement separators: it refuses every word that starts or belongs to a
// statement other than a query, wherever it appears outside a string,
// comment or quoted identifier. The check fails closed: text it cannot lex,
// a number it cannot read the way the server does, or a statement it does
// not recognise is refused. The second layer (rotx.go) runs the statement
// inside a transaction that is always rolled back.

type tokKind uint8

const (
	tokWord   tokKind = iota // bare word, upper-cased
	tokIdent                 // [quoted] or "quoted" identifier
	tokString                // '…' literal (N'…' is the word N, then this)
	tokNumber                // a numeric literal of a shape the server reads as one number
	tokPunct                 // one byte of punctuation, ';' included
)

type token struct {
	kind tokKind
	text string // the upper-cased word, the unquoted name of an identifier, or the punctuation byte; empty for strings and numbers
	pos  int    // byte offset of the token in the text
}

// lexResult is the token stream of a text, or why it cannot be judged.
type lexResult struct {
	toks []token
	bad  string // why the text could not be lexed; empty when it could
}

// numberShape is what SQL Server reads as one numeric literal: a binary
// literal 0x…, or digits with an optional fraction and exponent. Anything
// else in a Number token (1INTO, 0xINSERT, 1eDELETE) is a number followed
// by a word to the server, so the text is refused rather than misread. An
// exponent without digits ("1e" of "1e+5") is fine: the sign and digits
// that follow are punctuation and a number, never a keyword.
var numberShape = regexp.MustCompile(`^(?:0[xX][0-9a-fA-F]*|[0-9]+\.?[0-9]*(?:[eE][0-9]*)?)$`)

// lex splits text into tokens with driverutil/sqlscan's T-SQL dialect.
// SQL Server ends a "--" comment at a carriage return as well as at a line
// feed, so a line comment holding a lone carriage return makes the text
// unjudgeable; so do a NUL byte, an unterminated string, identifier or
// comment, and a number of an unknown shape.
func lex(text string) lexResult {
	var r lexResult
	if strings.IndexByte(text, 0) >= 0 {
		r.bad = "the text contains a NUL byte"
		return r
	}
	sqlscan.Scan(sqlscan.TSQL(), text, func(t sqlscan.Token) bool {
		raw := text[t.Start:t.End]
		switch t.Kind {
		case sqlscan.Space:
		case sqlscan.Comment, sqlscan.ExecComment:
			if raw[0] == '-' && loneCR(raw) {
				r.bad = "a carriage return inside a -- comment (SQL Server ends the comment there)"
				return false
			}
		case sqlscan.Unterminated:
			switch raw[0] {
			case '/':
				r.bad = "unterminated comment"
			case '[', '"':
				r.bad = "unterminated quoted identifier"
			default:
				r.bad = "unterminated quoted text"
			}
			return false
		case sqlscan.String:
			r.toks = append(r.toks, token{kind: tokString, pos: t.Start})
		case sqlscan.QuotedIdent:
			r.toks = append(r.toks, token{kind: tokIdent, text: identName(raw), pos: t.Start})
		case sqlscan.Number:
			if !numberShape.MatchString(raw) {
				r.bad = fmt.Sprintf("the number %q is not a literal the guard can read", raw)
				return false
			}
			r.toks = append(r.toks, token{kind: tokNumber, pos: t.Start})
		case sqlscan.Word:
			r.toks = append(r.toks, token{kind: tokWord, text: strings.ToUpper(raw), pos: t.Start})
		default: // Semicolon, Punct
			r.toks = append(r.toks, token{kind: tokPunct, text: raw, pos: t.Start})
		}
		return true
	})
	return r
}

// loneCR reports whether a line comment holds a carriage return that is not
// the first half of "\r\n" at its end.
func loneCR(comment string) bool {
	i := strings.IndexByte(comment, '\r')
	return i >= 0 && i != len(comment)-1
}

// identName returns the name inside a [quoted] or "quoted" identifier, with
// doubled closers undone.
func identName(quoted string) string {
	body := quoted[1 : len(quoted)-1]
	if quoted[0] == '[' {
		return strings.ReplaceAll(body, "]]", "]")
	}
	return strings.ReplaceAll(body, `""`, `"`)
}

func readOnlyError(reason error) error {
	return driver.Wrap(driver.ErrReadOnly, Name, "query", reason)
}

// errNotRead is the reason given for a statement outside the allow-list.
var errNotRead = errors.New("only SELECT and WITH … SELECT queries and sp_help procedures are allowed on a read-only path")

// readDenied are the words refused anywhere in a read-only statement. Each
// one starts a statement other than a query (a second statement can follow
// a query without a ';'), writes, runs text the check cannot see, or
// reaches outside the server. Most are reserved words, which a column name
// can only use when quoted ([delete]); the few that are not (GET, MOVE,
// SEND, RECEIVE, ENABLE, DISABLE, LINENO, CONVERSATION, COPY, RENAME) are
// refused too and must be quoted to be read. The lock hints TABLOCKX,
// XLOCK, UPDLOCK, HOLDLOCK, PAGLOCK, SERIALIZABLE and REPEATABLEREAD are
// refused because they would hold
// exclusive, update or range locks for as long as the result stays open,
// blocking writers.
var readDenied = setOf(`INSERT UPDATE DELETE MERGE TRUNCATE INTO OUTPUT
EXEC EXECUTE CREATE ALTER DROP GRANT REVOKE DENY ADD RENAME COPY
BEGIN COMMIT ROLLBACK SAVE DECLARE SET USE GO
PRINT RAISERROR THROW WAITFOR IF WHILE GOTO RETURN BREAK CONTINUE
BULK BACKUP RESTORE DBCC KILL SHUTDOWN CHECKPOINT RECONFIGURE
OPEN CLOSE DEALLOCATE READTEXT WRITETEXT UPDATETEXT
ENABLE DISABLE MOVE GET SEND RECEIVE CONVERSATION REVERT SETUSER LINENO
OPENROWSET OPENQUERY OPENDATASOURCE DUMP LOAD
TABLOCKX XLOCK UPDLOCK HOLDLOCK PAGLOCK SERIALIZABLE REPEATABLEREAD`)

// explainAllowed are the read-denied words an explained statement may hold:
// SHOWPLAN plans data changes without running them.
var explainAllowed = setOf(`INSERT UPDATE DELETE MERGE INTO OUTPUT`)

func setOf(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

// helpProcs are the catalog procedures a read-only path may run, called as
// "[EXEC[UTE]] [sys.]name [args]". They only read the catalog.
var helpProcs = setOf(`SP_HELP SP_HELPTEXT SP_HELPINDEX SP_HELPCONSTRAINT SP_HELPDB SP_HELPTRIGGER
SP_HELPSTATS SP_HELPFILE SP_HELPFILEGROUP SP_HELPROTECT`)

// checkReadOnly is the read-only path's allow-list. text is accepted when it
// holds exactly one statement (a trailing ';' is fine) and that statement is
// a query (SELECT, WITH … SELECT, a parenthesised query) or a call of a
// helpProcs procedure with literal arguments. A query is refused when it
// holds a word of readDenied, an extended procedure name (xp_…), NEXT VALUE
// FOR (it advances a sequence, which a rollback does not undo), or a second
// top-level SELECT that is not joined by UNION, EXCEPT or INTERSECT. Text
// that cannot be lexed is refused too (see lex). Every refusal is
// ErrReadOnly.
func checkReadOnly(text string) error {
	lr := lex(text)
	if lr.bad != "" {
		return readOnlyError(fmt.Errorf("%w: cannot check the statement: %s", errNotRead, lr.bad))
	}
	toks, err := oneStatement(lr.toks)
	if err != nil {
		return readOnlyError(err)
	}
	if len(toks) == 0 {
		return readOnlyError(errNotRead)
	}
	if isHelpCall(toks) {
		return nil
	}
	if err := checkQuery(toks, readDenied); err != nil {
		return readOnlyError(err)
	}
	return nil
}

var errOneStatement = errors.New("only one statement per query is allowed on a read-only path")

// oneStatement returns the tokens of the only statement in toks; empty
// statements (stray ';') do not count. Several statements are an error.
func oneStatement(toks []token) ([]token, error) {
	var stmt []token
	found := false
	start := 0
	for i := 0; i <= len(toks); i++ {
		if i < len(toks) && !isPunct(toks, i, ";") {
			continue
		}
		if s := toks[start:i]; len(s) > 0 {
			if found {
				return nil, errOneStatement
			}
			stmt, found = s, true
		}
		start = i + 1
	}
	return stmt, nil
}

// checkQuery checks a query statement against denied: it must start with
// SELECT, WITH or '(', hold none of the denied words, no extended
// procedure, no NEXT VALUE FOR, and only one top-level query.
func checkQuery(toks []token, denied map[string]bool) error {
	if err := deniedWord(toks, denied, "on a read-only path"); err != nil {
		return err
	}
	if !isWord(toks, 0, "SELECT", "WITH") && !isPunct(toks, 0, "(") {
		return errNotRead
	}
	main := 0
	if isWord(toks, 0, "WITH") {
		if main = withMain(toks); main < 0 || !isWord(toks, main, "SELECT") && !isPunct(toks, main, "(") {
			return errors.New("WITH must be followed by CTEs and a SELECT on a read-only path")
		}
	}
	return singleQuery(toks, main)
}

// deniedWord returns the refusal for the first denied word in toks; where
// ends its message ("on a read-only path").
func deniedWord(toks []token, denied map[string]bool, where string) error {
	for i, t := range toks {
		if t.kind != tokWord {
			continue
		}
		switch {
		case denied[t.text]:
			return fmt.Errorf("%s is not allowed %s", t.text, where)
		case strings.HasPrefix(t.text, "XP_"):
			return fmt.Errorf("extended procedures (%s) are not allowed %s", t.text, where)
		case t.text == "NEXT" && isWord(toks, i+1, "VALUE") && isWord(toks, i+2, "FOR"):
			return fmt.Errorf("NEXT VALUE FOR advances a sequence and is not allowed %s", where)
		}
	}
	return nil
}

// singleQuery refuses a second query juxtaposed with the first one: every
// SELECT outside parentheses after the main query's start must follow
// UNION [ALL], EXCEPT or INTERSECT. Unbalanced parentheses are refused.
func singleQuery(toks []token, main int) error {
	depth := 0
	for i := main; i < len(toks); i++ {
		switch {
		case isPunct(toks, i, "("):
			depth++
		case isPunct(toks, i, ")"):
			if depth--; depth < 0 {
				return errors.New("unbalanced parentheses")
			}
		case depth == 0 && i > main && isWord(toks, i, "SELECT") && !isWord(toks, i-1, "UNION", "EXCEPT", "INTERSECT") &&
			(!isWord(toks, i-1, "ALL") || !isWord(toks, i-2, "UNION")):
			return errOneStatement
		}
	}
	if depth != 0 {
		return errors.New("unbalanced parentheses")
	}
	return nil
}

// withMain walks the CTE list of "WITH name [(cols)] AS (…), …" (or WITH
// XMLNAMESPACES (…)) and returns the index of the main statement after it,
// or -1 when the list is malformed.
func withMain(toks []token) int {
	i := 1
	for {
		switch {
		case isWord(toks, i, "XMLNAMESPACES") && isPunct(toks, i+1, "("):
			if i = skipGroup(toks, i+1); i < 0 {
				return -1
			}
		case isName(toks, i):
			i++
			if isPunct(toks, i, "(") {
				if i = skipGroup(toks, i); i < 0 {
					return -1
				}
			}
			if !isWord(toks, i, "AS") || !isPunct(toks, i+1, "(") {
				return -1
			}
			if i = skipGroup(toks, i+1); i < 0 {
				return -1
			}
		default:
			return -1
		}
		if !isPunct(toks, i, ",") {
			return i
		}
		i++
	}
}

// skipGroup returns the index after the ')' matching the '(' at toks[i], or
// -1 when it is unbalanced.
func skipGroup(toks []token, i int) int {
	depth := 0
	for ; i < len(toks); i++ {
		switch {
		case isPunct(toks, i, "("):
			depth++
		case isPunct(toks, i, ")"):
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// isHelpCall reports whether toks is "[EXEC[UTE]] [sys.]sp_help… [args]"
// with arguments that are literals, names or NULL, optionally named
// (@objname = N'dbo.t'), separated by commas. The procedure must be
// unqualified or in sys, so it is always the system procedure.
func isHelpCall(toks []token) bool {
	i := 0
	if isWord(toks, i, "EXEC", "EXECUTE") {
		i++
	}
	if isWord(toks, i, "SYS") && isPunct(toks, i+1, ".") {
		i += 2
	}
	if i >= len(toks) || toks[i].kind != tokWord || !helpProcs[toks[i].text] {
		return false
	}
	i++
	for first := true; i < len(toks); first = false {
		if !first {
			if !isPunct(toks, i, ",") {
				return false
			}
			i++
		}
		if isPunct(toks, i, "@") && isWord(toks, i+1) && isPunct(toks, i+2, "=") {
			i += 3
		}
		if i = helpArg(toks, i); i < 0 {
			return false
		}
	}
	return true
}

// helpArg returns the index after one procedure argument at toks[i]: a
// string (N'…' too), a number (optionally negative), a name or NULL; -1
// when there is none.
func helpArg(toks []token, i int) int {
	switch {
	case i >= len(toks):
		return -1
	case isWord(toks, i, "N") && i+1 < len(toks) && toks[i+1].kind == tokString:
		return i + 2
	case toks[i].kind == tokString, toks[i].kind == tokNumber, toks[i].kind == tokIdent:
		return i + 1
	case isPunct(toks, i, "-") && i+1 < len(toks) && toks[i+1].kind == tokNumber:
		return i + 2
	case toks[i].kind == tokWord && !readDenied[toks[i].text] && !strings.HasPrefix(toks[i].text, "XP_"):
		return i + 1
	}
	return -1
}

func isWord(toks []token, i int, words ...string) bool {
	if i < 0 || i >= len(toks) || toks[i].kind != tokWord {
		return false
	}
	if len(words) == 0 {
		return true
	}
	for _, w := range words {
		if toks[i].text == w {
			return true
		}
	}
	return false
}

func isPunct(toks []token, i int, p string) bool {
	return i >= 0 && i < len(toks) && toks[i].kind == tokPunct && toks[i].text == p
}

// isName reports whether toks[i] can name a table, column or CTE.
func isName(toks []token, i int) bool {
	return i < len(toks) && (toks[i].kind == tokWord || toks[i].kind == tokIdent)
}

// explainTarget checks a statement Explain is asked to plan, on read-only
// and writable connections alike. Explain runs it under SET SHOWPLAN_XML ON,
// which plans statements without running them, except SET statements, which
// still run (SET SHOWPLAN_XML OFF would end the protection). So the text
// must be exactly one statement starting with SELECT, WITH, '(', INSERT,
// UPDATE, DELETE or MERGE, holding no other word of readDenied (no SET, GO,
// EXEC, transaction control, DDL …), no extended procedure and no NEXT
// VALUE FOR. Text that cannot be lexed is refused. The error is a plain
// reason; callers give it a kind.
func explainTarget(text string) error {
	lr := lex(text)
	if lr.bad != "" {
		return fmt.Errorf("cannot check the statement: %s", lr.bad)
	}
	toks, err := oneStatement(lr.toks)
	if err != nil {
		return errors.New("explain takes exactly one statement")
	}
	if !isWord(toks, 0, "SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "MERGE") && !isPunct(toks, 0, "(") {
		return errors.New("only SELECT, WITH, INSERT, UPDATE, DELETE and MERGE statements can be explained")
	}
	for i := range toks {
		if isWord(toks, i, "SET") && updateSet(toks, i) {
			toks[i].text = "" // UPDATE's own SET clause, not a SET statement
		}
	}
	return deniedWord(toks, explainDenied, "in an explained statement")
}

// explainDenied is readDenied without explainAllowed.
var explainDenied = func() map[string]bool {
	m := map[string]bool{}
	for w := range readDenied {
		if !explainAllowed[w] {
			m[w] = true
		}
	}
	return m
}()

// updateSet reports whether the SET at toks[i] is the SET clause of an
// UPDATE: it follows UPDATE and the target's name (dotted, quoted), or
// THEN UPDATE in a MERGE, with at most a WITH (…) table hint between. A
// SET statement juxtaposed later in the text has other tokens before it.
func updateSet(toks []token, i int) bool {
	j := i - 1
	if isPunct(toks, j, ")") { // WITH (hints)
		depth := 0
		for ; j >= 0; j-- {
			if isPunct(toks, j, ")") {
				depth++
			} else if isPunct(toks, j, "(") {
				if depth--; depth == 0 {
					break
				}
			}
		}
		if !isWord(toks, j-1, "WITH") {
			return false
		}
		j -= 2
	}
	for j >= 0 && (isName(toks, j) && !isWord(toks, j, "UPDATE") || isPunct(toks, j, ".")) {
		j--
	}
	return isWord(toks, j, "UPDATE")
}

// txStatementStarts are the words a statement inside a writable
// transaction may start with. Any other first word makes SQL Server run
// the text as a procedure call (allowed without EXEC at the start of a
// batch), and a procedure may COMMIT.
var txStatementStarts = setOf(`SELECT WITH INSERT UPDATE DELETE MERGE CREATE ALTER DROP TRUNCATE
GRANT REVOKE DENY DECLARE SET IF WHILE BEGIN PRINT RAISERROR THROW WAITFOR SAVE USE
OPEN FETCH CLOSE DEALLOCATE ENABLE DISABLE`)

// errTxStatement is the reason checkTxStatement gives.
var errTxStatement = errors.New("this statement could end the transaction (COMMIT, ROLLBACK, BEGIN TRANSACTION, EXEC, a procedure call, GO or SET IMPLICIT_TRANSACTIONS) and is not allowed inside a transaction: end the transaction with Commit or Rollback")

// checkTxStatement is the check a writable transaction applies to user
// text, with ErrUnsupported. It refuses COMMIT, ROLLBACK, BEGIN
// TRAN[SACTION] or BEGIN DISTRIBUTED, EXEC[UTE], GO and SET
// IMPLICIT_TRANSACTIONS anywhere; a statement starting with a word outside
// txStatementStarts (a procedure call); several ';'-separated statements;
// and text that cannot be lexed. It is a deny-list: the transaction's
// post-check (still exactly one open transaction after every statement) is
// the fail-closed layer behind it.
func checkTxStatement(text string) error {
	refuse := func(err error) error { return driver.Wrap(driver.ErrUnsupported, Name, "query", err) }
	lr := lex(text)
	if lr.bad != "" {
		return refuse(fmt.Errorf("cannot check the statement: %s", lr.bad))
	}
	toks, err := oneStatement(lr.toks)
	if err != nil {
		return refuse(errors.New("only one statement per query is supported"))
	}
	if len(toks) == 0 {
		return refuse(errors.New("the statement is empty"))
	}
	if !isPunct(toks, 0, "(") && (toks[0].kind != tokWord || !txStatementStarts[toks[0].text]) {
		return refuse(errTxStatement)
	}
	for i, t := range toks {
		if t.kind != tokWord {
			continue
		}
		switch {
		case t.text == "COMMIT", t.text == "ROLLBACK", t.text == "EXEC", t.text == "EXECUTE", t.text == "GO",
			t.text == "BEGIN" && isWord(toks, i+1, "TRAN", "TRANSACTION", "DISTRIBUTED"),
			t.text == "SET" && isWord(toks, i+1, "IMPLICIT_TRANSACTIONS"):
			return refuse(errTxStatement)
		}
	}
	return nil
}

// execWords start the statements Query runs with Exec, which reports the
// rows affected: data changes and DDL, which return no rows unless they
// have an OUTPUT clause.
var execWords = setOf(`INSERT UPDATE DELETE MERGE TRUNCATE CREATE ALTER DROP GRANT REVOKE DENY`)

// useExec reports whether toks is one statement that returns no rows and
// whose rows-affected count Query should report: a data change or DDL
// (behind WITH too) without an OUTPUT clause.
func useExec(toks []token) bool {
	stmt, err := oneStatement(toks)
	if err != nil || len(stmt) == 0 {
		return false
	}
	main := 0
	if isWord(stmt, 0, "WITH") {
		if main = withMain(stmt); main < 0 {
			return false
		}
	}
	if !isWord(stmt, main) || !execWords[stmt[main].text] {
		return false
	}
	for i := range stmt {
		if isWord(stmt, i, "OUTPUT") {
			return false
		}
	}
	return true
}

// isDML reports whether a useExec statement is a data change (INSERT,
// UPDATE, DELETE or MERGE, behind WITH too), whose own row count
// SELECT @@ROWCOUNT can follow in the same batch. DDL cannot be followed:
// CREATE PROCEDURE and the like take the rest of the batch as their body.
func isDML(toks []token) bool {
	stmt, err := oneStatement(toks)
	if err != nil || len(stmt) == 0 {
		return false
	}
	main := 0
	if isWord(stmt, 0, "WITH") {
		if main = withMain(stmt); main < 0 {
			return false
		}
	}
	return isWord(stmt, main, "INSERT", "UPDATE", "DELETE", "MERGE")
}

// checkRawFilter checks a Browse raw filter before it is wrapped in
// parentheses: it must lex, keep its parentheses balanced without ever
// closing more than it opened, and hold no ';', so it cannot close the
// wrapping parenthesis and escape the other conditions (keyset, filters) or
// add a statement. Inside parentheses SQL Server only reads an expression.
// Anything else is ErrSyntax.
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
		case t.kind != tokPunct:
		case t.text == "(":
			depth++
		case t.text == ")":
			if depth == 0 {
				return bad("unbalanced parentheses")
			}
			depth--
		case t.text == ";":
			return bad("';' is not allowed")
		}
	}
	if depth != 0 {
		return bad("unbalanced parentheses")
	}
	return nil
}

// quoteIdent quotes a SQL Server identifier with brackets.
func quoteIdent(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }
