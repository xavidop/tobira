package snowflake

import (
	"errors"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// The read-only path has one hard layer, the lexical allow-list below,
// checked before any text is sent, plus MULTI_STATEMENT_COUNT=1 on the
// session and on every request, so the server itself refuses a second
// statement. Snowflake has no read-only session or transaction mode: there
// is no TRANSACTION_READ_ONLY parameter, and BEGIN takes no READ ONLY. The
// one server-side layer a user can add is a role that holds only SELECT
// grants.

// readStatements are the statements a read-only call may run.
var readStatements = map[string]bool{
	"SELECT": true, "WITH": true, "SHOW": true, "DESC": true, "DESCRIBE": true, "EXPLAIN": true,
}

// deniedWords are refused anywhere outside strings and quoted names in a
// read-only statement. Each can turn an allowed statement into a write or a
// call out: WITH … DELETE, WITH p AS PROCEDURE … CALL p(), EXPLAIN of a
// write, SELECT … INTO, FOR UPDATE, COMMENT ON, START TRANSACTION,
// seq.NEXTVAL (advances the sequence),
// INFER_SCHEMA (reads stage files) and IDENTIFIER(), which can name a
// function the guard cannot see. A column with one of these names must be
// written in double quotes.
var deniedWords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "TRUNCATE": true,
	"CREATE": true, "ALTER": true, "DROP": true, "UNDROP": true, "GRANT": true, "REVOKE": true,
	"CALL": true, "PROCEDURE": true, "EXECUTE": true, "IMMEDIATE": true, "DECLARE": true,
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true, "SET": true, "UNSET": true, "USE": true,
	"COPY": true, "PUT": true, "REMOVE": true, "INTO": true, "COMMENT": true, "START": true,
	"NEXTVAL": true, "INFER_SCHEMA": true, "GENERATE_COLUMN_DESCRIPTION": true, "IDENTIFIER": true,
}

// deniedNames are the denied words that name a function or a sequence
// member rather than a statement: a quoted name resolves to them too
// ("NEXTVAL", "INFER_SCHEMA"), so quoted names are checked against these
// and against the SYSTEM$ rule. (A quoted "update" stays a plain name.)
var deniedNames = map[string]bool{
	"NEXTVAL": true, "INFER_SCHEMA": true, "GENERATE_COLUMN_DESCRIPTION": true, "IDENTIFIER": true,
}

// systemFunctionsAllowed are the SYSTEM$ functions a read-only statement
// may call: pure functions of their arguments. Every other SYSTEM$ function
// is refused, since many act on the account (SYSTEM$CANCEL_ALL_QUERIES,
// SYSTEM$ABORT_SESSION, SYSTEM$WAIT …).
var systemFunctionsAllowed = map[string]bool{
	"SYSTEM$TYPEOF": true,
}

// errNotRead is the reason given for a statement outside the allow-list.
var errNotRead = errors.New("only SELECT, WITH, SHOW, DESCRIBE and EXPLAIN statements are allowed on a read-only path")

func readOnlyError(op string, reason error) error {
	return driver.Wrap(driver.ErrReadOnly, Name, op, reason)
}

// checkReadOnly is the read-only path's allow-list. text is accepted when
// it holds exactly one statement (trailing ';' are fine) that starts with
// SELECT, WITH, SHOW, DESCRIBE, DESC or EXPLAIN, or with parentheses around
// a SELECT or WITH, and uses none of:
//
//   - deniedWords, nor a deniedNames entry or a SYSTEM$ function off
//     systemFunctionsAllowed, quoted or not;
//   - a stage reference (@name, @~, @%table): stages hold files outside
//     the database (SELECT $1 FROM @s, DIRECTORY(@s), GET_PRESIGNED_URL);
//   - the flow operator ->>, which chains statements (SHOW … ->> DELETE);
//   - a table function that is not a known built-in (checkTableFunctions):
//     a stored procedure that returns a table can be selected FROM
//     TABLE(proc(…)), and its body may run DML.
//
// Text the lexer cannot read is refused too. It fails closed: anything else
// is ErrReadOnly. The error names the reason, never the statement.
func checkReadOnly(text string) error { return checkReadOnlyOp("query", text) }

func checkReadOnlyOp(op, text string) error {
	refuse := func(why string) error { return readOnlyError(op, errors.New(why)) }
	toks, err := lex(text)
	if err != nil {
		return refuse("read-only: cannot check the statement: " + err.Error())
	}
	toks, ok := oneStatement(toks)
	if !ok {
		return refuse("read-only: only one statement is allowed")
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
	for i, t := range toks {
		switch t.kind {
		case tokWord:
			w := strings.ToUpper(t.text)
			if deniedWords[w] {
				return refuse("read-only: " + w + ` is not allowed (quote a column of that name with double quotes)`)
			}
			if systemFunction(w) {
				return refuse("read-only: " + w + " is not allowed")
			}
		case tokQuoted:
			w := strings.ToUpper(unquote(t.text))
			if deniedNames[w] || systemFunction(w) {
				return refuse("read-only: this name is not allowed, quoted or not")
			}
		case tokPunct:
			switch {
			case t.text == "@":
				return refuse("read-only: stages (@name) are not allowed")
			case t.text == "-" && i+2 < len(toks) && isPunct(toks[i+1], ">") && isPunct(toks[i+2], ">"):
				return refuse("read-only: the flow operator ->> is not allowed")
			}
		}
	}
	return checkTableFunctions(op, toks)
}

// tableFunctionsAllowed are the built-in table functions a read-only
// statement may call (https://docs.snowflake.com/en/sql-reference/functions-table):
// they only generate or reshape rows. Any INFORMATION_SCHEMA.<name> table
// function (QUERY_HISTORY, TASK_HISTORY, COPY_HISTORY …, which only read
// history) is allowed too, when it is qualified with INFORMATION_SCHEMA.
// Names match unquoted only (see checkTableFunctions).
var tableFunctionsAllowed = map[string]bool{
	"GENERATOR": true, "RESULT_SCAN": true, "FLATTEN": true, "SPLIT_TO_TABLE": true, "STRTOK_SPLIT_TO_TABLE": true,
}

// clauseWords are words a '(' after which opens a subquery or a list, not a
// function call's arguments.
var clauseWords = map[string]bool{
	"SELECT": true, "FROM": true, "JOIN": true, "ON": true, "WHERE": true, "AND": true, "OR": true, "NOT": true,
	"IN": true, "EXISTS": true, "AS": true, "ANY": true, "ALL": true, "SOME": true, "UNION": true, "INTERSECT": true,
	"EXCEPT": true, "MINUS": true, "LATERAL": true, "USING": true, "VALUES": true, "OVER": true, "WITH": true,
	"HAVING": true, "BY": true, "QUALIFY": true, "WHEN": true, "THEN": true, "ELSE": true, "RECURSIVE": true,
}

// tableFunctionRefusal is the refusal for a table function off the
// allow-list, with the way out.
func tableFunctionRefusal(op, name string) error {
	return readOnlyError(op, errors.New("read-only: the table function "+name+" is not a built-in the guard allows "+
		"(a stored procedure can return a table and run DML); only GENERATOR, RESULT_SCAN, FLATTEN, SPLIT_TO_TABLE, "+
		"STRTOK_SPLIT_TO_TABLE and INFORMATION_SCHEMA table functions are allowed. To call it, use a connection "+
		"that is not read-only, with a role that holds only SELECT grants"))
}

// fromEnders are the words that end a FROM clause at its own level.
var fromEnders = map[string]bool{
	"WHERE": true, "GROUP": true, "HAVING": true, "QUALIFY": true, "ORDER": true, "LIMIT": true, "OFFSET": true,
	"FETCH": true, "UNION": true, "INTERSECT": true, "EXCEPT": true, "MINUS": true, "WINDOW": true, "SELECT": true,
}

// checkTableFunctions refuses, on a read-only path, a table function that
// is not on tableFunctionsAllowed: every TABLE(<name>(…)), and a <name>(…)
// that starts a FROM item (after FROM, JOIN, LATERAL or a comma of a FROM
// clause) outside a function's arguments (EXTRACT(year FROM f(x)) is a
// scalar call). A quoted part in the name is refused: quoted names are
// case-sensitive user objects ("generator" is not GENERATOR), so the
// allow-list matches unquoted names only. TABLE() may also hold a string
// or a variable naming a table; anything else in it is refused, failing
// closed.
func checkTableFunctions(op string, toks []token) error {
	var callParens []bool   // for each open '(': does it hold a call's arguments?
	inFrom := []bool{false} // per paren level: inside a FROM clause, between items
	judge := func(j int) error {
		parts, quoted, next := qualifiedName(toks, j)
		if parts == nil || next >= len(toks) || !isPunct(toks[next], "(") ||
			!quoted && len(parts) == 1 && (parts[0] == "TABLE" || parts[0] == "LATERAL") {
			return nil
		}
		if quoted || !tableFunctionAllowed(parts) {
			return tableFunctionRefusal(op, strings.Join(parts, "."))
		}
		return nil
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		level := len(callParens)
		switch {
		case isPunct(t, "("):
			call := i > 0 && (toks[i-1].kind == tokQuoted ||
				toks[i-1].kind == tokWord && !clauseWords[strings.ToUpper(toks[i-1].text)])
			callParens = append(callParens, call)
			inFrom = append(inFrom, false)
			continue
		case isPunct(t, ")"):
			if level > 0 {
				callParens = callParens[:level-1]
				inFrom = inFrom[:level]
			}
			continue
		case isPunct(t, ","):
			if inFrom[level] {
				if err := judge(i + 1); err != nil {
					return err
				}
			}
			continue
		case t.kind != tokWord:
			continue
		}
		w := strings.ToUpper(t.text)
		inCall := level > 0 && callParens[level-1]
		switch {
		case w == "TABLE" && i+1 < len(toks) && isPunct(toks[i+1], "("):
			j := i + 2
			if j < len(toks) && (toks[j].kind == tokString || toks[j].kind == tokVar) {
				continue
			}
			parts, quoted, next := qualifiedName(toks, j)
			if parts == nil || next >= len(toks) || !isPunct(toks[next], "(") {
				return readOnlyError(op, errors.New("read-only: TABLE() may hold only a string, a variable or a built-in table function"))
			}
			if quoted || !tableFunctionAllowed(parts) {
				return tableFunctionRefusal(op, strings.Join(parts, "."))
			}
		case (w == "FROM" || w == "JOIN" || w == "LATERAL") && !inCall:
			if w == "FROM" {
				inFrom[level] = true
			}
			if err := judge(i + 1); err != nil {
				return err
			}
		case fromEnders[w]:
			inFrom[level] = false
		}
	}
	return nil
}

// qualifiedName reads a dotted name (words and quoted names) at toks[j]. It
// returns the parts (words upper-cased, quoted names as written), whether
// any part was quoted, and the index after the name; parts is nil when no
// name starts at j.
func qualifiedName(toks []token, j int) (parts []string, quoted bool, next int) {
	for j < len(toks) {
		switch toks[j].kind {
		case tokWord:
			parts = append(parts, strings.ToUpper(toks[j].text))
		case tokQuoted:
			parts = append(parts, unquote(toks[j].text))
			quoted = true
		default:
			return nil, false, j
		}
		j++
		if j < len(toks) && isPunct(toks[j], ".") {
			j++
			continue
		}
		return parts, quoted, j
	}
	return nil, false, j
}

// tableFunctionAllowed reports whether a table function name (upper-cased,
// unquoted parts) is a built-in on the allow-list or an INFORMATION_SCHEMA
// function.
func tableFunctionAllowed(parts []string) bool {
	switch n := len(parts); {
	case n == 1:
		return tableFunctionsAllowed[parts[0]]
	case n <= 3:
		return parts[n-2] == "INFORMATION_SCHEMA"
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

// systemFunction reports whether an upper-cased name is a SYSTEM$ function
// a read-only statement may not call.
func systemFunction(w string) bool {
	return strings.HasPrefix(w, "SYSTEM$") && !systemFunctionsAllowed[w]
}

func isPunct(t token, p string) bool { return t.kind == tokPunct && t.text == p }

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

// quoteIdent quotes a database, schema, table or column name: Snowflake
// keeps a quoted name's case, and "" stands for a quote.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// txStatements are the leading words refused inside a transaction: they end
// it (BEGIN, START, COMMIT, ROLLBACK), or Snowflake commits the open
// transaction before running them (every DDL statement, and TRUNCATE), or
// they run code that may (CALL, EXECUTE IMMEDIATE), or they change the
// session's schema under the transaction (USE).
var txStatements = map[string]bool{
	"BEGIN": true, "START": true, "COMMIT": true, "ROLLBACK": true,
	"CREATE": true, "ALTER": true, "DROP": true, "UNDROP": true, "GRANT": true, "REVOKE": true,
	"COMMENT": true, "TRUNCATE": true, "CALL": true, "PROCEDURE": true, "EXECUTE": true, "USE": true,
}

// txStatementRefusal returns why text may not run inside a transaction, or
// nil. Every word is checked, not only the first (a chained or embedded
// statement commits as well: SELECT 1 ->> CREATE …, WITH p AS PROCEDURE …
// CALL p()), and the flow operator is refused. It fails closed: text it
// cannot lex is refused, and a column named like one of txStatements must
// be double-quoted.
func txStatementRefusal(text string) error {
	refuse := func(why string) error {
		return driver.Wrap(driver.ErrUnsupported, Name, "query", errors.New(why))
	}
	toks, err := lex(text)
	if err != nil {
		return refuse("cannot check the statement: " + err.Error())
	}
	for i, t := range toks {
		if t.kind == tokWord {
			if w := strings.ToUpper(t.text); txStatements[w] {
				return refuse(w + " is not allowed inside a transaction: Snowflake would end or commit it; " +
					"use Commit and Rollback, and run DDL outside the transaction")
			}
		}
		if isPunct(t, "-") && i+2 < len(toks) && isPunct(toks[i+1], ">") && isPunct(toks[i+2], ">") {
			return refuse("the flow operator ->> is not allowed inside a transaction")
		}
	}
	return nil
}
