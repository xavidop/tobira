package vertica

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/vertica/vertica-sql-go/parse"

	"github.com/xavidop/tobira/driver"
)

// The read-only path has three layers.
//
//  1. checkReadOnly, a lexical allow-list run before anything is sent:
//     one SELECT, WITH, SHOW or EXPLAIN statement with no write, session,
//     transaction, COPY, EXPORT or CONNECT word in it, and no function call
//     off the allow-list (functions.go). The allow-list matters most:
//     Vertica's meta-functions act from a SELECT (SELECT SHUTDOWN(),
//     SET_CONFIG_PARAMETER, PURGE, EXPORT_TABLES …), and they run even
//     inside a READ ONLY transaction.
//  2. The server: every read-only call runs inside START TRANSACTION READ
//     ONLY and always ends in ROLLBACK, and a connection opened read-only
//     also sets SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY on
//     every session it opens, so a write the guard missed fails with
//     SQLSTATE 25006.
//  3. checkShape, on every path: one statement, as the guard and as
//     vertica-sql-go's client-side splitter count them (it would run each
//     piece it finds on its own, outside the server's one-statement Parse),
//     and no COPY that reads the client's files or standard input.

// readStatements are the statements a read-only call may run.
var readStatements = map[string]bool{"SELECT": true, "WITH": true, "SHOW": true, "EXPLAIN": true}

// deniedWords are refused anywhere outside strings and quoted names in a
// read-only statement. Each starts or hides a write, a session or
// transaction change, a call out or a data movement: WITH … DELETE,
// SELECT … INTO, FOR UPDATE, EXPLAIN ANALYZE (it runs the statement),
// seq.NEXTVAL (advances the sequence), COPY/EXPORT/CONNECT. A column with
// one of these names must be written in double quotes.
var deniedWords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "TRUNCATE": true, "UPSERT": true,
	"CREATE": true, "ALTER": true, "DROP": true, "GRANT": true, "REVOKE": true, "COMMENT": true, "RENAME": true,
	"CALL": true, "EXECUTE": true, "PERFORM": true, "DO": true, "PROFILE": true, "ANALYZE": true,
	"BEGIN": true, "START": true, "COMMIT": true, "ROLLBACK": true, "ABORT": true,
	"SAVEPOINT": true, "RELEASE": true, "SET": true, "RESET": true, "LOCK": true,
	"COPY": true, "EXPORT": true, "IMPORT": true, "CONNECT": true, "DISCONNECT": true, "INTO": true,
	"NEXTVAL": true, "CURRVAL": true, "ACTIVATE": true, "DEACTIVATE": true, "SAVE": true, "REPLICATE": true,
	"PREPARE": true, "DEALLOCATE": true, "LISTEN": true, "NOTIFY": true, "REFRESH": true,
}

// deniedNames are the denied words that may also appear quoted and still
// mean the sequence operation (Vertica names are case-insensitive, quoted
// or not): seq."NEXTVAL".
var deniedNames = map[string]bool{"NEXTVAL": true, "CURRVAL": true}

// errNotRead is the reason given for a statement outside the allow-list.
var errNotRead = errors.New("only SELECT, WITH, SHOW and EXPLAIN statements are allowed on a read-only path")

func readOnlyError(op string, reason error) error {
	return driver.Wrap(driver.ErrReadOnly, Name, op, reason)
}

// checkReadOnly is the read-only path's allow-list for a Query's text.
func checkReadOnly(text string) error { return checkReadOnlyOp("query", text) }

// checkReadOnlyOp accepts text when it holds exactly one statement
// (trailing ';' are fine) that starts with SELECT, WITH, SHOW or EXPLAIN
// (or parentheses around a SELECT or WITH), holds no non-ASCII byte outside
// strings, quoted names and comments, and uses none of deniedWords,
// no quoted deniedNames, no function call off the allow-list, and no
// backslash inside a plain '…' string (whether it escapes depends on the
// StandardConformingStrings setting; use E'…'). It fails closed: text the
// lexer cannot read, and anything else, is ErrReadOnly. The error names
// the reason, never the statement.
func checkReadOnlyOp(op, text string) error {
	refuse := func(why string) error { return readOnlyError(op, errors.New(why)) }
	toks, err := lex(text)
	if err != nil {
		return refuse("read-only: cannot check the statement: " + err.Error())
	}
	toks, ok := oneStatement(toks)
	if !ok {
		return refuse("read-only: exactly one statement is allowed")
	}
	first := 0
	for first < len(toks) && isPunct(toks[first], "(") {
		first++
	}
	if first == len(toks) || toks[first].kind != tokWord || !readStatements[strings.ToUpper(toks[first].text)] {
		return readOnlyError(op, errNotRead)
	}
	if w := strings.ToUpper(toks[first].text); first > 0 && w != "SELECT" && w != "WITH" {
		return readOnlyError(op, errNotRead)
	}
	for _, t := range toks {
		switch t.kind {
		case tokPunct:
			// Vertica names may hold non-ASCII letters (malé); the lexer
			// splits them, so the guard could not judge such a name.
			if t.text[0] >= 0x80 {
				return refuse("read-only: non-ASCII characters are allowed only in strings, quoted names and comments")
			}
		case tokWord:
			if w := strings.ToUpper(t.text); deniedWords[w] {
				return refuse("read-only: " + w + " is not allowed (quote a column of that name with double quotes)")
			}
		case tokQuoted:
			if deniedNames[strings.ToUpper(unquote(t.text))] {
				return refuse("read-only: this name is not allowed, quoted or not")
			}
		case tokString:
			if t.text[0] == '\'' && strings.Contains(t.text, `\`) {
				return refuse(`read-only: a backslash in a '…' string is read differently when StandardConformingStrings is off; write E'…' with \\ instead`)
			}
		}
	}
	return checkFunctions(op, toks)
}

// checkFunctions refuses a function call that is not on the allow-list: a
// name (dotted or not) followed by '('. A word before '(' that opens a
// subquery, a list or a clause (FROM (, IN (, OVER (…) is not a call, and
// neither is a type name with a length or precision (VARCHAR(20)). A quoted
// name before '(' is refused outright, and so is a qualified name: the
// allow-list holds built-ins, which need no schema.
func checkFunctions(op string, toks []token) error {
	cte := cteColumnLists(toks)
	for i := 1; i < len(toks); i++ {
		if !isPunct(toks[i], "(") {
			continue
		}
		prev := toks[i-1]
		if cte[i] {
			continue // a CTE column list, after a bare or quoted name
		}
		switch prev.kind {
		case tokQuoted:
			return readOnlyError(op, errors.New("read-only: calling a function by a quoted name is not allowed"))
		case tokWord:
		default:
			continue
		}
		w := strings.ToUpper(prev.text)
		qualified := i >= 2 && isPunct(toks[i-2], ".")
		if !qualified && notACall(toks, i-1, w) {
			continue
		}
		if qualified || !functionsAllowed[w] {
			name := w
			if qualified {
				name = "a schema-qualified function"
			}
			return readOnlyError(op, errors.New("read-only: "+name+" is not a function the read-only guard allows "+
				"(Vertica meta-functions act from a SELECT); run it on a connection that is not read-only"))
		}
	}
	return nil
}

// notACall reports whether the word w at toks[at], followed by '(', opens
// something other than a function call. Only reserved words qualify on
// their own; other words count only in the one context where they cannot
// name a function: a type with a length after a literal :: or AS (CAST),
// ROLLUP and CUBE right after GROUP BY, SETS after GROUPING, OVER after
// ')' or a string (an analytic clause, TIMESERIES … OVER), and EXISTS when
// the '(' opens a SELECT or WITH subquery. Anything else is judged as a
// call: BY, IS and BETWEEN are not reserved in Vertica.
func notACall(toks []token, at int, w string) bool {
	if reservedBeforeParen[w] {
		return true
	}
	var before token
	if at > 0 {
		before = toks[at-1]
	}
	switch {
	case typeNames[w]:
		return isWord(before, "AS") || at >= 2 && isPunct(before, ":") && isPunct(toks[at-2], ":") &&
			toks[at-2].pos+1 == before.pos
	case w == "ROLLUP" || w == "CUBE":
		return isWord(before, "BY") && at >= 2 && isWord(toks[at-2], "GROUP")
	case w == "EXISTS":
		return at+2 < len(toks) && (isWord(toks[at+2], "SELECT") || isWord(toks[at+2], "WITH"))
	case w == "SETS":
		return isWord(before, "GROUPING")
	case w == "OVER":
		return isPunct(before, ")") || before.kind == tokString
	}
	return false
}

func isWord(t token, w string) bool { return t.kind == tokWord && strings.EqualFold(t.text, w) }

// cteColumnLists returns the indexes of the '(' tokens that open a common
// table expression's column list: in a statement that starts with WITH,
// before its main query, at the top level, name (a, b) AS (…) right after
// WITH, RECURSIVE or ','. The list holds only names and commas. The scan
// stops at the main query: the first top-level '(' that neither opens such
// a list nor follows AS, or the first top-level word that is not
// RECURSIVE, AS or a CTE name.
func cteColumnLists(toks []token) map[int]bool {
	out := map[int]bool{}
	if len(toks) == 0 || !isWord(toks[0], "WITH") {
		return out
	}
	depth := 0
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		switch {
		case isPunct(t, "("):
			if depth == 0 {
				switch {
				case i >= 2 && toks[i-1].kind != tokPunct && cteList(toks, i):
					out[i] = true
				case !isWord(toks[i-1], "AS"):
					return out // a parenthesised main query
				}
			}
			depth++
		case isPunct(t, ")"):
			depth--
		case depth == 0 && (t.kind == tokWord || t.kind == tokQuoted):
			if isWord(t, "RECURSIVE") || isWord(t, "AS") {
				continue
			}
			next := i + 1
			if next < len(toks) && (isPunct(toks[next], "(") || isWord(toks[next], "AS")) && !isWord(t, "SELECT") {
				continue // a CTE name
			}
			return out // the main query
		}
	}
	return out
}

// cteList checks the shape at toks[open]: preceded by name and WITH,
// RECURSIVE or ',', holding only names and commas, and followed by
// ") AS (".
func cteList(toks []token, open int) bool {
	before := toks[open-2]
	if !isPunct(before, ",") && (before.kind != tokWord ||
		!strings.EqualFold(before.text, "WITH") && !strings.EqualFold(before.text, "RECURSIVE")) {
		return false
	}
	names := 0
	for j := open + 1; j < len(toks); j++ {
		switch t := toks[j]; {
		case t.kind == tokWord || t.kind == tokQuoted:
			names++
		case isPunct(t, ","):
		case isPunct(t, ")"):
			return names > 0 && j+2 < len(toks) && toks[j+1].kind == tokWord &&
				strings.EqualFold(toks[j+1].text, "AS") && isPunct(toks[j+2], "(")
		default:
			return false
		}
	}
	return false
}

// oneStatement returns the tokens of the only statement in toks, dropping
// trailing ';'. ok is false when a second statement follows or there is
// none.
func oneStatement(toks []token) (_ []token, ok bool) {
	end := len(toks)
	for i, t := range toks {
		if isPunct(t, ";") {
			end = i
			break
		}
	}
	for _, t := range toks[end:] {
		if !isPunct(t, ";") {
			return nil, false
		}
	}
	return toks[:end], end > 0
}

func isPunct(t token, p string) bool { return t.kind == tokPunct && t.text == p }

// checkShape is checked on every path before text is sent: it must lex,
// hold exactly one statement as the guard counts them and as
// vertica-sql-go's splitter does (parse.SplitStatements), and must not be
// a COPY from the client: COPY … FROM LOCAL makes vertica-sql-go open the
// files the server names, and COPY … FROM STDIN reads the process's
// standard input. Server-side COPY (a file on a Vertica node) is left to
// the server's privileges.
func checkShape(op, text string) error {
	refuse := func(kind error, why string) error { return driver.Wrap(kind, Name, op, errors.New(why)) }
	toks, err := lex(text)
	if err != nil {
		return refuse(driver.ErrSyntax, "cannot read the statement: "+err.Error())
	}
	if _, ok := oneStatement(toks); !ok {
		return refuse(driver.ErrSyntax, "exactly one statement is allowed per query")
	}
	if len(parse.SplitStatements(text)) != 1 {
		return refuse(driver.ErrSyntax, "exactly one statement is allowed per query (vertica-sql-go splits this text into several)")
	}
	// The words are taken from the raw text, ignoring quotes and comments:
	// vertica-sql-go finds COPY … LOCAL with a tokenizer of its own, which
	// does not know E'…' strings, and must never find one the guard missed.
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToUpper(text), func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '$'
	}) {
		words[w] = true
	}
	if words["COPY"] && (words["LOCAL"] || words["STDIN"]) {
		return refuse(driver.ErrUnsupported, "COPY FROM LOCAL and COPY FROM STDIN are not supported: they read files or input on this machine")
	}
	return nil
}

// explainTarget checks the text Explain puts after EXPLAIN: one statement
// that starts with SELECT, WITH, INSERT, UPDATE, DELETE or MERGE (or a
// parenthesised SELECT). EXPLAIN options in the text (ANALYZE runs the
// statement) are refused on every path.
func explainTarget(op, text string) error {
	if err := checkShape(op, text); err != nil {
		return err
	}
	toks, _ := lex(text)
	first := 0
	for first < len(toks) && isPunct(toks[first], "(") {
		first++
	}
	if first < len(toks) && toks[first].kind == tokWord {
		switch strings.ToUpper(toks[first].text) {
		case "SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "MERGE":
			return nil
		}
	}
	return driver.Wrap(driver.ErrSyntax, Name, op, errors.New("EXPLAIN takes a SELECT, WITH, INSERT, UPDATE, DELETE or MERGE statement, without EXPLAIN options"))
}

// txControl are the leading words of statements that start, end or shape
// a transaction.
var txControl = map[string]bool{
	"BEGIN": true, "START": true, "COMMIT": true, "ROLLBACK": true, "END": true, "ABORT": true,
	"SAVEPOINT": true, "RELEASE": true,
}

// txControlRefusal refuses transaction control in a plain Query: on a
// pooled session it would leave a transaction (or autocommit off) behind
// for the next caller. Text that does not lex is left to checkShape.
func txControlRefusal(text string) error {
	toks, err := lex(text)
	if err != nil || len(toks) == 0 || toks[0].kind != tokWord {
		return nil
	}
	w := strings.ToUpper(toks[0].text)
	if txControl[w] || w == "SET" && hasWord(toks, "AUTOCOMMIT") {
		return driver.Wrap(driver.ErrUnsupported, Name, "query",
			errors.New(w+" is not allowed in a query: use Begin, Commit and Rollback (sessions are pooled)"))
	}
	return nil
}

// txStatements are the words refused inside a transaction: Vertica commits
// the open transaction before every DDL statement, and the others end it,
// change it or the session, or load data outside it.
var txStatements = map[string]bool{
	"BEGIN": true, "START": true, "COMMIT": true, "ROLLBACK": true, "END": true, "ABORT": true,
	"SAVEPOINT": true, "RELEASE": true, "SET": true,
	"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true, "GRANT": true, "REVOKE": true,
	"COMMENT": true, "RENAME": true, "COPY": true, "EXPORT": true, "CONNECT": true, "DISCONNECT": true,
}

// txStatementRefusal returns why text may not run inside a transaction, or
// nil. Every word is checked, not only the first, so a word hidden behind
// another fails too; a column named like one of txStatements must be
// double-quoted. Text it cannot lex is refused.
func txStatementRefusal(text string) error {
	refuse := func(why string) error { return driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New(why)) }
	toks, err := lex(text)
	if err != nil {
		return refuse("cannot check the statement: " + err.Error())
	}
	for _, t := range toks {
		if t.kind != tokWord {
			continue
		}
		if w := strings.ToUpper(t.text); txStatements[w] {
			if w == "SET" && isUpdateSet(toks, t.pos) || w == "END" && t.pos != toks[0].pos {
				continue // UPDATE … SET, CASE … END
			}
			return refuse(w + " is not allowed inside a transaction: Vertica would end or commit it; " +
				"use Commit and Rollback, and run DDL outside the transaction")
		}
	}
	return nil
}

// isUpdateSet reports whether the SET at byte offset pos is the SET clause
// of an UPDATE or of MERGE's WHEN MATCHED THEN UPDATE: the word right
// before the target list starts with UPDATE.
func isUpdateSet(toks []token, pos int) bool {
	if toks[0].kind == tokWord && strings.EqualFold(toks[0].text, "UPDATE") {
		for _, t := range toks[1:] {
			if t.pos == pos {
				return true
			}
			if t.kind == tokWord && strings.EqualFold(t.text, "SET") {
				return false
			}
		}
	}
	if toks[0].kind == tokWord && strings.EqualFold(toks[0].text, "MERGE") {
		for i, t := range toks {
			if t.pos == pos {
				return i > 0 && toks[i-1].kind == tokWord && strings.EqualFold(toks[i-1].text, "UPDATE")
			}
		}
	}
	return false
}

func hasWord(toks []token, w string) bool {
	for _, t := range toks {
		if t.kind == tokWord && strings.EqualFold(t.text, w) {
			return true
		}
	}
	return false
}

// checkRawFilter checks a Browse raw filter, a WHERE body that is pasted
// into the generated SELECT between parentheses: it must lex, keep its
// parentheses balanced (so it cannot close ours) and hold no ';'. A comment
// may close the filter's line: the generated text ends it with a newline.
// On a read-only call the whole generated SELECT then passes checkReadOnly.
func checkRawFilter(raw string) error {
	bad := func(reason string) error {
		return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid raw filter: "+reason))
	}
	toks, err := lex(raw)
	if err != nil {
		return bad(err.Error())
	}
	depth := 0
	for _, t := range toks {
		switch {
		case isPunct(t, ";"):
			return bad("';' is not allowed")
		case isPunct(t, "("):
			depth++
		case isPunct(t, ")"):
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

// quoteIdent quotes a schema, table or column name; "" stands for a quote.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// maxNameLen is Vertica's identifier limit in bytes.
const maxNameLen = 128

// checkSchemaName vets a caller's namespace before it is quoted into SET
// SEARCH_PATH, which runs outside the statement guard: not empty, valid
// UTF-8, no NUL, at most maxNameLen bytes. quoteIdent handles '"'.
func checkSchemaName(op, ns string) error {
	if ns == "" || len(ns) > maxNameLen || !utf8.ValidString(ns) || strings.IndexByte(ns, 0) >= 0 {
		return driver.Wrap(driver.ErrSyntax, Name, op, errors.New("invalid namespace: a schema name of 1 to 128 bytes of UTF-8"))
	}
	return nil
}
