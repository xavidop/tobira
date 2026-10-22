package tui

import (
	"strings"

	"github.com/xavidop/tobira/driverutil/sqlcomplete"
	"github.com/xavidop/tobira/driverutil/sqlscan"
)

// jsonDialects are the driver dialects whose queries are JSON bodies (or,
// for weaviate, a JSON request or a GraphQL read, which nests the same way):
// they are split into top-level values, never at a semicolon.
var jsonDialects = map[string]bool{
	"elasticsearch": true, "opensearch": true, "firestore-json": true, "pinecone": true,
	"weaviate": true, "chroma": true, "milvus": true, "mongodb": true, "qdrant": true,
}

// splitStatements splits the editor text into the statements ctrl+enter
// picks from: top-level JSON values for the JSON drivers (and for a Cosmos
// DB query that is a JSON envelope), the dialect's SQL statements
// otherwise.
func splitStatements(dialect, text string) []sqlcomplete.Span {
	d := strings.ToLower(dialect)
	if jsonDialects[d] || (d == "cosmos-sql" && strings.HasPrefix(strings.TrimSpace(text), "{")) {
		return splitJSON(text)
	}
	return sqlcomplete.Split(splitDialect(d), text)
}

// splitDialect is the statement splitter's dialect for a driver dialect:
// only its lexical rules matter, so a semicolon inside a string, a quoted
// name or a comment never splits. A dialect it does not know gets SQLite's
// rules.
func splitDialect(dialect string) sqlcomplete.Dialect {
	lexical := func(name string, d sqlscan.Dialect) sqlcomplete.Dialect {
		return sqlcomplete.Dialect{Name: name, Lexical: &d}
	}
	switch d := strings.ToLower(dialect); d {
	case "postgresql", "postgres":
		return sqlcomplete.Postgres()
	case "mysql", "mariadb":
		return sqlcomplete.MySQL()
	case "clickhouse":
		return sqlcomplete.ClickHouse()
	case "oracle":
		// q'…' strings; a PL/SQL block keeps its semicolons and runs to a
		// line holding only "/" or the end of the text.
		return sqlcomplete.Dialect{Name: d, Oracle: true}
	case "googlesql":
		// "#" and "--" comments (with or without a space), "…" strings,
		// backslash escapes in strings and `…` names.
		s := sqlscan.MySQL(false, false)
		s.DashCommentSpace, s.ExecComments, s.IdentBackslashEscapes = false, false, true
		return lexical(d, s)
	case "tsql":
		return lexical(d, sqlscan.TSQL())
	case "cql":
		// $$…$$ strings.
		s := sqlscan.Generic()
		s.DollarQuotes = true
		return lexical(d, s)
	case "partiql":
		// `…` Ion literals.
		s := sqlscan.Generic()
		s.Backticks = true
		return lexical(d, s)
	case "snowflake":
		// Backslash escapes in strings and $$…$$ strings.
		s := sqlscan.Generic()
		s.BackslashEscapes, s.DollarQuotes = true, true
		return lexical(d, s)
	case "vertica":
		// E'…' and $$…$$ strings; comments do not nest.
		s := sqlscan.Postgres(true)
		s.NestedComments = false
		return lexical(d, s)
	case "duckdb", "cosmos-sql":
		// DuckDB reads PostgreSQL's lexical rules; Cosmos DB SQL only needs
		// '…' strings and "…" names, which they include.
		return lexical(d, sqlscan.Postgres(true))
	}
	return sqlcomplete.SQLite()
}

// splitJSON splits text into its top-level JSON values: a value opened by
// '{' or '[' ends at its matching close, outside "…" strings (with
// backslash escapes). Text outside brackets runs to the next value that
// opens after whitespace, or to the end. Each span is trimmed of
// whitespace; an unterminated value runs to the end of the text.
func splitJSON(text string) []sqlcomplete.Span {
	var out []sqlcomplete.Span
	i := 0
	for i < len(text) {
		for i < len(text) && isJSONSpace(text[i]) {
			i++
		}
		if i == len(text) {
			break
		}
		start, depth := i, 0
	value:
		for i < len(text) {
			switch c := text[i]; c {
			case '"':
				i = jsonStringEnd(text, i)
				continue
			case '{', '[':
				if depth == 0 && i > start && isJSONSpace(text[i-1]) {
					break value // a new value after loose text
				}
				depth++
			case '}', ']':
				if depth > 0 {
					depth--
					if depth == 0 {
						i++
						break value
					}
				}
			}
			i++
		}
		end := i
		for end > start && isJSONSpace(text[end-1]) {
			end--
		}
		out = append(out, sqlcomplete.Span{Start: start, End: end})
	}
	return out
}

// jsonStringEnd returns the offset just past the "…" string opened at
// text[i], or len(text) when it is unterminated.
func jsonStringEnd(text string, i int) int {
	for j := i + 1; j < len(text); j++ {
		switch text[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return len(text)
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
