package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// The read-only path has two layers. The hard one is the server's readonly
// mode: the setting readonly=1, sent with every read-only call (see
// pool.roCtx), or the user's own profile setting readonly (1 or 2), which
// Open probes and then leaves alone, since the server refuses to change it.
// With readonly=1 the server refuses writes, DDL, SYSTEM and OPTIMIZE, any settings change (SET or
// a SETTINGS clause, readonly itself included) and the table functions that
// reach outside it (url, s3, file, remote …). The first layer is the
// lexical allow-list below, checked before any text is sent. It fails
// closed, and it also stops what readonly=1 lets through: KILL QUERY, which
// a user may run on queries they own.

// A user whose profile already sets readonly takes no readonly=1 from the
// driver (the server refuses the change). readonly=1 there still refuses
// table functions, but readonly=2 does not: such a user may read url(), s3(),
// file(), remote() … wherever its grants allow. So on that read-only path the
// driver checks table functions itself (pool.checkTableFunctions): the
// server parses the text (EXPLAIN AST) and the driver walks the tree,
// allowing only a few row generators and refusing everything else,
// including output it cannot read. It fails closed for both levels.

// token is one significant token of a query: comments and whitespace are
// dropped.
type token struct {
	kind sqlscan.Kind
	text string // upper-cased for words; the raw text otherwise
}

// lex splits text into significant tokens with driverutil/sqlscan's
// ClickHouse dialect. bad says why the text cannot be judged.
func lex(text string) (toks []token, bad string) {
	if strings.IndexByte(text, 0) >= 0 {
		return nil, "the text contains a NUL byte"
	}
	sqlscan.Scan(sqlscan.ClickHouse(), text, func(t sqlscan.Token) bool {
		raw := text[t.Start:t.End]
		switch t.Kind {
		case sqlscan.Space, sqlscan.Comment:
		case sqlscan.Unterminated:
			switch raw[0] {
			case '/':
				bad = "unterminated comment"
			case '`', '"':
				bad = "unterminated quoted identifier"
			default:
				bad = "unterminated quoted text"
			}
		case sqlscan.Word:
			toks = append(toks, token{kind: t.Kind, text: strings.ToUpper(raw)})
		default:
			toks = append(toks, token{kind: t.Kind, text: raw})
		}
		return bad == ""
	})
	return toks, bad
}

// readStatements are the statements a read-only call may run.
var readStatements = map[string]bool{
	"SELECT": true, "WITH": true, "SHOW": true, "DESCRIBE": true, "DESC": true, "EXPLAIN": true, "EXISTS": true,
}

// errNotRead is the reason given for a statement outside the allow-list.
var errNotRead = errors.New("only SELECT, WITH, SHOW, DESCRIBE, EXPLAIN and EXISTS statements are allowed on a read-only path")

func readOnlyError(reason error) error {
	return driver.Wrap(driver.ErrReadOnly, Name, "query", reason)
}

// checkReadOnly is the read-only path's allow-list. text is accepted when it
// holds exactly one statement (trailing ';' are fine) that starts with
// SELECT, WITH, SHOW, DESCRIBE, DESC, EXPLAIN or EXISTS, or with
// parentheses around a SELECT or WITH. Everything else is ErrReadOnly, and
// so is:
//
//   - text that cannot be lexed (an unterminated literal, identifier,
//     comment or heredoc, a NUL byte) or holds no statement;
//   - a second statement after a ';' (the server refuses several
//     statements anyway);
//   - INTO OUTFILE, which asks the client to write a file.
func checkReadOnly(text string) error {
	toks, bad := lex(text)
	if bad != "" {
		return readOnlyError(fmt.Errorf("%w: cannot check the statement: %s", errNotRead, bad))
	}
	end := len(toks)
	for i, t := range toks {
		if t.kind == sqlscan.Semicolon {
			end = i
			break
		}
	}
	for _, t := range toks[end:] {
		if t.kind != sqlscan.Semicolon {
			return readOnlyError(errors.New("only one statement is allowed on a read-only path"))
		}
	}
	toks = toks[:end]
	first := 0
	for first < len(toks) && toks[first].kind == sqlscan.Punct && toks[first].text == "(" {
		first++
	}
	if first == len(toks) || toks[first].kind != sqlscan.Word || !readStatements[toks[first].text] {
		return readOnlyError(errNotRead)
	}
	if first > 0 && toks[first].text != "SELECT" && toks[first].text != "WITH" {
		return readOnlyError(errNotRead)
	}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].kind == sqlscan.Word && toks[i].text == "INTO" && toks[i+1].kind == sqlscan.Word && toks[i+1].text == "OUTFILE" {
			return readOnlyError(errors.New("INTO OUTFILE is not allowed on a read-only path"))
		}
	}
	return nil
}

// checkRawFilter checks a Browse raw filter, a WHERE body that is pasted
// into the generated SELECT between parentheses: it must lex, keep its
// parentheses balanced (so it cannot close ours) and hold no ';'. A comment
// may close the filter's line: the generated text ends it with a newline.
func checkRawFilter(raw string) error {
	bad := func(reason string) error {
		return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid raw filter: "+reason))
	}
	toks, why := lex(raw)
	if why != "" {
		return bad(why)
	}
	depth := 0
	for _, t := range toks {
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

// quoteIdent quotes a database, table or column name with backticks,
// escaping backslashes and backticks the way ClickHouse reads them.
func quoteIdent(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "`" + strings.ReplaceAll(s, "`", "\\`") + "`"
}

// checkParams closes a gap in the allow-list on a read-only call with
// parameters. clickhouse-go writes the parameters into the text after the
// allow-list has judged it, and it substitutes placeholders inside $$…$$
// heredocs too, where a value holding the closing tag would end the string
// and turn the rest of the value into SQL. So, failing closed, it refuses a
// heredoc holding a placeholder (? or $N) and any value holding "$$".
func checkParams(text string, params []driver.Value) error {
	if len(params) == 0 {
		return nil
	}
	refusal := ""
	sqlscan.Scan(sqlscan.ClickHouse(), text, func(t sqlscan.Token) bool {
		raw := text[t.Start:t.End]
		if (t.Kind == sqlscan.String || t.Kind == sqlscan.Unterminated) && raw[0] == '$' && hasPlaceholder(raw) {
			refusal = "a parameter placeholder inside a $$…$$ heredoc"
		}
		return refusal == ""
	})
	for i := 0; refusal == "" && i < len(params); i++ {
		if valueHasHeredocTag(params[i]) {
			refusal = fmt.Sprintf("parameter %d holds \"$$\"", i+1)
		}
	}
	if refusal != "" {
		return readOnlyError(errors.New(refusal + " is not allowed on a read-only path"))
	}
	return nil
}

// hasPlaceholder reports whether s holds a ? or $N placeholder.
func hasPlaceholder(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '?' || s[i] == '$' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			return true
		}
	}
	return false
}

// valueHasHeredocTag reports whether a parameter's text holds "$$".
func valueHasHeredocTag(v driver.Value) bool {
	switch v.Type() {
	case driver.TypeList:
		list, _ := v.AsList()
		for _, e := range list {
			if valueHasHeredocTag(e) {
				return true
			}
		}
		return false
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return strings.Contains(string(raw), "$$")
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return strings.Contains(string(b), "$$")
	}
	s, _ := v.AsText()
	return strings.Contains(s, "$$")
}

// tableFunctionsAllowed are the table functions a read-only call may use
// when the driver cannot send readonly=1: built-ins that only generate rows
// from their arguments. Names compare case-insensitively and unqualified.
// system.* tables are tables, not functions, and need no entry.
var tableFunctionsAllowed = map[string]bool{
	"numbers": true, "numbers_mt": true, "zeros": true, "zeros_mt": true,
	"generaterandom": true, "values": true, "null": true,
}

// outsideFunctions are functions that reach outside the server, refused
// wherever they are called, not only in a table position: file() is a
// scalar function too. outsideFunction adds the families by name.
var outsideFunctions = map[string]bool{
	"url": true, "s3": true, "gcs": true, "oss": true, "cosn": true, "file": true, "remote": true,
	"remotesecure": true, "cluster": true, "mysql": true, "postgresql": true, "mongodb": true, "redis": true,
	"sqlite": true, "jdbc": true, "odbc": true, "hdfs": true, "azureblobstorage": true, "executable": true,
	"arrowflight": true, "ytsaurus": true, "clusterallreplicas": true, "hive": true,
}

// outsideFunction reports whether a function (lower-cased) reaches outside
// the server: outsideFunctions, any *cluster function, and the lakehouse
// families.
func outsideFunction(name string) bool {
	if outsideFunctions[name] || strings.HasSuffix(name, "cluster") {
		return true
	}
	for _, p := range []string{"iceberg", "deltalake", "hudi", "paimon"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// inFunctions are the IN operators (lower-cased): a function call as one of
// their right-hand arguments is read as a table position.
var inFunctions = map[string]bool{
	"in": true, "notin": true, "globalin": true, "globalnotin": true,
	"nullin": true, "notnullin": true, "globalnullin": true, "globalnotnullin": true,
}

// tableExpressionChildren are the nodes a TableExpression may hold in an
// EXPLAIN AST dump. Anything else is not understood, and refused.
var tableExpressionChildren = map[string]bool{
	"TableIdentifier": true, "Identifier": true, "Subquery": true, "Function": true, "SampleRatio": true,
}

// maxASTLines bounds the EXPLAIN AST output checkAST reads.
const maxASTLines = 100_000

// errASTUnread is the refusal for EXPLAIN AST output checkAST cannot read.
// It never quotes the output.
var errASTUnread = errors.New("cannot check the statement for table functions: " +
	"the server's EXPLAIN AST output was not understood")

// tableFunctionRefusal is the refusal for a table function off the
// allow-list. name is lower-cased and holds only [a-z0-9_.].
func tableFunctionRefusal(name string) error {
	return readOnlyError(fmt.Errorf("the table function %s() is not allowed on a read-only path: "+
		"the user's profile sets readonly, so tobira cannot add readonly=1, and allows only "+
		"numbers, numbers_mt, zeros, zeros_mt, generateRandom, values and null", name))
}

// checkASTText is the lexical step before EXPLAIN AST: the text must lex,
// and a quoted name must hold no control character and no backslash. The
// dump prints names and aliases raw, so a newline in one (typed, or made by
// an escape) could forge lines of the tree.
func checkASTText(text string) error {
	if _, bad := lex(text); bad != "" {
		return readOnlyError(fmt.Errorf("%w: cannot check the statement: %s", errNotRead, bad))
	}
	refusal := ""
	sqlscan.Scan(sqlscan.ClickHouse(), text, func(t sqlscan.Token) bool {
		if t.Kind != sqlscan.QuotedIdent {
			return true
		}
		for _, r := range text[t.Start:t.End] {
			if r < 0x20 || r == 0x7f || r == '\\' {
				refusal = "a quoted name holding a control character or a backslash is not allowed on a read-only path " +
					"when the user's profile sets readonly"
				return false
			}
		}
		return true
	})
	if refusal != "" {
		return readOnlyError(errors.New(refusal))
	}
	return nil
}

// astNode is one line of an EXPLAIN AST dump being walked.
type astNode struct {
	kind     string // the node type: SelectQuery, Function, TableExpression …
	name     string // a Function's name, lower-cased
	declared int    // its "(children N)"
	children int    // the children seen so far
	index    int    // its position among its parent's children
}

// checkAST walks the output of EXPLAIN AST (one node per line, indented one
// space per level) and refuses, as ErrReadOnly:
//
//   - a table function off tableFunctionsAllowed: a Function directly under
//     a TableExpression (FROM, JOIN, comma joins, DESCRIBE, at any depth:
//     CTEs, subqueries, UNION arms, EXPLAIN) or an InsertQuery (INSERT INTO
//     FUNCTION), or a right-hand argument of an IN operator other than
//     tuple() and array();
//   - an outsideFunction called anywhere (scalar file(), a url() alias …);
//   - output it cannot read as one well-formed tree, failing closed: an
//     empty dump, a control byte, more than one root, an indentation jump,
//     a "(children N)" that does not match the lines below it, a function
//     name outside [A-Za-z0-9_.], or an unknown TableExpression child.
//
// The refusal names the function, never the dump.
func checkAST(dump string) error {
	unread := readOnlyError(errASTUnread)
	dump = strings.TrimSuffix(dump, "\n")
	if dump == "" {
		return unread
	}
	lines := strings.Split(dump, "\n")
	if len(lines) > maxASTLines {
		return unread
	}
	var stack []*astNode
	// pop closes the nodes deeper than depth, checking their counts.
	pop := func(depth int) bool {
		for len(stack) > depth {
			n := stack[len(stack)-1]
			if n.children != n.declared {
				return false
			}
			stack = stack[:len(stack)-1]
		}
		return true
	}
	for i, line := range lines {
		for _, r := range line {
			if r < 0x20 || r == 0x7f {
				return unread
			}
		}
		body := strings.TrimLeft(line, " ")
		depth := len(line) - len(body)
		if depth > len(stack) || i > 0 && depth == 0 || body == "" || !isASCIILetter(body[0]) {
			return unread
		}
		if !pop(depth) {
			return unread
		}
		n := &astNode{}
		kind, rest, _ := strings.Cut(body, " ")
		for j := 0; j < len(kind); j++ {
			if !isASCIILetter(kind[j]) {
				return unread
			}
		}
		n.kind = kind
		if c, ok := strings.CutSuffix(rest, ")"); ok {
			if k := strings.LastIndex(c, "(children "); k >= 0 {
				num, err := strconv.Atoi(c[k+len("(children "):])
				if err != nil || num < 1 {
					return unread
				}
				n.declared, rest = num, strings.TrimRight(c[:k], " ")
			}
		}
		var parent, grand *astNode
		if depth > 0 {
			parent = stack[depth-1]
			n.index = parent.children
			parent.children++
			if depth > 1 {
				grand = stack[depth-2]
			}
		}
		if parent != nil && parent.kind == "TableExpression" && !tableExpressionChildren[kind] {
			return unread
		}
		if kind == "Function" {
			name, _, _ := strings.Cut(rest, " (alias ")
			if name == "" || !isFunctionName(name) {
				return unread
			}
			n.name = strings.ToLower(name)
			table := parent != nil && (parent.kind == "TableExpression" || parent.kind == "InsertQuery") ||
				parent != nil && parent.kind == "ExpressionList" && n.index > 0 &&
					grand != nil && grand.kind == "Function" && inFunctions[grand.name] &&
					n.name != "tuple" && n.name != "array"
			if table && !tableFunctionsAllowed[n.name] {
				return tableFunctionRefusal(n.name)
			}
			if outsideFunction(n.name) {
				return readOnlyError(fmt.Errorf("the function %s() reaches outside the server and is not allowed on a read-only path", n.name))
			}
		}
		stack = append(stack, n)
	}
	if !pop(0) {
		return unread
	}
	return nil
}

func isASCIILetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

// isFunctionName reports whether s holds only [A-Za-z0-9_.].
func isFunctionName(s string) bool {
	for i := 0; i < len(s); i++ {
		if b := s[i]; !isASCIILetter(b) && (b < '0' || b > '9') && b != '_' && b != '.' {
			return false
		}
	}
	return true
}

// checkTableFunctions is the read-only path's table-function guard when the
// user's profile sets readonly (see the note at the top): the text passes
// checkASTText, then the server parses it (EXPLAIN AST, with the same
// parameters, on the same pool and settings, so it sees the text the query
// would run) and checkAST judges the tree. It fails closed: an EXPLAIN the
// server refuses is ErrReadOnly too, naming only the server's error code,
// and the query is never sent. ctx ending is returned as the ctx error.
func (p pool) checkTableFunctions(ctx context.Context, op, text string, args []any) error {
	if err := checkASTText(text); err != nil {
		return err
	}
	dctx, done := detach(ctx)
	defer done()
	// The newline ends a line comment that opens the text.
	rows, err := p.db.Query(p.roCtx(dctx), "EXPLAIN AST\n"+text, args...)
	if err != nil {
		return astRefusal(ctx, op, err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	n := 0
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return astRefusal(ctx, op, err)
		}
		if n++; n > maxASTLines {
			return readOnlyError(errASTUnread)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		return astRefusal(ctx, op, err)
	}
	return checkAST(b.String())
}

// astRefusal turns a failed EXPLAIN AST into the guard's refusal: the ctx
// error when ctx ended, else ErrReadOnly naming the server's error code
// only (the server's message can quote the statement).
func astRefusal(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return mapError(ctx, op, err)
	}
	reason := "the server could not parse it"
	var ex *ch.Exception
	if errors.As(err, &ex) {
		reason = fmt.Sprintf("%s (code %d)", reason, ex.Code)
	}
	return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("cannot check the statement for table functions: "+reason))
}
