package sqlitecore

import (
	"regexp"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// MarkRowidAlias sets AutoIncrement on a rowid alias: the single primary key
// column of a rowid table declared exactly INTEGER (with or without
// AUTOINCREMENT), which SQLite fills when an insert leaves it out. A
// column-level "INTEGER PRIMARY KEY DESC" is not an alias.
func MarkRowidAlias(s *driver.CollectionSchema) {
	if len(s.PrimaryKey) != 1 || s.Type != driver.CollectionTable || withoutRowid.MatchString(s.DDL) {
		return
	}
	for i := range s.Fields {
		f := &s.Fields[i]
		if f.Name == s.PrimaryKey[0] && strings.EqualFold(strings.TrimSpace(f.NativeType), "INTEGER") && !columnPKDesc(s.DDL, f.Name) {
			f.AutoIncrement = true
		}
	}
}

// columnPKDesc reports a column declared "<name> INTEGER <constraints>
// PRIMARY KEY DESC" (NOT NULL or CONSTRAINT pk may come first): SQLite's
// documented exception, which is not a rowid alias. A table-level
// PRIMARY KEY (name DESC) still is one.
func columnPKDesc(ddl, name string) bool {
	for _, def := range columnDefs(ddlTokens(ddl)) {
		if len(def) == 0 || !def[0].quoted && tableConstraint[def[0].text] || def[0].text != asciiUpper(name) {
			continue
		}
		for i := 1; i+2 < len(def); i++ {
			if def[i].is("PRIMARY") && def[i+1].is("KEY") && def[i+2].is("DESC") {
				return true
			}
		}
	}
	return false
}

// tableConstraint are the keywords that start a table constraint rather than
// a column definition in CREATE TABLE.
var tableConstraint = map[string]bool{"CONSTRAINT": true, "PRIMARY": true, "UNIQUE": true, "CHECK": true, "FOREIGN": true}

// ddlTok is one token of a CREATE statement: a word or quoted identifier
// (upper-cased, unquoted), a string literal ("'"), or one punctuation byte.
type ddlTok struct {
	text   string
	quoted bool
	depth  int // parenthesis depth; "(" and ")" carry the outer depth
}

func (t ddlTok) is(kw string) bool { return !t.quoted && t.text == kw }

// ddlTokens splits ddl into tokens, skipping whitespace and comments (see
// skipSpace for the other scanners).
func ddlTokens(ddl string) []ddlTok {
	var out []ddlTok
	depth := 0
	for i := skipSpace(ddl, 0); i < len(ddl); i = skipSpace(ddl, i) {
		switch c := ddl[i]; c {
		case '(':
			out = append(out, ddlTok{text: "(", depth: depth})
			depth++
			i++
		case ')':
			depth = max(depth-1, 0)
			out = append(out, ddlTok{text: ")", depth: depth})
			i++
		case '"', '`', '[', '\'':
			end := c
			if c == '[' {
				end = ']'
			}
			var b strings.Builder
			j := i + 1
			for ; j < len(ddl); j++ {
				if ddl[j] == end {
					if end != ']' && j+1 < len(ddl) && ddl[j+1] == end {
						b.WriteByte(end)
						j++
						continue
					}
					break
				}
				b.WriteByte(ddl[j])
			}
			i = j + 1
			if c == '\'' {
				out = append(out, ddlTok{text: "'", depth: depth})
			} else {
				out = append(out, ddlTok{text: asciiUpper(b.String()), quoted: true, depth: depth})
			}
		default:
			w, j := word(ddl, i)
			if j == i {
				w, j = ddl[i:i+1], i+1
			}
			out = append(out, ddlTok{text: w, depth: depth})
			i = j
		}
	}
	return out
}

// columnDefs returns the depth-1 tokens of each comma-separated definition
// inside the first parenthesised list (the column and constraint list of a
// CREATE TABLE).
func columnDefs(toks []ddlTok) [][]ddlTok {
	var defs [][]ddlTok
	var cur []ddlTok
	in := false
	for _, t := range toks {
		switch {
		case !in:
			in = t.text == "(" && !t.quoted && t.depth == 0
		case t.depth == 0 && t.text == ")" && !t.quoted:
			return append(defs, cur)
		case t.depth == 1 && t.text == "," && !t.quoted:
			defs, cur = append(defs, cur), nil
		case t.depth == 1:
			cur = append(cur, t)
		}
	}
	return append(defs, cur)
}

var withoutRowid = regexp.MustCompile(`(?i)\bwithout\s+rowid\b`)

// QuoteIdent quotes an identifier by doubling embedded double quotes.
func QuoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// NamespaceName is the schema name a namespace ref means: "" is "main".
func NamespaceName(n string) string {
	if n == "" {
		return "main"
	}
	return n
}
