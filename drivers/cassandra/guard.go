package cassandra

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Cassandra has no read-only session or transaction, so the read-only path
// is a lexical allow-list, checked before anything is sent. It fails
// closed: text the lexer cannot read to its end (lex.go) is refused, and so
// is anything but one SELECT or DESCRIBE statement. CQL's SELECT cannot
// write by itself; the only way it runs user code is a user-defined
// function or aggregate, so every call in it must be a built-in function.

// errNotRead is the refusal for a statement outside the allow-list.
var errNotRead = errors.New("only SELECT and DESCRIBE statements are allowed on a read-only path")

func readOnlyError(reason error) error {
	return driver.Wrap(driver.ErrReadOnly, Name, "query", reason)
}

// builtinFunctions are Cassandra 5.0's native scalar and aggregate
// functions, lower-cased; isBuiltin adds the blob conversions. A call to
// anything else may run a user-defined function, which is refused on a
// read-only path.
var builtinFunctions = map[string]bool{
	"token": true, "ttl": true, "writetime": true, "maxwritetime": true, "max_writetime": true, "write_time": true,
	"count": true, "min": true, "max": true, "sum": true, "avg": true, "cast": true,
	"now": true, "uuid": true, "currenttimestamp": true, "currentdate": true, "currenttime": true, "currenttimeuuid": true,
	"current_timestamp": true, "current_date": true, "current_time": true, "current_timeuuid": true,
	"mintimeuuid": true, "maxtimeuuid": true, "min_timeuuid": true, "max_timeuuid": true,
	"dateof": true, "unixtimestampof": true, "totimestamp": true, "todate": true, "tounixtimestamp": true,
	"to_timestamp": true, "to_date": true, "to_unix_timestamp": true,
	"fromjson": true, "tojson": true, "from_json": true, "to_json": true,
	"abs": true, "exp": true, "log": true, "log10": true, "round": true, "floor": true,
	"map_keys": true, "map_values": true, "collection_count": true, "collection_min": true, "collection_max": true,
	"collection_sum": true, "collection_avg": true,
	"mask_null": true, "mask_default": true, "mask_replace": true, "mask_inner": true, "mask_outer": true, "mask_hash": true,
	"similarity_cosine": true, "similarity_euclidean": true, "similarity_dot_product": true,
}

// blobTypes are the native types with blobAsX/XAsBlob conversions.
var blobTypes = map[string]bool{
	"ascii": true, "bigint": true, "boolean": true, "counter": true, "date": true, "decimal": true, "double": true,
	"duration": true, "float": true, "inet": true, "int": true, "smallint": true, "text": true, "time": true,
	"timestamp": true, "timeuuid": true, "tinyint": true, "uuid": true, "varchar": true, "varint": true,
}

// isBuiltin reports whether a lower-cased function name is a native
// function.
func isBuiltin(name string) bool {
	if builtinFunctions[name] {
		return true
	}
	for _, pre := range []string{"blobas", "blob_as_"} {
		if t, ok := strings.CutPrefix(name, pre); ok && blobTypes[t] {
			return true
		}
	}
	for _, suf := range []string{"asblob", "_as_blob"} {
		if t, ok := strings.CutSuffix(name, suf); ok && blobTypes[t] {
			return true
		}
	}
	return false
}

// parenKeywords are the words a '(' may follow without being a call.
// (CONTAINS (…) and CONTAINS KEY (…) take a parenthesised term.)
var parenKeywords = map[string]bool{
	"in": true, "and": true, "where": true, "select": true, "distinct": true, "json": true, "contains": true, "key": true,
}

// statement returns the tokens of text's single statement, without
// trailing semicolons, or the reason it is not one statement.
func statement(text string) ([]token, error) {
	toks, bad := lexCQL(text)
	if bad != "" {
		return nil, fmt.Errorf("%w: cannot check the statement: %s", errNotRead, bad)
	}
	end := len(toks)
	for i, t := range toks {
		if t.kind == tSemicolon {
			end = i
			break
		}
	}
	for _, t := range toks[end:] {
		if t.kind != tSemicolon {
			return nil, errors.New("only one statement is allowed on a read-only path")
		}
	}
	if end == 0 {
		return nil, fmt.Errorf("%w: the text holds no statement", errNotRead)
	}
	return toks[:end], nil
}

// checkReadOnly is the read-only path's allow-list: text must hold exactly
// one statement (trailing ';' are fine) that starts with SELECT, DESCRIBE
// or DESC, and every function a SELECT calls must be a built-in one,
// unqualified or in the system keyspace. Everything else is ErrReadOnly:
// INSERT, UPDATE, DELETE, BATCH, TRUNCATE, DDL, GRANT/REVOKE, role
// statements, USE, a user-defined function or aggregate (named, quoted or
// keyspace-qualified), several statements, and text the lexer refuses.
func checkReadOnly(text string) error {
	toks, err := statement(text)
	if err != nil {
		return readOnlyError(err)
	}
	if toks[0].kind != tWord {
		return readOnlyError(errNotRead)
	}
	switch toks[0].low {
	case "describe", "desc":
		return nil
	case "select":
	default:
		return readOnlyError(errNotRead)
	}
	return checkCalls(toks)
}

// checkCalls refuses a call to anything but a built-in function.
func checkCalls(toks []token) error {
	for i := 0; i+1 < len(toks); i++ {
		if toks[i+1].kind != tPunct || toks[i+1].text != "(" {
			continue
		}
		t := toks[i]
		switch t.kind {
		case tQuoted:
			return readOnlyError(fmt.Errorf("the function %s() is not a built-in function; user-defined functions are not allowed on a read-only path", t.text))
		case tWord:
		default:
			continue // a tuple, a type hint or a grouping
		}
		qualified := i >= 2 && toks[i-1].kind == tPunct && toks[i-1].text == "."
		switch {
		case qualified && (toks[i-2].kind != tWord || toks[i-2].low != "system"):
			return readOnlyError(fmt.Errorf("the function %s.%s() is not a built-in function; user-defined functions are not allowed on a read-only path", toks[i-2].text, t.text))
		case isBuiltin(t.low):
		case !qualified && parenKeywords[t.low]:
		default:
			return readOnlyError(fmt.Errorf("the function %s() is not a built-in function; user-defined functions are not allowed on a read-only path", t.text))
		}
	}
	return nil
}

// builtinCalls returns the unqualified calls of text to built-in function
// names, lower-cased, each once, in order. Cassandra resolves such a call
// among the native functions and the user functions of the statement's
// keyspace by argument types, so a UDF that shares the name could be the
// one that runs (see refuseShadowed). A system.-qualified call is always
// native and is not returned.
func builtinCalls(text string) []string {
	toks, bad := lexCQL(text)
	if bad != "" {
		return nil
	}
	var out []string
	for i := 0; i+1 < len(toks); i++ {
		t := toks[i]
		if t.kind != tWord || toks[i+1].kind != tPunct || toks[i+1].text != "(" || !isBuiltin(t.low) {
			continue
		}
		if i > 0 && toks[i-1].kind == tPunct && toks[i-1].text == "." {
			continue
		}
		if !slices.Contains(out, t.low) {
			out = append(out, t.low)
		}
	}
	return out
}

// refuseShadowed refuses an unqualified call to a built-in name that a
// user-defined function or aggregate also has (udfs: lower-cased names
// from every keyspace).
func refuseShadowed(calls []string, udfs map[string]bool) error {
	for _, name := range calls {
		if udfs[name] {
			return readOnlyError(fmt.Errorf("%s() may resolve to a user-defined function of the same name, which is not allowed on a read-only path: "+
				"call the built-in as system.%s()", name, name))
		}
	}
	return nil
}

// rawFilterStops are words that would end a WHERE body and start another
// clause of the generated SELECT.
var rawFilterStops = map[string]bool{"limit": true, "allow": true, "order": true, "group": true, "per": true}

// checkRawFilter checks a Browse raw filter, a WHERE body pasted into the
// generated SELECT after the conditions (followed by a newline, so a line
// comment may end it): it must lex, keep its parentheses balanced, hold no
// ';' and start no other clause (LIMIT, ALLOW FILTERING, ORDER BY, GROUP
// BY, PER PARTITION LIMIT), which the driver adds itself. On a read-only
// path the whole generated statement then goes through checkReadOnly.
func checkRawFilter(raw string) error {
	bad := func(reason string) error {
		return driver.Wrap(driver.ErrSyntax, Name, "browse", errors.New("invalid raw filter: "+reason))
	}
	toks, why := lexCQL(raw)
	if why != "" {
		return bad(why)
	}
	depth := 0
	for _, t := range toks {
		switch {
		case t.kind == tSemicolon:
			return bad("';' is not allowed")
		case t.kind == tWord && rawFilterStops[t.low]:
			return bad(strings.ToUpper(t.text) + " is not allowed: the filter is a WHERE body")
		case t.kind != tPunct:
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

// quoteIdent quotes a keyspace, table or column name, doubling quotes.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
