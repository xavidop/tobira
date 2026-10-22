package mysql

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// The read-only path's first layer is a lexical allow-list, checked before
// any text reaches the server. It exists because MySQL ends a transaction
// implicitly on DDL, LOCK TABLES, SET autocommit and similar statements, so
// the READ ONLY transaction (the second layer) could be left behind by the
// very statement it is meant to contain. The check fails closed: text it
// cannot lex, or a statement it does not recognise, is refused.

// lexMode is how the server reads quoted text, from its sql_mode.
type lexMode struct {
	noBackslash bool // NO_BACKSLASH_ESCAPES: a backslash is an ordinary character in strings
	ansiQuotes  bool // ANSI_QUOTES: "…" is an identifier, where a backslash never escapes
}

// modeFromSQLMode reads the lexMode out of an @@sql_mode value.
func modeFromSQLMode(sqlMode string) lexMode {
	var m lexMode
	for _, f := range strings.Split(sqlMode, ",") {
		switch strings.ToUpper(strings.TrimSpace(f)) {
		case "NO_BACKSLASH_ESCAPES":
			m.noBackslash = true
		case "ANSI_QUOTES", "ANSI":
			m.ansiQuotes = true
		}
	}
	return m
}

type tokKind uint8

const (
	tokWord   tokKind = iota // bare word, upper-cased
	tokIdent                 // `quoted` identifier (or "…" under ANSI_QUOTES)
	tokString                // '…' or "…" literal
	tokNumber                // a run starting with a digit
	tokPunct                 // one byte of punctuation
)

type token struct {
	kind tokKind
	text string // the upper-cased word, the upper-cased unquoted name of a quoted identifier, or the punctuation byte; empty for strings and numbers
	pos  int    // byte offset of the token in the text
}

// lexResult is the token stream of a text plus what makes it unsafe to judge.
type lexResult struct {
	toks        []token
	execComment bool   // a /*! … */ (or MariaDB /*M! … */) comment, possibly nested in another comment
	bad         string // why the text could not be lexed; empty when it could
}

// lex splits text into tokens with driverutil/sqlscan's MySQL dialect for
// mode, the scanner the other SQL drivers and completion share: comments
// ("# …", "-- …" where the dashes are followed by whitespace, a control
// character or the end, and "/* … */", which does not nest), quoted strings
// and identifiers, words, numbers and punctuation. The guards below decide
// on its tokens.
func lex(text string, mode lexMode) lexResult {
	var r lexResult
	if strings.IndexByte(text, 0) >= 0 {
		r.bad = "the text contains a NUL byte"
		return r
	}
	sqlscan.Scan(sqlscan.MySQL(mode.noBackslash, mode.ansiQuotes), text, func(t sqlscan.Token) bool {
		raw := text[t.Start:t.End]
		switch t.Kind {
		case sqlscan.Space, sqlscan.Comment:
		case sqlscan.ExecComment:
			r.execComment = true
		case sqlscan.Unterminated:
			switch raw[0] {
			case '/':
				r.bad = "unterminated comment"
			case '`':
				r.bad = "unterminated quoted identifier"
			default:
				r.bad = "unterminated quoted text"
			}
		case sqlscan.String:
			r.toks = append(r.toks, token{kind: tokString, pos: t.Start})
		case sqlscan.QuotedIdent:
			r.toks = append(r.toks, token{kind: tokIdent, text: identName(raw), pos: t.Start})
		case sqlscan.Number:
			r.toks = append(r.toks, token{kind: tokNumber, pos: t.Start})
		case sqlscan.Word:
			r.toks = append(r.toks, token{kind: tokWord, text: strings.ToUpper(raw), pos: t.Start})
		case sqlscan.Semicolon, sqlscan.Punct:
			r.toks = append(r.toks, token{kind: tokPunct, text: raw, pos: t.Start})
		}
		return true
	})
	return r
}

// identName returns the upper-cased name inside a quoted identifier
// (`…` or "…"), with doubled quotes undone.
func identName(quoted string) string {
	q := quoted[:1]
	return strings.ToUpper(strings.ReplaceAll(quoted[1:len(quoted)-1], q+q, q))
}

// errNotRead is the reason given for a statement outside the allow-list.
var errNotRead = errors.New("only SELECT, WITH, VALUES, TABLE, SHOW, DESCRIBE, EXPLAIN and USE statements are allowed on a read-only path")

func readOnlyError(reason error) error {
	return driver.Wrap(driver.ErrReadOnly, Name, "query", reason)
}

// checkReadOnly is the read-only path's allow-list. text is accepted when it
// holds exactly one statement (a trailing ';' is fine) and that statement is
// a query (SELECT, WITH … SELECT, VALUES, TABLE, a parenthesised query), SHOW,
// USE, DESCRIBE/DESC of a table, or EXPLAIN/DESCRIBE of an accepted query
// (EXPLAIN ANALYZE runs it). Anything else is ErrReadOnly, and so is:
//
//   - text that cannot be lexed (unterminated literal or comment, a NUL byte);
//   - any executable comment /*! … */, which the server runs as SQL;
//   - SELECT … INTO OUTFILE or DUMPFILE, which writes a file outside any
//     transaction;
//   - several statements when one of them would be refused.
//
// Several statements that would all be accepted are ErrUnsupported: the
// driver runs one statement per query. mode must match the session's
// sql_mode for texts that contain a backslash (see modeFromSQLMode).
func checkReadOnly(text string, mode lexMode) error {
	lr := lex(text, mode)
	if lr.bad != "" {
		return readOnlyError(fmt.Errorf("%w: cannot check the statement: %s", errNotRead, lr.bad))
	}
	if lr.execComment {
		return readOnlyError(errors.New("executable comments (/*! … */) are not allowed on a read-only path"))
	}
	stmts := splitStatements(lr.toks)
	for len(stmts) > 0 && len(stmts[len(stmts)-1]) == 0 {
		stmts = stmts[:len(stmts)-1]
	}
	if len(stmts) == 0 {
		return readOnlyError(errNotRead)
	}
	for _, s := range stmts {
		if err := checkStatement(s); err != nil {
			return readOnlyError(err)
		}
	}
	if len(stmts) > 1 {
		return driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New("only one statement per query is supported"))
	}
	return nil
}

// splitStatements cuts toks at every ';'.
func splitStatements(toks []token) [][]token {
	var out [][]token
	start := 0
	for i, t := range toks {
		if t.kind == tokPunct && t.text == ";" {
			out = append(out, toks[start:i])
			start = i + 1
		}
	}
	return append(out, toks[start:])
}

func isWord(toks []token, i int, words ...string) bool {
	if i >= len(toks) || toks[i].kind != tokWord {
		return false
	}
	for _, w := range words {
		if toks[i].text == w {
			return true
		}
	}
	return false
}

func isPunct(toks []token, i int, p string) bool {
	return i < len(toks) && toks[i].kind == tokPunct && toks[i].text == p
}

// isName reports whether toks[i] can name a table, column or CTE.
func isName(toks []token, i int) bool {
	return i < len(toks) && (toks[i].kind == tokWord || toks[i].kind == tokIdent)
}

// checkStatement checks one statement's tokens; nil means allowed.
func checkStatement(toks []token) error {
	for i := range toks {
		if isWord(toks, i, "INTO") && isWord(toks, i+1, "OUTFILE", "DUMPFILE") {
			return errors.New("SELECT … INTO OUTFILE/DUMPFILE is not allowed on a read-only path")
		}
	}
	switch {
	case isPunct(toks, 0, "("), isWord(toks, 0, "SELECT", "VALUES", "TABLE", "WITH"):
		return checkQuery(toks)
	case isWord(toks, 0, "SHOW", "USE"):
		return nil
	case isWord(toks, 0, "EXPLAIN", "DESCRIBE", "DESC"):
		return checkExplain(toks[1:])
	}
	return errNotRead
}

// checkQuery accepts a query expression: SELECT, VALUES, TABLE, a
// parenthesised query, or WITH whose main statement is one of those.
func checkQuery(toks []token) error {
	switch {
	case isPunct(toks, 0, "("):
		return checkQuery(toks[1:])
	case isWord(toks, 0, "SELECT", "VALUES", "TABLE"):
		return nil
	case isWord(toks, 0, "WITH"):
		return checkWith(toks)
	}
	return errNotRead
}

// checkWith walks the CTE list of "WITH [RECURSIVE] name [(cols)] AS (…), …"
// and checks the main statement after it: MySQL allows WITH … UPDATE and
// WITH … DELETE.
func checkWith(toks []token) error {
	bad := errors.New("WITH must be followed by CTEs and a SELECT, TABLE or VALUES statement on a read-only path")
	i := withMain(toks)
	if i < 0 {
		return bad
	}
	if err := checkQuery(toks[i:]); err != nil {
		return bad
	}
	return nil
}

// withMain walks the CTE list of "WITH [RECURSIVE] name [(cols)] AS (…), …"
// and returns the index of the main statement after it, or -1 when the list
// is malformed.
func withMain(toks []token) int {
	i := 1
	if isWord(toks, i, "RECURSIVE") {
		i++
	}
	for {
		if !isName(toks, i) {
			return -1
		}
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

// checkExplain checks what follows EXPLAIN, DESCRIBE or DESC: FOR CONNECTION,
// an explained query (checked like a statement of its own, because EXPLAIN
// ANALYZE runs it, and refused when it is DML), or a table name with an
// optional column name or pattern.
func checkExplain(toks []token) error {
	bad := errors.New("EXPLAIN/DESCRIBE is only allowed for a table or an allowed query on a read-only path")
	i, modifiers := 0, false
modifierLoop:
	for {
		switch {
		case isWord(toks, i, "ANALYZE", "EXTENDED", "PARTITIONS"):
			i++
		case isWord(toks, i, "FORMAT") && isPunct(toks, i+1, "=") && i+2 < len(toks):
			i += 3
		case isWord(toks, i, "INTO") && isPunct(toks, i+1, "@") && i+2 < len(toks):
			i += 3
		default:
			break modifierLoop
		}
		modifiers = true
	}
	switch {
	case i >= len(toks):
		return bad
	case isWord(toks, i, "FOR"):
		if isWord(toks, i+1, "CONNECTION") && i+3 == len(toks) {
			return nil
		}
		return bad
	case isPunct(toks, i, "("), isWord(toks, i, "SELECT", "VALUES", "TABLE", "WITH"):
		if err := checkQuery(toks[i:]); err != nil {
			return bad
		}
		return nil
	case modifiers:
		return bad
	}
	// DESCRIBE [db.]table [column | 'pattern']
	if !isName(toks, i) || isWord(toks, i, "INSERT", "REPLACE", "UPDATE", "DELETE") {
		return bad
	}
	j := i + 1
	if isPunct(toks, j, ".") {
		if !isName(toks, j+1) {
			return bad
		}
		j += 2
	}
	if j < len(toks) && (isName(toks, j) || toks[j].kind == tokString) {
		j++
	}
	if j != len(toks) {
		return bad
	}
	return nil
}

// The EXPLAIN forms Explain builds statements with: FORMAT=JSON, TREE and
// TRADITIONAL (MySQL; MariaDB has no TREE), and SingleStore's EXPLAIN JSON
// and plain EXPLAIN.
const (
	fmtJSON        = "JSON"
	fmtTree        = "TREE"
	fmtTraditional = "TRADITIONAL"
	fmtBareJSON    = "EXPLAIN JSON"
	fmtBare        = "EXPLAIN"
)

// explainFormats are the FORMAT= values explainTarget accepts.
var explainFormats = []string{fmtJSON, fmtTree, fmtTraditional}

// explainText is the statement Explain runs: EXPLAIN FORMAT=format stmt, or
// for fmtBareJSON and fmtBare, EXPLAIN JSON stmt and EXPLAIN stmt.
func explainText(format, stmt string) string {
	if format == fmtBareJSON || format == fmtBare {
		return format + " " + stmt
	}
	return "EXPLAIN FORMAT=" + format + " " + stmt
}

// explainTarget checks a statement built by explainText. It is Explain's
// allow-list, on read-only and writable connections alike: the text must be
// exactly one statement, one of the driver's prefixes (EXPLAIN FORMAT=…,
// EXPLAIN JSON or EXPLAIN) followed by a
// query (SELECT, WITH, VALUES, TABLE, a parenthesised query) or an INSERT,
// REPLACE, UPDATE or DELETE (possibly behind WITH). Plain EXPLAIN only plans
// the statement; EXPLAIN ANALYZE, which runs it, cannot be reached because
// the explained statement must start with one of those words. Text that
// cannot be lexed, executable comments (/*! … */) and SELECT … INTO
// OUTFILE/DUMPFILE are refused. The error is a plain reason; callers give it
// a kind.
func explainTarget(text string, mode lexMode) error {
	bad := errors.New("only SELECT, WITH, VALUES, TABLE, INSERT, REPLACE, UPDATE and DELETE statements can be explained")
	lr := lex(text, mode)
	switch {
	case lr.bad != "":
		return fmt.Errorf("cannot check the statement: %s", lr.bad)
	case lr.execComment:
		return errors.New("executable comments (/*! … */) are not allowed in an explained statement")
	}
	stmts := splitStatements(lr.toks)
	for len(stmts) > 0 && len(stmts[len(stmts)-1]) == 0 {
		stmts = stmts[:len(stmts)-1]
	}
	if len(stmts) != 1 {
		return errors.New("explain takes exactly one statement")
	}
	toks := stmts[0]
	var target []token
	switch {
	case !isWord(toks, 0, "EXPLAIN"):
		return bad
	case isWord(toks, 1, "FORMAT") && isPunct(toks, 2, "=") && isWord(toks, 3, explainFormats...):
		target = toks[4:]
	case isWord(toks, 1, "JSON"):
		target = toks[2:]
	default:
		target = toks[1:]
	}
	for i := range target {
		if isWord(target, i, "INTO") && isWord(target, i+1, "OUTFILE", "DUMPFILE") {
			return errors.New("SELECT … INTO OUTFILE/DUMPFILE cannot be explained")
		}
	}
	main := target
	if isWord(target, 0, "WITH") {
		i := withMain(target)
		if i < 0 {
			return bad
		}
		main = target[i:]
	}
	if checkQuery(main) == nil || isWord(main, 0, "INSERT", "REPLACE", "UPDATE", "DELETE") {
		return nil
	}
	return bad
}

// txEndingWords start statements that end a transaction, implicitly
// (https://dev.mysql.com/doc/refman/8.4/en/implicit-commit.html) or
// explicitly, or that run text the check cannot see (CALL, PREPARE,
// EXECUTE): a stored procedure may COMMIT.
var txEndingWords = map[string]bool{
	"ALTER": true, "CREATE": true, "DROP": true, "RENAME": true, "TRUNCATE": true, "INSTALL": true, "UNINSTALL": true,
	"IMPORT": true, "GRANT": true, "REVOKE": true, "LOCK": true, "UNLOCK": true, "BEGIN": true, "START": true,
	"COMMIT": true, "ROLLBACK": true, "SAVEPOINT": true, "RELEASE": true, "XA": true, "ANALYZE": true, "CACHE": true,
	"CHECK": true, "FLUSH": true, "OPTIMIZE": true, "REPAIR": true, "RESET": true, "CHANGE": true, "STOP": true,
	"PURGE": true, "CALL": true, "PREPARE": true, "EXECUTE": true, "DEALLOCATE": true,
	// MariaDB: BACKUP STAGE/LOCK, and the compound statements it runs
	// outside stored programs, which can hold a COMMIT.
	"BACKUP": true, "IF": true, "CASE": true, "LOOP": true, "REPEAT": true, "WHILE": true, "FOR": true,
}

// errTxStatement is the reason checkTxStatement gives.
var errTxStatement = errors.New("this statement would end the transaction (MySQL commits implicitly on DDL, LOCK TABLES, SET autocommit and similar statements) and is not allowed inside a transaction: run it outside one, and end the transaction with Commit or Rollback")

// checkTxStatement is the check a writable transaction applies to user
// text. MySQL ends a transaction implicitly on DDL, LOCK TABLES, SET
// autocommit and other statements, which would leave later statements
// running in autocommit. It refuses, with ErrUnsupported:
//
//   - statements starting with a word in txEndingWords, plus LOAD INDEX,
//     SET PASSWORD, SET DEFAULT ROLE, any SET that names autocommit, and
//     MariaDB's SET STATEMENT … FOR and labelled compound statements;
//   - text that cannot be lexed, and executable comments (/*! … */), which
//     could hide such a statement;
//   - several statements.
//
// It is a deny-list. The transaction's post-check (still in the transaction
// after every statement) is the fail-closed layer behind it.
func checkTxStatement(text string, mode lexMode) error {
	refuse := func(err error) error { return driver.Wrap(driver.ErrUnsupported, Name, "query", err) }
	lr := lex(text, mode)
	switch {
	case lr.bad != "":
		return refuse(fmt.Errorf("cannot check the statement: %s", lr.bad))
	case lr.execComment:
		return refuse(errors.New("executable comments (/*! … */) are not allowed inside a transaction"))
	}
	stmts := splitStatements(lr.toks)
	for len(stmts) > 0 && len(stmts[len(stmts)-1]) == 0 {
		stmts = stmts[:len(stmts)-1]
	}
	for _, s := range stmts {
		if endsTx(s) {
			return refuse(errTxStatement)
		}
	}
	if len(stmts) > 1 {
		return refuse(errors.New("only one statement per query is supported"))
	}
	return nil
}

// endsTx reports whether one statement's tokens would end a transaction.
func endsTx(toks []token) bool {
	switch {
	case len(toks) == 0:
		return false
	case toks[0].kind == tokWord && txEndingWords[toks[0].text]:
		return true
	case isWord(toks, 0, "LOAD"):
		return isWord(toks, 1, "INDEX")
	case isName(toks, 0) && isPunct(toks, 1, ":"): // MariaDB "label: BEGIN NOT ATOMIC … END"
		return true
	case isWord(toks, 0, "SET"):
		// MariaDB's SET STATEMENT … FOR <statement> hides the statement it runs.
		if isWord(toks, 1, "PASSWORD", "STATEMENT") || isWord(toks, 1, "DEFAULT") && isWord(toks, 2, "ROLE") {
			return true
		}
		for _, t := range toks { // autocommit, @@autocommit, `autocommit`, @@session.`autocommit` …
			if (t.kind == tokWord || t.kind == tokIdent) && t.text == "AUTOCOMMIT" {
				return true
			}
		}
	}
	return false
}

// checkRawFilter checks a Browse raw filter, lexed with the session's mode,
// before it is wrapped in
// parentheses: it must lex, keep its parentheses balanced without ever
// closing more than it opened, and hold no ';' outside parentheses, so it
// cannot close the wrapping parenthesis and escape the other conditions
// (keyset, filters) or add a statement. Anything else is ErrSyntax.
func checkRawFilter(raw string, mode lexMode) error {
	bad := func(reason string) error {
		return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid raw filter: "+reason))
	}
	lr := lex(raw, mode)
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
		case t.text == ";" && depth == 0:
			return bad("';' is not allowed")
		}
	}
	if depth != 0 {
		return bad("unbalanced parentheses")
	}
	return nil
}
