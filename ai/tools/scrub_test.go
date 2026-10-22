package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
)

// Scrubbing fails closed: everything from the first quote-like character to
// the end of the message becomes "…" (a value may hold a newline); codes in
// the dropped part survive;
// runs of 4+ digits elsewhere become "#".
func TestScrubErrorGreedy(t *testing.T) {
	for in, want := range map[string]string{
		`invalid input syntax for type integer: "alice@example.com"`:              `invalid input syntax for type integer: …`,
		`ungültige Eingabesyntax für Typ integer: »alice@example.com«`:            `ungültige Eingabesyntax für Typ integer: …`,
		`syntaxe en entrée invalide pour le type integer : « alice@example.com »`: `syntaxe en entrée invalide pour le type integer : …`,
		`ungültig: „alice@example.com“`:                                           `ungültig: …`,
		`値 「alice」 は無効`:                                                           `値 …`,
		`it ‘quoted’ here`:                                                        `it …`,
		`invalid input syntax for type json: "{"ssn":"123-45-6789"}"`:             `invalid input syntax for type json: …`,
		`Incorrect integer value: 'O'Hara' for column 'n' at row 1`:               `Incorrect integer value: …`,
		`syntax error at or near "selec" (SQLSTATE 42601)`:                        `syntax error at or near …`,
		`near "x" (SQLSTATE SECRT)`:                                               `near …`,
		"Error 1064 (42000): You have an error near 'selec x' at line 1":          "Error 1064 (42000): You have an error near …",
		"head \"x\" Error 1146 (42S02) tail":                                      "head …",
		"line one \"secret\" more\nDETAIL: Key (id)=(12345) exists":               "line one …",
		"no quote 12345\nDETAIL: then \"x\"\nHINT: 9999":                          "no quote #\nDETAIL: then …",
		`body $q$top secret$q$ end`:                                               `body …`,
		`body $$x$$ end`:                                                          `body …`,
		`param $1 and 98765`:                                                      `param $1 and #`,
		"col `secret` gone":                                                       "col …",
		`code 123 at line 42`:                                                     `code 123 at line 42`,
		`SQLSTATE 22P02 without quotes 2026`:                                      `SQLSTATE 22P02 without quotes #`,
		``:                                                                        ``,
	} {
		if got := scrubError(in); got != want {
			t.Errorf("scrubError(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// Every rune of unicode.Quotation_Mark, and the CJK and guillemet extras,
// starts a scrub.
func TestScrubQuoteRunes(t *testing.T) {
	for _, r := range []rune{'\'', '"', '`', '«', '»', '‹', '›', '「', '」', '『', '』', '„', '“', '”', '‘', '’', '‚', '〝', '＂'} {
		in := "x " + string(r) + "secret"
		if got := scrubError(in); got != "x …" {
			t.Errorf("quote %U: %q -> %q", r, in, got)
		}
	}
}

// Without row consent every driver error is only kind, a generic text and
// the code from driver.Error.Code. Pure syntax errors (pg 42601, mysql 1064
// and 1149, sqlite "near"/"incomplete input"/"syntax error") keep the text
// before the first quote.
func TestErrorsCollapseToKindAndCode(t *testing.T) {
	id := func(s string) string { return s }
	cases := []struct {
		err  *driver.Error
		want string
	}{
		{&driver.Error{Kind: driver.ErrSyntax, Driver: "postgres", Op: "query", Code: "22P02",
			Err: errors.New(`ERROR: invalid input syntax for type integer: "alice" (SQLSTATE 22P02)`)}, "invalid query: syntax error (SQLSTATE 22P02)"},
		{&driver.Error{Driver: "postgres", Op: "query", Code: "42883",
			Err: errors.New(`ERROR: operator does not exist: alice (SQLSTATE 42883)`)}, "error: query failed (SQLSTATE 42883)"},
		{&driver.Error{Driver: "postgres", Op: "query", Code: "42883",
			Err: errors.New(`ERROR: function alice() does not exist (SQLSTATE 42883)`)}, "error: query failed (SQLSTATE 42883)"},
		{&driver.Error{Kind: driver.ErrNotFound, Driver: "postgres", Op: "query", Code: "42P01",
			Err: errors.New(`ERROR: relation "alice" does not exist (SQLSTATE 42P01)`)}, "not found: object not found; use describe/list tools to check names (SQLSTATE 42P01)"},
		{&driver.Error{Kind: driver.ErrConstraint, Driver: "postgres", Op: "query", Code: "23505",
			Err: errors.New("Key (email)=(alice@example.com) already exists")}, "constraint violation: query failed (SQLSTATE 23505)"},
		{&driver.Error{Kind: driver.ErrPermission, Driver: "postgres", Op: "query", Code: "42501", Err: errors.New("permission denied for table alice")}, "permission denied: permission denied (SQLSTATE 42501)"},
		{&driver.Error{Kind: driver.ErrReadOnly, Driver: "fake", Op: "query", Err: errors.New("delete refused: read-only")}, "read-only violation: refused: only read-only statements may run here"},
		{&driver.Error{Kind: driver.ErrConstraint, Driver: "mysql", Op: "query", Code: "1366", Err: errors.New("Error 1366 (HY000): Incorrect integer value: 'O'Hara'")}, "constraint violation: query failed (error 1366)"},
		{&driver.Error{Driver: "sqlite", Op: "query", Code: "1", Err: errors.New("SQL logic error: no such function: alice")}, "error: query failed (error 1)"},
		{&driver.Error{Driver: "x", Op: "query", Code: "SECRET-CODE", Err: errors.New("alice")}, "error: query failed"},
		// The syntax whitelist keeps the text before the first quote.
		{&driver.Error{Kind: driver.ErrSyntax, Driver: "postgres", Op: "query", Code: "42601",
			Err: errors.New(`ERROR: syntax error at or near "selec" (SQLSTATE 42601)`)}, "invalid query: ERROR: syntax error at or near … (SQLSTATE 42601)"},
		{&driver.Error{Kind: driver.ErrSyntax, Driver: "mysql", Op: "query", Code: "1064",
			Err: errors.New(`Error 1064 (42000): You have an error in your SQL syntax near 'O'Hara' at line 1`)}, "invalid query: Error 1064 (42000): You have an error in your SQL syntax near … (error 1064)"},
		{&driver.Error{Kind: driver.ErrSyntax, Driver: "mysql", Op: "query", Code: "1149",
			Err: errors.New(`Error 1149 (42000): syntax error near 'x'`)}, "invalid query: Error 1149 (42000): syntax error near … (error 1149)"},
		{&driver.Error{Kind: driver.ErrSyntax, Driver: "sqlite", Op: "query", Code: "1",
			Err: errors.New(`SQL logic error: near "selec": syntax error`)}, "invalid query: near … (error 1)"},
		{&driver.Error{Kind: driver.ErrSyntax, Driver: "sqlite", Op: "query", Code: "1",
			Err: errors.New(`incomplete input`)}, "invalid query: incomplete input (error 1)"},
		// A pg error that is not 42601 is not whitelisted, even of kind syntax.
		{&driver.Error{Kind: driver.ErrSyntax, Driver: "postgres", Op: "query", Code: "42000", Err: errors.New("syntax-ish alice")}, "invalid query: syntax error (SQLSTATE 42000)"},
	}
	for _, tc := range cases {
		if got := scrubDriverError(tc.err, id); got != tc.want {
			t.Errorf("%s %s:\n got %q\nwant %q", tc.err.Driver, tc.err.Code, got, tc.want)
		}
	}
	if got := scrubDriverError(errors.New(`boom alice`), id); got != "error: query failed" {
		t.Errorf("plain error: %q", got)
	}
}

// Property: for a corpus of values embedded in error templates, the
// scrubbed error never holds a 3+ character substring of the value.
func TestScrubNeverLeaksAValue(t *testing.T) {
	values := []string{
		"alice@example.com", "O'Hara", `{"ssn":"123-45-6789"}`, `x"y'z`, "»inner«", "„low“", "‘single’",
		"bob.12345@example.com", "Zoë Müller", "李小龍", "back`tick", "$$dollar$$", "$q$tag$q$", "line\nbreak value",
		"9876543210", "unquoted-free-text", "  padded  ", "a'b\"c`d",
	}
	type tmpl struct {
		drv, code, text string
	}
	templates := []tmpl{
		{"postgres", "22P02", `ERROR: invalid input syntax for type integer: "%s" (SQLSTATE 22P02)`},
		{"postgres", "22P02", `FEHLER: ungültige Eingabesyntax für Typ integer: »%s« (SQLSTATE 22P02)`},
		{"postgres", "22P02", `ERREUR: syntaxe en entrée invalide pour le type integer : « %s » (SQLSTATE 22P02)`},
		{"postgres", "23505", `ERROR: duplicate key value violates unique constraint "k" (SQLSTATE 23505)` + "\nDETAIL: Key (email)=(%s) already exists."},
		{"postgres", "42703", `ERROR: column "%s" does not exist (SQLSTATE 42703)`},
		{"postgres", "42883", `ERROR: operator does not exist: text = „%s“ (SQLSTATE 42883)`},
		{"postgres", "42883", `ERROR: operator does not exist: %s (SQLSTATE 42883)`},
		{"postgres", "42883", `ERROR: function %s() does not exist (SQLSTATE 42883)`},
		{"postgres", "42883", `FEHLER: Operator existiert nicht: %s (SQLSTATE 42883)`},
		{"postgres", "42601", `ERROR: syntax error at or near "%s" (SQLSTATE 42601)`},
		{"sqlite", "1", `SQL logic error: no such function: %s`},
		{"sqlite", "1", `SQL logic error: near "%s": syntax error`},
		{"mysql", "1366", `Error 1366 (HY000): Incorrect integer value: '%s' for column 'n' at row 1`},
		{"mysql", "1062", `Error 1062 (23000): Duplicate entry '%s' for key 'email'`},
		{"mysql", "1054", "Error 1054 (42S22): Unknown column '%s' in 'field list'"},
		{"mysql", "1064", "Error 1064 (42000): You have an error in your SQL syntax; near '%s' at line 1"},
		{"sqlite", "1", `SQL logic error: no such column: "%s"`},
		{"fake", "", "unrecognized token: `%s`"},
	}
	ident := func(s string) string { return s }
	for _, tp := range templates {
		for _, v := range values {
			e := &driver.Error{Kind: driver.ErrSyntax, Driver: tp.drv, Op: "query", Code: tp.code, Err: fmt.Errorf(tp.text, v)}
			out := scrubDriverError(e, ident)
			// Substrings the message has anyway (template words, kind, driver)
			// are not leaks.
			skeleton := tp.drv + " query " + fmt.Sprintf(tp.text, "") + " invalid query: data exception error SQLSTATE " + tp.code
			if sub, ok := leaks(out, v, skeleton); ok {
				t.Errorf("%s %s with %q leaks %q:\n%s", tp.drv, tp.code, v, sub, out)
			}
		}
	}
}

// leaks reports a 3+ rune substring of v found in out, ignoring substrings
// with leading or trailing space and those the skeleton already holds.
func leaks(out, v, skeleton string) (string, bool) {
	r := []rune(v)
	for i := 0; i+3 <= len(r); i++ {
		sub := string(r[i : i+3])
		if strings.TrimSpace(sub) != sub || !utf8.ValidString(sub) || strings.Contains(skeleton, sub) {
			continue
		}
		if strings.Contains(out, sub) {
			return sub, true
		}
	}
	return "", false
}

// Through the tool: a translated message under never.
func TestToolScrubsTranslatedMessages(t *testing.T) {
	e := newEnv(t, withPolicy(RowsNever))
	e.fake.FailNext("query", &driver.Error{Driver: "fake", Op: "query", Code: "42883",
		Err: errors.New(`FEHLER: Operator existiert nicht: alice@example.com (SQLSTATE 42883)`)})
	q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	if q.Error != "error: query failed (SQLSTATE 42883)" {
		t.Fatalf("error = %q", q.Error)
	}
}

// A panicking consent callback must not leave the flight open: the next
// call asks again instead of waiting forever.
func TestAskConsentPanicReleasesTheFlight(t *testing.T) {
	e := newEnv(t, withPolicy(RowsAsk))
	calls := 0
	e.setEvents(assistant.Events{ConsentRows: func(context.Context, assistant.ConsentRequest) (assistant.Consent, error) {
		calls++
		if calls == 1 {
			panic("ui crashed")
		}
		return assistant.ConsentOnce, nil
	}})
	func() {
		defer func() { _ = recover() }()
		_, _ = e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
	}()
	done := make(chan RowsOutput, 1)
	go func() {
		q, _ := e.ts.Query(ctx, QueryInput{Connection: "dev", Query: "select * from orders"})
		done <- q
	}()
	select {
	case q := <-done:
		if q.Rows == nil || calls != 2 {
			t.Fatalf("after a panic: %+v (calls %d)", q, calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call after a panicking prompt hung on the dead flight")
	}
}

// Production turns put their callbacks on the context (WithEvents); the
// Toolset's own deps carry none, so no caller can bypass a turn's callbacks.
func TestToolDepsCarryNoEvents(t *testing.T) {
	if _, ok := reflect.TypeOf(ToolDeps{}).FieldByName("Events"); ok {
		t.Fatal("ToolDeps.Events exists: callbacks come from the turn's context only")
	}
	ts := NewToolset(ToolDeps{})
	if ev := ts.events(context.Background()); ev.ConsentRows != nil || ev.ConfirmExecute != nil || ev.ToolCall != nil {
		t.Fatal("a context without callbacks got some")
	}
}

// A front end that refuses a statement without asking (an open transaction
// in the TUI) passes its reason to the model as the call's error.
func TestExecuteRefusalReason(t *testing.T) {
	e := newEnv(t)
	const why = "a transaction is open on dev; commit or roll back first"
	turn := WithEvents(WithExecute(ctx), assistant.Events{ConfirmExecute: func(context.Context, string, string) (bool, error) {
		return false, &assistant.ExecuteRefusal{Reason: why}
	}})
	before := len(e.fake.Calls())
	out, err := e.ts.Exec(turn, ExecuteInput{Connection: "dev", Statement: "delete from orders"})
	if err != nil || out.Error != why {
		t.Fatalf("execute = %+v, %v; want the refusal %q", out, err, why)
	}
	for _, c := range e.fake.Calls()[before:] {
		if c.Op == "query" {
			t.Fatalf("the statement ran: %+v", c)
		}
	}
}
