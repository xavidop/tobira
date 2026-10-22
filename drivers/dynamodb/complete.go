package dynamodb

import (
	"context"
	"regexp"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// maxCompletions caps one completion list.
const maxCompletions = 200

var (
	partiqlKeywords = []string{
		"SELECT", "FROM", "WHERE", "AND", "OR", "NOT", "IN", "IS", "NULL", "MISSING", "BETWEEN",
		"ORDER", "BY", "ASC", "DESC", "INSERT", "INTO", "VALUE", "UPDATE", "SET", "REMOVE",
		"DELETE", "RETURNING", "EXISTS", "TRUE", "FALSE",
	}
	partiqlFunctions = []string{
		"begins_with", "contains", "attribute_type", "size", "exists", "missing",
		"list_append", "set_add", "set_delete",
	}
	plainName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

var _ driver.Completer = (*conn)(nil)

// Complete suggests PartiQL keywords, functions, table names (cached 60 s)
// and the attribute names Describe finds in the statement's table, for the
// word at the cursor. After FROM, INTO or UPDATE it suggests tables only.
func (c *conn) Complete(ctx context.Context, req driver.CompletionRequest) ([]driver.Completion, error) {
	if err := c.check(ctx, "complete"); err != nil {
		return nil, err
	}
	tables, err := c.catalog.tableNames(ctx)
	if err != nil {
		return nil, err
	}
	attrs := func(table string) []string { return c.catalog.attributes(ctx, table) }
	return completionsFor(req, tables, attrs), nil
}

// completionsFor computes completions without I/O beyond attrs.
func completionsFor(req driver.CompletionRequest, tables []string, attrs func(string) []string) []driver.Completion {
	text := req.Text
	if req.Cursor < 0 || req.Cursor > len(text) {
		return nil
	}
	start := req.Cursor
	for start > 0 && isWordPart(text[start-1]) {
		start--
	}
	prefix := strings.ToLower(text[start:req.Cursor])
	quoted := start > 0 && text[start-1] == '"'
	before := text[:start]
	if quoted {
		before = text[:start-1]
	}
	tablesOnly := false
	if toks, err := lex(before); err == nil && len(toks) > 0 {
		last := toks[len(toks)-1]
		tablesOnly = last.isWord("FROM") || last.isWord("INTO") || last.isWord("UPDATE")
	}
	var out []driver.Completion
	add := func(label, kind, insert string) {
		if len(out) < maxCompletions && strings.HasPrefix(strings.ToLower(label), prefix) {
			out = append(out, driver.Completion{Label: label, Kind: kind, Insert: insert})
		}
	}
	for _, t := range tables {
		insert := ""
		if !quoted && !plainName.MatchString(t) {
			insert = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		}
		add(t, "collection", insert)
	}
	if tablesOnly || quoted {
		return out
	}
	if table := statementTable(text, before); table != "" {
		for _, a := range attrs(table) {
			insert := ""
			if !plainName.MatchString(a) || refusedWords[strings.ToUpper(a)] {
				insert = `"` + strings.ReplaceAll(a, `"`, `""`) + `"`
			}
			add(a, "field", insert)
		}
	}
	for _, f := range partiqlFunctions {
		add(f, "function", "")
	}
	for _, k := range partiqlKeywords {
		add(k, "keyword", "")
	}
	return out
}

// statementTable finds the table a statement names after FROM, INTO or
// UPDATE, in the whole text or, if that does not lex, the text before the
// cursor's word.
func statementTable(text, before string) string {
	toks, err := lex(text)
	if err != nil {
		if toks, err = lex(before); err != nil {
			return ""
		}
	}
	for i := 0; i+1 < len(toks); i++ {
		t := toks[i]
		if t.isWord("FROM") || t.isWord("INTO") || t.isWord("UPDATE") {
			if n := toks[i+1]; n.kind == tokWord || n.kind == tokQuotedIdent {
				return n.text
			}
		}
	}
	return ""
}
