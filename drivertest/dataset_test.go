package drivertest

import (
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestPeopleShape(t *testing.T) {
	d := People
	wantCols := []struct {
		name string
		typ  driver.Type
	}{
		{"id", driver.TypeInt}, {"name", driver.TypeString}, {"email", driver.TypeString},
		{"age", driver.TypeInt}, {"score", driver.TypeDecimal}, {"active", driver.TypeBool},
		{"created_at", driver.TypeTime}, {"tags", driver.TypeJSON},
	}
	if len(d.Columns) != len(wantCols) {
		t.Fatalf("columns = %d, want %d", len(d.Columns), len(wantCols))
	}
	for i, w := range wantCols {
		if d.Columns[i].Name != w.name || d.Columns[i].Type != w.typ {
			t.Errorf("column %d = %s/%v, want %s/%v", i, d.Columns[i].Name, d.Columns[i].Type, w.name, w.typ)
		}
	}
	if len(d.Key) != 1 || d.Key[0] != "id" {
		t.Errorf("Key = %v, want [id]", d.Key)
	}
	if len(d.Rows) != 57 {
		t.Fatalf("rows = %d, want 57", len(d.Rows))
	}
	emails := map[string]bool{}
	nullAges, trailingZero, nonASCII, emptyTags := 0, false, false, false
	for i, r := range d.Rows {
		if len(r) != len(wantCols) {
			t.Fatalf("row %d has %d values", i, len(r))
		}
		if id, _ := r[0].AsInt(); id != int64(i+1) {
			t.Errorf("row %d id = %d", i, id)
		}
		email, _ := r[2].AsText()
		if emails[email] {
			t.Errorf("duplicate email %q", email)
		}
		emails[email] = true
		if r[3].IsNull() {
			nullAges++
		}
		if name, _ := r[1].AsText(); name == "Ōtani Shōhei" {
			nonASCII = true
		}
		if s, _ := r[4].AsText(); s == "12.50" {
			trailingZero = true
		}
		ts, ok := r[6].AsTime()
		if !ok || ts.Location() != time.UTC || ts.Nanosecond() != 0 {
			t.Errorf("row %d created_at = %v, want UTC whole seconds", i, ts)
		}
		raw, ok := r[7].AsJSON()
		if !ok {
			t.Errorf("row %d tags is %v, want JSON", i, r[7].Type())
		}
		if string(raw) == "[]" {
			emptyTags = true
		}
	}
	if nullAges < 5 {
		t.Errorf("null ages = %d, want >= 5", nullAges)
	}
	if !nonASCII || !trailingZero || !emptyTags {
		t.Errorf("missing edge rows: nonASCII=%v trailingZero=%v emptyTags=%v", nonASCII, trailingZero, emptyTags)
	}
}

func TestPeopleDatasetIsCopy(t *testing.T) {
	d := PeopleDataset()
	d.Rows[0][1] = driver.String("changed")
	d.Columns[0].Name = "changed"
	d.Key[0] = "changed"
	if n, _ := People.Rows[0][1].AsText(); n == "changed" {
		t.Error("mutating the copy changed People rows")
	}
	if People.Columns[0].Name != "id" || People.Key[0] != "id" {
		t.Error("mutating the copy changed People columns or key")
	}
}

func TestPeopleCollection(t *testing.T) {
	if got := PeopleCollection(); got != "tobira_people" {
		t.Errorf("PeopleCollection() = %q", got)
	}
}

func countTuples(stmts []string) int {
	n := 0
	for _, s := range stmts {
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "(") {
				n++
			}
		}
	}
	return n
}

func TestSQLSeed(t *testing.T) {
	tests := []struct {
		dialect  string
		contains []string
		tuple    string
	}{
		{"postgres", []string{"CREATE TABLE tobira_people", "created_at timestamptz", "tags jsonb", "score numeric(10,2)", "active boolean", "UNIQUE"},
			"TIMESTAMPTZ '2026-01-02 03:04:05+00'"},
		{"sqlite", []string{"CREATE TABLE tobira_people", "DECIMAL(10,2)", "BOOLEAN", "DATETIME", "JSON", "UNIQUE"},
			"'2026-01-02 03:04:05'"},
		{"mysql", []string{"CREATE TABLE tobira_people", "DATETIME", "JSON", "DECIMAL(10,2)", "TINYINT(1)", "UNIQUE"},
			"'2026-01-02 03:04:05'"},
	}
	for _, tc := range tests {
		t.Run(tc.dialect, func(t *testing.T) {
			stmts := SQLSeed(tc.dialect)
			if len(stmts) < 2 {
				t.Fatalf("got %d statements", len(stmts))
			}
			for _, c := range tc.contains {
				if !strings.Contains(stmts[0], c) {
					t.Errorf("CREATE lacks %q:\n%s", c, stmts[0])
				}
			}
			for _, s := range stmts[1:] {
				if !strings.HasPrefix(s, "INSERT INTO tobira_people") {
					t.Errorf("non-INSERT statement: %.40s", s)
				}
			}
			if got := countTuples(stmts[1:]); got != 57 {
				t.Errorf("inserted rows = %d, want 57", got)
			}
			all := strings.Join(stmts, "\n")
			if !strings.Contains(all, tc.tuple) {
				t.Errorf("seed lacks timestamp literal %q", tc.tuple)
			}
			if !strings.Contains(all, "Ōtani Shōhei") {
				t.Error("seed lacks non-ASCII name")
			}
		})
	}
}

func TestSQLSeedLiterals(t *testing.T) {
	pg := strings.Join(SQLSeed("postgres"), "\n")
	if !strings.Contains(pg, "TRUE") || !strings.Contains(pg, "FALSE") {
		t.Error("postgres seed should use TRUE/FALSE")
	}
	if !strings.Contains(pg, "O''") {
		t.Error("single quotes must be doubled")
	}
	if !strings.Contains(strings.Join(SQLSeed("sqlite"), "\n"), `'["admin","beta"]'`) {
		t.Error("sqlite seed lacks JSON tags literal")
	}
}

func TestSQLSeedUnknownDialectPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	SQLSeed("oracle")
}
