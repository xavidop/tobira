package bigquery

import (
	"errors"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// deniedWords are refused anywhere outside strings and quoted names in a
// read-only statement. None can appear in one SELECT; the allow-list below
// already refuses a statement that starts with one, so this is a second
// net. A column with one of these names must be written in backticks.
var deniedWords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "TRUNCATE": true,
	"CREATE": true, "DROP": true, "ALTER": true, "UNDROP": true, "EXPORT": true, "LOAD": true,
	"DECLARE": true, "SET": true, "EXECUTE": true, "CALL": true, "BEGIN": true, "COMMIT": true,
	"ROLLBACK": true, "GRANT": true, "REVOKE": true, "ASSERT": true, "RAISE": true,
	// EXTERNAL_QUERY sends its text to a Cloud SQL or Spanner database,
	// which the guard cannot judge.
	"EXTERNAL_QUERY": true,
}

// deniedNames are the denied words that name a function rather than start
// a statement. Backticks change only how a name is parsed, so
// `EXTERNAL_QUERY` or `region-us.EXTERNAL_QUERY` resolves to the same
// table function: every dot-separated part of a quoted name is checked
// against them too. (A quoted `update` stays allowed: quoting a statement
// keyword makes it a plain name.)
var deniedNames = map[string]bool{"EXTERNAL_QUERY": true}

// checkReadOnly is the lexical layer of the read-only guard, run before
// anything is sent. The text must be one GoogleSQL query: it starts with
// SELECT or WITH (after any opening parentheses), holds no second
// statement after a ';' (trailing ';' are fine), and uses none of
// deniedWords, nor a deniedNames entry as any part of a backtick name
// (where a backslash escape is refused outright). Text the lexer cannot read, or a #legacySQL directive, is
// refused too. It fails closed: anything else is ErrReadOnly. The error
// names the reason, never the statement.
func checkReadOnly(text string) error {
	refuse := func(why string) error {
		return driver.Wrap(driver.ErrReadOnly, Name, "query", errors.New(why))
	}
	r, err := lex(text)
	if err != nil {
		return refuse("read-only: cannot read the statement: " + err.Error())
	}
	if r.legacySQL {
		return refuse("read-only: legacy SQL is not allowed")
	}
	toks := r.toks
	end := len(toks)
	for i, t := range toks {
		if t.kind == tokPunct && t.text == ";" {
			end = i
			break
		}
	}
	for _, t := range toks[end:] {
		if t.kind != tokPunct || t.text != ";" {
			return refuse("read-only: only one statement is allowed")
		}
	}
	toks = toks[:end]
	first := 0
	for first < len(toks) && toks[first].kind == tokPunct && toks[first].text == "(" {
		first++
	}
	if first >= len(toks) || toks[first].kind != tokWord {
		return refuse("read-only: only SELECT and WITH queries are allowed")
	}
	if w := strings.ToUpper(toks[first].text); w != "SELECT" && w != "WITH" {
		return refuse("read-only: only SELECT and WITH queries are allowed, not " + w)
	}
	for _, t := range toks {
		switch t.kind {
		case tokWord:
			if deniedWords[strings.ToUpper(t.text)] {
				return refuse("read-only: " + strings.ToUpper(t.text) + " is not allowed (quote a column of that name with backticks)")
			}
		case tokIdent:
			body := t.text[1 : len(t.text)-1]
			if strings.Contains(body, `\`) {
				// An escape could spell any name (\x45 is E).
				return refuse("read-only: escapes in backtick names are not allowed")
			}
			for part := range strings.SplitSeq(body, ".") {
				if w := strings.ToUpper(strings.TrimSpace(part)); deniedNames[w] {
					return refuse("read-only: " + w + " is not allowed, quoted or not")
				}
			}
		}
	}
	return nil
}

// checkStatementType is the dry-run layer of the read-only guard: the
// statementType real BigQuery reports for the dry run must be SELECT. A
// script, DML, DDL, EXPORT DATA, or a missing type is ErrReadOnly.
func checkStatementType(op, st string) error {
	if st == "SELECT" {
		return nil
	}
	if st == "" {
		return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("read-only: the dry run reported no statement type"))
	}
	return driver.Wrap(driver.ErrReadOnly, Name, op, errors.New("read-only: the dry run says this is a "+st+" statement, not a SELECT"))
}
