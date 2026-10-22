package drivertest

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// peopleResult is People as a driver would return it: columns plus rows,
// each row a private copy.
func peopleResult() ([]driver.Column, []driver.Row) {
	rows := make([]driver.Row, len(People.Rows))
	for i, r := range People.Rows {
		rows[i] = slices.Clone(r)
	}
	return slices.Clone(People.Columns), rows
}

// shuffled reverses the rows and swaps the first two columns, so neither
// row order nor column order matches People.
func shuffled() ([]driver.Column, []driver.Row) {
	cols, rows := peopleResult()
	slices.Reverse(rows)
	cols[0], cols[1] = cols[1], cols[0]
	for _, r := range rows {
		r[0], r[1] = r[1], r[0]
	}
	return cols, rows
}

func TestPeopleMismatchesOrdered(t *testing.T) {
	cols, rows := peopleResult()
	if got := peopleMismatches(Fixture{}, "x", cols, rows); len(got) != 0 {
		t.Fatalf("People in order must match: %v", got)
	}
	cols, rows = shuffled()
	got := peopleMismatches(Fixture{}, "x", cols, rows)
	if len(got) == 0 {
		t.Fatal("without UnorderedSelectAll, rows out of id order must fail")
	}
	if matchesPeople(Fixture{}, cols, rows) {
		t.Error("matchesPeople: rows out of order must not match without UnorderedSelectAll")
	}
}

func TestPeopleMismatchesUnordered(t *testing.T) {
	fx := Fixture{UnorderedSelectAll: true}
	cols, rows := shuffled()
	if got := peopleMismatches(fx, "x", cols, rows); len(got) != 0 {
		t.Fatalf("UnorderedSelectAll: People in any order must match: %v", got)
	}
	if !matchesPeople(fx, cols, rows) {
		t.Error("matchesPeople must honour UnorderedSelectAll")
	}
	// id is column 1 of the shuffled result.
	for _, tc := range []struct {
		name   string
		mutate func(rows []driver.Row) []driver.Row
		want   string
	}{
		{"wrong value", func(rows []driver.Row) []driver.Row {
			rows[3][0] = driver.String("someone else") // name
			return rows
		}, `column "name"`},
		{"missing row", func(rows []driver.Row) []driver.Row { return rows[1:] }, "want 57 rows, got 56"},
		{"extra row", func(rows []driver.Row) []driver.Row { return append(rows, slices.Clone(rows[0])) }, "want 57 rows, got 58"},
		{"duplicate replacing a row", func(rows []driver.Row) []driver.Row {
			rows[5] = slices.Clone(rows[6])
			return rows
		}, "appears 2 times"},
		{"unknown id", func(rows []driver.Row) []driver.Row {
			rows[5][1] = driver.Int(9999)
			return rows
		}, "is missing"},
	} {
		cols, rows := shuffled()
		rows = tc.mutate(rows)
		got := peopleMismatches(fx, "x", cols, rows)
		if len(got) == 0 {
			t.Errorf("%s: UnorderedSelectAll passed a wrong result", tc.name)
			continue
		}
		if !strings.Contains(strings.Join(got, "\n"), tc.want) {
			t.Errorf("%s: mismatches %q do not mention %q", tc.name, got, tc.want)
		}
		if matchesPeople(fx, cols, rows) {
			t.Errorf("%s: matchesPeople passed a wrong result", tc.name)
		}
	}
	// Columns are still checked.
	cols, rows = shuffled()
	cols[7].Name = "renamed"
	if got := peopleMismatches(fx, "x", cols, rows); len(got) == 0 || !strings.Contains(got[0], "no column") {
		t.Errorf("a missing column must fail: %v", got)
	}
}

// reversedConn returns the Fake's SelectAll rows in reverse order.
type reversedConn struct{ driver.Conn }

func (r reversedConn) Query(ctx context.Context, q driver.Query) (driver.ResultSet, error) {
	rs, err := r.Conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		return nil, err
	}
	slices.Reverse(rows)
	return driver.NewSliceResultSet(cols, rows, st), nil
}

// A driver whose SelectAll is unordered passes the core read checks with
// UnorderedSelectAll set.
func TestUnorderedSelectAllEndToEnd(t *testing.T) {
	f := NewFake(WithPeople())
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			c, err := f.Driver().Open(context.Background(), driver.ConnConfig{ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return reversedConn{c}
		}
	}
	c := Config{Driver: f.Driver(), Open: open(false), OpenReadOnly: open(true), Fixture: Fixture{
		Namespace: "main", Collection: "tobira_people",
		SelectAll:          driver.Query{Text: "select * from tobira_people order by id"},
		Missing:            driver.Query{Text: "select * from nope"},
		Write:              driver.Query{Text: "delete from tobira_people where id = 1"},
		UnorderedSelectAll: true,
	}}
	for _, s := range []subtest{{"QueryRead", testQueryRead}, {"ReadOnly", testReadOnly}, {"Concurrency", testConcurrency}} {
		t.Run(s.name, func(t *testing.T) { s.fn(t, c) })
	}
}
