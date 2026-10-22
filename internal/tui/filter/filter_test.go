package filter

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

var cols = drivertest.People.Columns

func cond(f string, op driver.Op, v driver.Value) driver.Condition {
	return driver.Condition{Field: f, Op: op, Value: v}
}

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want driver.Filter
	}{
		{"", driver.Filter{}},
		{"   ", driver.Filter{}},
		{"age > 30", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpGt, driver.Int(30))}}},
		{"age >= -4", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpGe, driver.Int(-4))}}},
		{"age<3", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpLt, driver.Int(3))}}},
		{"age <= 3", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpLe, driver.Int(3))}}},
		{"age != 3", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpNe, driver.Int(3))}}},
		{"name ~ 'ō'", driver.Filter{Conditions: []driver.Condition{cond("name", driver.OpContains, driver.String("ō"))}}},
		{"email ^= 'user0'", driver.Filter{Conditions: []driver.Condition{cond("email", driver.OpPrefix, driver.String("user0"))}}},
		{"name = 'O''Brien'", driver.Filter{Conditions: []driver.Condition{cond("name", driver.OpEq, driver.String("O'Brien"))}}},
		{"age is null", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpIsNull, driver.Value{})}}},
		{"AGE IS NOT NULL", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpNotNull, driver.Value{})}}},
		{"age = null", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpIsNull, driver.Value{})}}},
		{"age != NULL", driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpNotNull, driver.Value{})}}},
		{"active = true AND age >= 18", driver.Filter{Conditions: []driver.Condition{
			cond("active", driver.OpEq, driver.Bool(true)), cond("age", driver.OpGe, driver.Int(18)),
		}}},
		{"score = 12.50", driver.Filter{Conditions: []driver.Condition{cond("score", driver.OpEq, driver.Decimal("12.50"))}}},
		{"score < 1e3", driver.Filter{Conditions: []driver.Condition{cond("score", driver.OpLt, driver.Float(1000))}}},
		{"score > 1E3", driver.Filter{Conditions: []driver.Condition{cond("score", driver.OpGt, driver.Float(1000))}}},
		{"score = -1.5e-2", driver.Filter{Conditions: []driver.Condition{cond("score", driver.OpEq, driver.Float(-0.015))}}},
		{"! age > 3 or id = 1", driver.Filter{Raw: "age > 3 or id = 1"}},
		{"  !{\"a\":1}", driver.Filter{Raw: `{"a":1}`}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Parse(tt.in, cols)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		in   string
		pos  int
		want string
	}{
		{"nme = 1", 0, `unknown column "nme" (did you mean "name"?)`},
		{"zzzzzzzz = 1", 0, `unknown column "zzzzzzzz"`},
		{"age >", 5, "expected a value"},
		{"age", 3, "expected an operator"},
		{"age > 3 or id = 1", 8, "expected 'and'"},
		{"age > 3 and", 11, "expected a column name"},
		{"name = 'abc", 7, "unterminated string"},
		{"age is 3", 7, "expected null or not null"},
		{"age ~ null", 6, "null"},
		{"age > @", 6, "unexpected"},
		{"!", 1, "expected a native filter"},
		{"age = 1.2.3", 6, "invalid number"},
		{"name ~ 5", 7, "contains/prefix need a quoted string"},
		{"name ^= 5", 8, "contains/prefix need a quoted string"},
		{"name ~ true", 7, "contains/prefix need a quoted string"},
		{"name ~ 1.5", 7, "contains/prefix need a quoted string"},
		{"age = 1 and name ^= false", 20, "contains/prefix need a quoted string"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			_, err := Parse(tt.in, cols)
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *ParseError", err)
			}
			if pe.Pos != tt.pos || !strings.Contains(pe.Msg, tt.want) {
				t.Fatalf("got pos=%d msg=%q, want pos=%d containing %q", pe.Pos, pe.Msg, tt.pos, tt.want)
			}
		})
	}
}

func TestParseColumnMatching(t *testing.T) {
	f, err := Parse("NAME = 'x'", cols)
	if err != nil || f.Conditions[0].Field != "name" {
		t.Fatalf("case-insensitive match: %+v %v", f, err)
	}
	// Exact match wins over a case-insensitive twin; ambiguous folds are errors.
	amb := []driver.Column{{Name: "Id"}, {Name: "ID"}}
	if f, err := Parse("ID = 1", amb); err != nil || f.Conditions[0].Field != "ID" {
		t.Fatalf("exact match: %+v %v", f, err)
	}
	if _, err := Parse("iD = 1", amb); err == nil {
		t.Fatal("ambiguous case-insensitive match should error")
	}
}

func run(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	return cmd()
}

func typeText(m Model, s string) Model {
	for _, r := range s {
		m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

func newModel() Model {
	return New(theme.Dark(), keymap.Default()).Resize(60)
}

func TestFilterModelApply(t *testing.T) {
	m := newModel().Open(cols, "")
	m = typeText(m, "age > 30")
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got, ok := run(t, cmd).(AppliedMsg)
	if !ok {
		t.Fatalf("enter should apply, got %T", run(t, cmd))
	}
	want := driver.Filter{Conditions: []driver.Condition{cond("age", driver.OpGt, driver.Int(30))}}
	if got.Text != "age > 30" || !reflect.DeepEqual(got.Filter, want) {
		t.Fatalf("applied = %+v", got)
	}
	if m.Focused() {
		t.Fatal("the bar should close after applying")
	}

	// Reopening prefills the current text; emptying it clears the filter.
	m = m.Open(cols, got.Text)
	if m.Value() != "age > 30" {
		t.Fatalf("prefill = %q", m.Value())
	}
	for range got.Text {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	clr, ok := run(t, cmd).(AppliedMsg)
	if !ok || clr.Text != "" || !reflect.DeepEqual(clr.Filter, driver.Filter{}) {
		t.Fatalf("clear = %+v %v", clr, ok)
	}
}

func TestFilterModelError(t *testing.T) {
	m := typeText(newModel().Open(cols, ""), "nme = 1")
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil && run(t, cmd) != nil {
		t.Fatalf("a parse error must not emit a message, got %T", run(t, cmd))
	}
	if !m.Focused() {
		t.Fatal("the bar must stay open on error")
	}
	v := ansi.Strip(m.View(60))
	lines := strings.Split(v, "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `did you mean "name"?`) {
		t.Fatalf("view:\n%s", v)
	}
	if i := strings.Index(lines[1], "^"); i != len("filter> ") {
		t.Fatalf("caret at column %d under:\n%s", i, v)
	}
	// Editing clears the error.
	m = typeText(m, "x")
	if strings.Contains(ansi.Strip(m.View(60)), "did you mean") {
		t.Fatal("the error should clear on edit")
	}
}

func TestFilterModelEsc(t *testing.T) {
	m := typeText(newModel().Open(cols, ""), "age >")
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if _, ok := run(t, cmd).(CancelledMsg); !ok {
		t.Fatalf("esc should cancel, got %T", run(t, cmd))
	}
	if m.Focused() {
		t.Fatal("the bar should close on esc")
	}
}

func TestFilterModelSwallowsKeys(t *testing.T) {
	m := typeText(newModel().Open(cols, ""), "q")
	if m.Value() != "q" {
		t.Fatalf("q must be typed into the input, got %q", m.Value())
	}
}

func TestFilterViewTiny(t *testing.T) {
	m := typeText(newModel().Open(cols, ""), "nme = 1")
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	for _, w := range []int{-1, 0, 1, 2, 5, 10} {
		_ = m.Resize(w).View(w)
	}
	_ = New(theme.Dark(), keymap.Default()).View(0)
}

func TestParseUnicodeIdentifiers(t *testing.T) {
	uc := []driver.Column{{Name: "名前"}, {Name: "naïve_col"}, {Name: "Ünï"}}
	f, err := Parse("名前 = 'x' and naïve_col > 1 and ünï is null", uc)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{f.Conditions[0].Field, f.Conditions[1].Field, f.Conditions[2].Field}
	if !reflect.DeepEqual(got, []string{"名前", "naïve_col", "Ünï"}) {
		t.Fatalf("fields = %v", got)
	}
	_, err = Parse("名字 = 1", uc)
	var pe *ParseError
	if !errors.As(err, &pe) || !strings.Contains(pe.Msg, `did you mean "名前"?`) {
		t.Fatalf("err = %v", err)
	}
}
