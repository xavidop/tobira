package postgres

import (
	"fmt"

	"github.com/xavidop/tobira/driverutil/sqlscan"

	"github.com/xavidop/tobira/driver"
)

// The read-only path of CockroachDB and Redshift.
//
// On PostgreSQL and Greenplum, BEGIN READ ONLY plus the refusals of rofile.go
// guard the read-only path. CockroachDB and Redshift honour BEGIN READ ONLY
// too, but each has statements that reach outside the database without
// writing to it, so a read-only transaction does not stop them: Redshift's
// UNLOAD writes query results to S3, and CockroachDB's EXPORT, BACKUP and
// CREATE CHANGEFEED write to external storage, while crdb_internal builtins
// such as force_error, reset_sql_stats or unsafe_upsert_descriptor act on
// the cluster through internal transactions of their own. Neither flavor has
// had its statements checked one by one the way PostgreSQL's have, so their
// read-only path is an allow-list (fail-closed):
//
//   - the first keyword must be SELECT, VALUES, TABLE, WITH, SHOW or "(",
//     or EXPLAIN [ANALYZE] [VERBOSE] [(options)] followed by one of those,
//     where the options are only VERBOSE, TYPES, OPT, VEC, DISTSQL, GIST and
//     REDACT (EXPLAIN ANALYZE (DEBUG) writes a statement bundle);
//   - SHOW BACKUP(S), and any SHOW in text with a string literal (a storage
//     URI), are refused wherever they appear, including bracketed [SHOW …]
//     sources at any depth: they read external storage;
//   - SELECT … INTO (a new table) is refused, without relying on the
//     server's read-only transaction;
//   - no crdb_internal function may be called (crdb_internal tables and
//     views are not refused here).
//
// Everything else is refused with ErrReadOnly before it reaches the server.
// Every lexing of guardDialects must agree.

// flavorReadKeywords are the first keywords the allow-list accepts.
var flavorReadKeywords = map[string]bool{"SELECT": true, "VALUES": true, "TABLE": true, "WITH": true, "SHOW": true, "(": true}

// explainOptions are the bare EXPLAIN options that may precede the statement.
var explainOptions = map[string]bool{"ANALYZE": true, "ANALYSE": true, "VERBOSE": true}

// flavorRefusal returns the ErrReadOnly refusal of text on the read-only
// path of f, or nil. Flavors without an allow-list (PostgreSQL, Greenplum)
// always return nil: rofile.go guards them.
func flavorRefusal(f flavor, op, text string) error {
	if f != flavorCockroach && f != flavorRedshift {
		return nil
	}
	refuse := func(err error) error { return driver.Wrap(driver.ErrReadOnly, Name, op, err) }
	kw := leadingKeyword(text)
	if !flavorReadKeywords[kw] && kw != "EXPLAIN" {
		if kw == "" {
			return refuse(fmt.Errorf("an empty statement is not allowed on a read-only %s path", f))
		}
		return refuse(fmt.Errorf("%s is not allowed on a read-only %s path (only SELECT, WITH, VALUES, TABLE, SHOW and EXPLAIN)", kw, f))
	}
	for _, d := range guardDialects() {
		words := scanWords(text, true, true, d)
		if _, ok := readStatement(words); !ok {
			return refuse(fmt.Errorf("only SELECT, WITH, VALUES, TABLE, SHOW and EXPLAIN of those (options %s) are allowed on a read-only %s path", "VERBOSE, TYPES, OPT, VEC, DISTSQL, GIST, REDACT", f))
		}
		if externalShow(text, d) {
			// SHOW BACKUP … IN '<uri>' and the like read external storage,
			// as a statement or as a [SHOW …] source at any depth.
			return refuse(fmt.Errorf("SHOW BACKUP and SHOW with a quoted location are not allowed on a read-only %s path", f))
		}
		if fn, ok := crdbInternalCall(words); ok {
			return refuse(fmt.Errorf("crdb_internal.%s(…) is not allowed on a read-only %s path", fn, f))
		}
	}
	if hasTopLevelInto(text) {
		return refuse(fmt.Errorf("SELECT INTO creates a table and is not allowed on a read-only %s path", f))
	}
	return nil
}

// readStatement returns the index in words (punctuation included) of the
// first keyword of the statement, looking through EXPLAIN, its bare options
// and its option blocks, and whether that statement is on the allow-list.
// Every "(" after EXPLAIN and its bare options is an option block, which
// may hold only explainBlockOptions and commas: EXPLAIN ANALYZE (DEBUG)
// writes a diagnostics bundle on CockroachDB, so unknown options are
// refused, and a parenthesised statement after EXPLAIN is refused with them.
func readStatement(words []sqlWord) (int, bool) {
	i := 0
	for i < len(words) && words[i].text == ";" {
		i++
	}
	if i < len(words) && words[i].text == "EXPLAIN" {
		i++
		for i < len(words) {
			switch {
			case explainOptions[words[i].text]:
				i++
				continue
			case words[i].text == "(":
				end, ok := explainBlock(words, i)
				if !ok {
					return i, false
				}
				i = end
				continue
			}
			break
		}
		if i < len(words) && words[i].text == "(" {
			return i, false
		}
	}
	return i, i < len(words) && flavorReadKeywords[words[i].text]
}

// explainBlockOptions are the EXPLAIN (…) options the allow-list accepts.
var explainBlockOptions = map[string]bool{
	"VERBOSE": true, "TYPES": true, "OPT": true, "VEC": true, "DISTSQL": true, "GIST": true, "REDACT": true,
}

// explainBlock checks the option block opening at words[i] ("(") and
// returns the index after its ")". Anything but explainBlockOptions and
// commas, or a missing ")", fails.
func explainBlock(words []sqlWord, i int) (int, bool) {
	for j := i + 1; j < len(words); j++ {
		switch w := words[j].text; {
		case w == ")":
			return j + 1, j > i+1
		case w == ",", explainBlockOptions[w]:
		default:
			return j, false
		}
	}
	return len(words), false
}

// externalShow reports whether text holds, anywhere, a bare SHOW keyword
// followed by BACKUP or BACKUPS, or a bare SHOW in text that holds a string
// literal. That covers a SHOW statement and CockroachDB's bracketed
// [SHOW …] data sources at any depth, inside WITH or subqueries. The string
// check is on the whole text, so a quoted literal anywhere next to a SHOW
// source is refused too (fail-closed). Quoted identifiers ("show") do not
// count as the keyword.
func externalShow(text string, d sqlscan.Dialect) bool {
	words := scanWords(text, false, false, d)
	for i, w := range words {
		if w.text != "SHOW" {
			continue
		}
		if i+1 < len(words) && (words[i+1].text == "BACKUP" || words[i+1].text == "BACKUPS") {
			return true
		}
		if hasStringLiteral(text, d) {
			return true
		}
	}
	return false
}

// hasStringLiteral reports whether text holds a string literal (or an
// unterminated one) under dialect d.
func hasStringLiteral(text string, d sqlscan.Dialect) bool {
	found := false
	sqlscan.Scan(d, text, func(t sqlscan.Token) bool {
		if t.Kind == sqlscan.String || t.Kind == sqlscan.Unterminated && text[t.Start] != '/' && text[t.Start] != '"' {
			found = true
		}
		return !found
	})
	return found
}

// crdbInternalCall returns the name of the first crdb_internal.<name>( call
// in words, quoted spellings included, and whether there is one.
func crdbInternalCall(words []sqlWord) (string, bool) {
	for i := 0; i+3 < len(words); i++ {
		if words[i].text == "CRDB_INTERNAL" && words[i+1].text == "." && words[i+3].text == "(" {
			return words[i+2].text, true
		}
	}
	return "", false
}
