package duckdb

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// The read-only guard is layered. This file is its lexical layer, which
// runs before anything reaches DuckDB: an allow-list of leading words, a
// deny-list of words anywhere, a deny-list of functions that read files,
// URLs or run SQL, and a check of every table position so a string or a
// path-like quoted name never stands in for a table (DuckDB's replacement
// scans read 'file.csv' as a table). Text it cannot lex is refused.
//
// Behind it, on every read-only path (query.go): the statement runs in a
// READ ONLY transaction and only the parser's SELECT and EXPLAIN statement
// types run. Every instance starts with enable_external_access=false,
// extension auto-install and auto-load off and lock_configuration=true, and
// a read-only conn's own instance also with access_mode=READ_ONLY
// (open.go), so the engine stops file and URL reads whatever this layer
// misses.

// dialect is DuckDB's lexer as far as the guard needs it: PostgreSQL's
// scanner (nested comments, E'…' strings, $tag$ strings, "…" identifiers,
// non-ASCII letters in words). Numbers are digits only, so 1delete lexes as
// 1 then the word DELETE, and checkWords refuses any other word glued to a
// number (1e5delete), whatever DuckDB would make of it.
var dialect = sqlscan.Postgres(true)

// leadWords may start a read-only statement.
var leadWords = map[string]bool{
	"SELECT": true, "WITH": true, "VALUES": true, "FROM": true, "TABLE": true,
	"SHOW": true, "DESCRIBE": true, "DESC": true, "SUMMARIZE": true,
	"PIVOT": true, "UNPIVOT": true, "EXPLAIN": true, "PRAGMA": true,
}

// deniedWords are refused anywhere in read-only text, as bare words. Quote
// an identifier with one of these names to use it.
var deniedWords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "UPSERT": true, "TRUNCATE": true,
	"CREATE": true, "DROP": true, "ALTER": true, "COMMENT": true, "RETURNING": true, "INTO": true,
	"COPY": true, "ATTACH": true, "DETACH": true, "USE": true, "EXPORT": true, "IMPORT": true,
	"INSTALL": true, "LOAD": true, "FORCE": true, "SET": true, "RESET": true, "PRAGMA": true,
	"CALL": true, "CHECKPOINT": true, "VACUUM": true, "ANALYZE": true, "ANALYSE": true,
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true, "ABORT": true, "START": true, "TRANSACTION": true,
	"PREPARE": true, "EXECUTE": true, "DEALLOCATE": true, "GRANT": true, "REVOKE": true,
	"SECRET": true, "MACRO": true,
}

// deniedFunctions are functions refused when called anywhere: they read
// files or URLs, run SQL text, change a sequence or reveal secrets.
var deniedFunctions = map[string]bool{
	"glob": true, "query": true, "query_table": true, "sniff_csv": true, "csv_scan": true,
	"json_execute_serialized_sql": true, "nextval": true, "setval": true, "getenv": true,
	"duckdb_secrets": true, "which_secret": true, "load_aws_credentials": true,
	"checkpoint": true, "force_checkpoint": true,
}

// deniedPrefixes extend deniedFunctions to families of functions.
var deniedPrefixes = []string{
	"read_", "parquet_", "sqlite_", "postgres_", "mysql_", "iceberg_", "delta_", "st_read",
	"json_execute", "ducklake_", "http", "s3_", "azure_", "gcs_", "hf_", "excel_",
}

// tableFunctions are the table functions allowed in a table position: the
// generators and DuckDB's own catalog functions. Anything else called there
// (a table macro, an extension's scanner) is refused, since it may read
// files or run SQL.
var tableFunctions = map[string]bool{
	"range": true, "generate_series": true, "unnest": true, "repeat": true, "repeat_row": true,
	"json_each": true, "json_tree": true, "sql_auto_complete": true, "test_all_types": true,
	"pragma_table_info": true, "pragma_show": true, "pragma_storage_info": true,
	"pragma_database_size": true, "pragma_version": true, "pragma_database_list": true,
	"pragma_collations": true, "pragma_platform": true, "pragma_user_agent": true,
	"pragma_metadata_info": true, "pragma_functions": true,
}

// pragmas are the PRAGMA statements a read-only connection may run: each
// only reads.
var pragmas = map[string]bool{
	"version": true, "database_list": true, "show_tables": true, "show_tables_expanded": true,
	"show_databases": true, "table_info": true, "show": true, "database_size": true,
	"storage_info": true, "platform": true, "user_agent": true, "metadata_info": true,
	"collations": true, "functions": true,
}

// clauseEnds end a FROM clause at the same parenthesis depth.
var clauseEnds = map[string]bool{
	"WHERE": true, "GROUP": true, "HAVING": true, "QUALIFY": true, "WINDOW": true, "ORDER": true,
	"LIMIT": true, "OFFSET": true, "UNION": true, "INTERSECT": true, "EXCEPT": true, "SELECT": true,
	"VALUES": true,
}

// token is one significant token: its kind and text.
type token struct {
	kind       sqlscan.Kind
	text       string
	start, end int
}

func (t token) word() string {
	if t.kind != sqlscan.Word {
		return ""
	}
	return strings.ToUpper(t.text)
}

func (t token) is(p string) bool { return t.kind == sqlscan.Punct && t.text == p }

// name is the lower-cased name of a word or quoted identifier. DuckDB
// matches names case-insensitively, quoted or not.
func (t token) name() string {
	switch t.kind {
	case sqlscan.Word:
		return strings.ToLower(t.text)
	case sqlscan.QuotedIdent:
		return strings.ToLower(strings.ReplaceAll(t.text[1:len(t.text)-1], `""`, `"`))
	}
	return ""
}

// lex returns text's significant tokens (no spaces or comments). It fails
// on text it cannot classify: invalid UTF-8, a NUL byte or an unterminated
// string, identifier or comment.
func lex(text string) ([]token, error) {
	if !utf8.ValidString(text) {
		return nil, errors.New("the text is not valid UTF-8")
	}
	if strings.IndexByte(text, 0) >= 0 {
		return nil, errors.New("the text holds a NUL byte")
	}
	var toks []token
	var bad error
	sqlscan.Scan(dialect, text, func(tk sqlscan.Token) bool {
		switch tk.Kind {
		case sqlscan.Space, sqlscan.Comment:
		case sqlscan.Unterminated:
			bad = errors.New("the text ends inside a string, quoted name or comment")
			return false
		case sqlscan.ExecComment:
			bad = errors.New("the text holds an executable comment")
			return false
		default:
			toks = append(toks, token{tk.Kind, text[tk.Start:tk.End], tk.Start, tk.End})
		}
		return true
	})
	return toks, bad
}

// statement lexes text as one statement: a trailing semicolon is dropped,
// any other is an error, and so is empty text.
func statement(text string) ([]token, error) {
	toks, err := lex(text)
	if err != nil {
		return nil, err
	}
	for len(toks) > 0 && toks[len(toks)-1].kind == sqlscan.Semicolon {
		toks = toks[:len(toks)-1]
		if len(toks) > 0 && toks[len(toks)-1].kind == sqlscan.Semicolon {
			return nil, errMultiple
		}
	}
	if len(toks) == 0 {
		return nil, errors.New("the text holds no statement")
	}
	for _, t := range toks {
		if t.kind == sqlscan.Semicolon {
			return nil, errMultiple
		}
	}
	return toks, nil
}

var errMultiple = errors.New("only one statement per call is allowed")

func refuse(reason string) error {
	return driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New("read-only: "+reason))
}

// checkReadOnly is the lexical layer of the read-only guard: nil only for
// one statement that can only read. Every refusal is ErrReadOnly.
func checkReadOnly(text string) error {
	toks, err := statement(text)
	if err != nil {
		return refuse(err.Error())
	}
	return checkTokens(toks)
}

func checkTokens(toks []token) error {
	i := 0
	for i < len(toks) && toks[i].is("(") {
		i++
	}
	if i == len(toks) {
		return refuse("the statement cannot be classified")
	}
	lead := toks[i].word()
	if !leadWords[lead] {
		return refuse("only SELECT, WITH, VALUES, FROM, TABLE, SHOW, DESCRIBE, SUMMARIZE, PIVOT, EXPLAIN and read-only PRAGMA statements are allowed")
	}
	switch lead {
	case "EXPLAIN":
		if i > 0 || len(toks) < 2 {
			return refuse("EXPLAIN must lead and explain a statement")
		}
		if toks[1].is("(") {
			return refuse("EXPLAIN options are not allowed")
		}
		return checkTokens(toks[1:])
	case "PRAGMA":
		if i > 0 {
			return refuse("PRAGMA must lead the statement")
		}
		return checkPragma(toks[1:])
	}
	if i > 0 && lead != "SELECT" && lead != "WITH" && lead != "VALUES" && lead != "FROM" && lead != "TABLE" {
		return refuse("only a query may be parenthesized")
	}
	if err := checkWords(toks); err != nil {
		return err
	}
	return checkTables(toks, i)
}

// checkPragma allows a read-only PRAGMA: a listed name, optionally with
// parenthesized names, strings and numbers, and no assignment.
func checkPragma(rest []token) error {
	if len(rest) == 0 || !pragmas[rest[0].name()] || rest[0].kind != sqlscan.Word {
		return refuse("only read-only PRAGMAs (version, table_info, show_tables, database_size …) are allowed")
	}
	args := rest[1:]
	if len(args) == 0 {
		return nil
	}
	if !args[0].is("(") || !args[len(args)-1].is(")") {
		return refuse("PRAGMA arguments must be parenthesized")
	}
	for _, t := range args[1 : len(args)-1] {
		switch {
		case t.kind == sqlscan.String, t.kind == sqlscan.QuotedIdent, t.kind == sqlscan.Number,
			t.kind == sqlscan.Word && !deniedWords[t.word()], t.is("."), t.is(","):
		default:
			return refuse("PRAGMA arguments may only be names, strings and numbers")
		}
	}
	return nil
}

// numberTail matches the word a number may run into: an exponent (1e5,
// 1e as in 1e-5), digit separators (1_000) or what DuckDB reads as an alias
// after 0 (0x1F, 0b101).
var numberTail = regexp.MustCompile(`^(?i:e[0-9]*|_[0-9_]*|x[0-9a-f_]*|b[01_]*)$`)

// checkWords applies the word and function deny-lists to every token, and
// refuses a word glued to a number unless it is a number's own tail, so
// that 1e5delete or 1returning cannot hide a keyword from the deny-list.
func checkWords(toks []token) error {
	for j, t := range toks {
		if w := t.word(); deniedWords[w] {
			return refuse(w + " is not allowed")
		}
		if j > 0 && t.kind == sqlscan.Word && toks[j-1].kind == sqlscan.Number && toks[j-1].end == t.start && !numberTail.MatchString(t.text) {
			return refuse("a number runs into the word " + t.text)
		}
		if t.is("&") && j > 0 && strings.EqualFold(toks[j-1].text, "u") && toks[j-1].end == t.start {
			return refuse("U& escapes are not allowed")
		}
		if j+1 < len(toks) && toks[j+1].is("(") && deniedFunction(t.name()) {
			return refuse("the function " + t.name() + " is not allowed: it reads files or URLs, runs SQL or changes state")
		}
	}
	return nil
}

func deniedFunction(name string) bool {
	if name == "" {
		return false
	}
	if deniedFunctions[name] {
		return true
	}
	for _, p := range deniedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// allowedTableFunction reports whether name may be called in a table
// position.
func allowedTableFunction(name string) bool {
	if tableFunctions[name] {
		return true
	}
	return strings.HasPrefix(name, "duckdb_") && !deniedFunction(name)
}

// tableWords are followed by a table reference wherever they appear (a
// subquery, a set operation or a join, not only as the lead word): DuckDB
// reads a string or a path-like quoted name there as a file to scan.
var tableWords = map[string]bool{
	"FROM": true, "JOIN": true, "LATERAL": true, "ONLY": true, "TABLE": true, "SHOW": true,
	"DESCRIBE": true, "SUMMARIZE": true, "PIVOT": true, "UNPIVOT": true,
}

// checkTables finds every table position and judges the token there:
//   - after a tableWords word, or a comma of a FROM list (tracked per
//     parenthesis depth), checkTableRef refuses a string, a path-like
//     quoted name and a table function not on the allow-list;
//   - after DESC (DESCRIBE's alias, but also ORDER BY's), or after a "("
//     that itself stands in a table position (FROM (…), JOIN ((…))), a
//     string or a path-like quoted name is refused.
//
// The engine stops a file read on every instance anyway (open.go); this is
// defence in depth.
func checkTables(toks []token, lead int) error {
	clauses := []string{""} // the clause at each parenthesis depth
	full, str := false, false
	for j := lead; j < len(toks); j++ {
		t := toks[j]
		switch {
		case full:
			if err := checkTableRef(toks, j); err != nil {
				return err
			}
		case str:
			if err := checkStringRef(t, j+1 < len(toks) && toks[j+1].is(".")); err != nil {
				return err
			}
		}
		inTable := full || str
		full, str = false, false
		depth := len(clauses) - 1
		switch w := t.word(); {
		case t.is("("):
			clauses = append(clauses, "")
			str = inTable
		case t.is(")"):
			if depth > 0 {
				clauses = clauses[:depth]
			}
		case t.is(","):
			full = clauses[depth] == "FROM"
		case w == "FROM":
			clauses[depth] = "FROM"
			full = true
		case tableWords[w]:
			full = true
		case w == "DESC":
			str = true
		case clauseEnds[w]:
			clauses[depth] = ""
		}
	}
	return nil
}

// checkStringRef refuses a string, or an unqualified path-like quoted
// name, where DuckDB may read it as a file to scan.
func checkStringRef(t token, qualified bool) error {
	switch {
	case t.kind == sqlscan.String:
		return refuse("a string in place of a table reads a file or URL")
	case t.kind == sqlscan.QuotedIdent && !qualified && strings.ContainsAny(t.name(), `./\:*?~`):
		return refuse("a path-like quoted name in place of a table reads a file or URL")
	}
	return nil
}

// checkTableRef judges the token at a table position.
func checkTableRef(toks []token, j int) error {
	t := toks[j]
	if err := checkStringRef(t, j+1 < len(toks) && toks[j+1].is(".")); err != nil {
		return err
	}
	if t.kind != sqlscan.Word && t.kind != sqlscan.QuotedIdent {
		return nil
	}
	// A qualified name: name (. name)*, then a call or not.
	k := j
	for k+2 < len(toks) && toks[k+1].is(".") && (toks[k+2].kind == sqlscan.Word || toks[k+2].kind == sqlscan.QuotedIdent) {
		k += 2
	}
	if k+1 < len(toks) && toks[k+1].is("(") && t.word() != "LATERAL" {
		if name := toks[k].name(); !allowedTableFunction(name) {
			return refuse("the table function " + name + " is not allowed on a read-only path")
		}
	}
	return nil
}

// checkSingle refuses anything but exactly one statement, for the
// read-write paths: duckdb-go would run every statement of a text.
func checkSingle(text string) error {
	if _, err := statement(text); err != nil {
		kind := driver.ErrSyntax
		if errors.Is(err, errMultiple) {
			kind = driver.ErrUnsupported
		}
		return driver.Wrap(kind, Name, "query", err)
	}
	return nil
}

// txWords lead a transaction-control statement.
var txWords = map[string]bool{"BEGIN": true, "COMMIT": true, "ROLLBACK": true, "ABORT": true, "END": true, "START": true}

// isTxControl reports whether text leads with a transaction-control word.
// The parser's statement type is checked as well before anything runs.
func isTxControl(text string) bool {
	toks, err := lex(text)
	if err != nil || len(toks) == 0 {
		return false
	}
	return txWords[toks[0].word()]
}

// hasReturning reports whether text holds a RETURNING clause, so a write
// returns rows.
func hasReturning(text string) bool {
	toks, _ := lex(text)
	for _, t := range toks {
		if t.word() == "RETURNING" {
			return true
		}
	}
	return false
}
