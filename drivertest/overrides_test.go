package drivertest

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// influxLikePeople is People with the storage limits of a store without
// Decimal and JSON: score is a Float, tags a String holding the JSON text.
func influxLikePeople(t *testing.T) ([]driver.Column, []driver.Row) {
	t.Helper()
	p := PeopleDataset()
	cols := append([]driver.Column(nil), p.Columns...)
	rows := make([]driver.Row, len(p.Rows))
	for i, r := range p.Rows {
		rows[i] = append(driver.Row(nil), r...)
	}
	for ci, c := range cols {
		for ri := range rows {
			v := rows[ri][ci]
			if v.Type() == driver.TypeNull {
				continue
			}
			switch c.Name {
			case "score":
				s, _ := v.AsText()
				f, err := strconv.ParseFloat(s, 64)
				if err != nil {
					t.Fatal(err)
				}
				rows[ri][ci] = driver.Float(f)
			case "tags":
				raw, _ := v.AsJSON()
				rows[ri][ci] = driver.String(string(raw))
			}
		}
		switch c.Name {
		case "score":
			cols[ci].Type = driver.TypeFloat
		case "tags":
			cols[ci].Type = driver.TypeString
		}
	}
	return cols, rows
}

func overrideFixture() Fixture {
	fx := baseFixture()
	fx.TypeOverrides = map[string]driver.Type{
		"score":              driver.TypeFloat,
		"tobira_people.tags": driver.TypeString,
	}
	return fx
}

func TestTypeOverridesLookup(t *testing.T) {
	score := driver.Column{Name: "score", Type: driver.TypeDecimal}
	var zero Fixture
	if got := zero.wantType(score); got != driver.TypeDecimal {
		t.Errorf("zero value: wantType = %s, want default Decimal", got)
	}
	fx := Fixture{Collection: "c", TypeOverrides: map[string]driver.Type{"score": driver.TypeFloat}}
	if got := fx.wantType(score); got != driver.TypeFloat {
		t.Errorf("column key: wantType = %s, want Float", got)
	}
	fx.TypeOverrides["c.score"] = driver.TypeString
	if got := fx.wantType(score); got != driver.TypeString {
		t.Errorf("qualified key must win: wantType = %s, want String", got)
	}
	fx.TypeOverrides = map[string]driver.Type{"other.score": driver.TypeFloat}
	if got := fx.wantType(score); got != driver.TypeDecimal {
		t.Errorf("other collection's key must not apply: wantType = %s", got)
	}
}

func TestTypeOverridesDataIsStillChecked(t *testing.T) {
	score := driver.Column{Name: "score", Type: driver.TypeDecimal}
	tags := driver.Column{Name: "tags", Type: driver.TypeJSON}
	fx := Fixture{TypeOverrides: map[string]driver.Type{"score": driver.TypeFloat, "tags": driver.TypeString}}
	for _, tc := range []struct {
		name      string
		col       driver.Column
		want, got driver.Value
		ok        bool
	}{
		{"float within 1e-9", score, driver.Decimal("12.50"), driver.Float(12.5), true},
		{"float close enough", score, driver.Decimal("0.1"), driver.Float(0.1 + 1e-12), true},
		{"float too far", score, driver.Decimal("12.50"), driver.Float(12.51), false},
		{"wrong type for override", score, driver.Decimal("12.50"), driver.Decimal("12.50"), false},
		{"int is not the override type", score, driver.Decimal("12"), driver.Int(12), false},
		{"null stays null", score, driver.Null(), driver.Null(), true},
		{"null vs value", score, driver.Decimal("1"), driver.Null(), false},
		{"json as string", tags, driver.JSON([]byte(`["a","b"]`)), driver.String(`["a", "b"]`), true},
		{"json different", tags, driver.JSON([]byte(`["a","b"]`)), driver.String(`["a"]`), false},
		{"json not overridden type", tags, driver.JSON([]byte(`["a"]`)), driver.JSON([]byte(`["a"]`)), false},
	} {
		if got := fx.equalPeopleValue(tc.col, tc.want, tc.got); got != tc.ok {
			t.Errorf("%s: equalPeopleValue = %v, want %v", tc.name, got, tc.ok)
		}
	}
	// Without overrides the default strict rules apply.
	var none Fixture
	if none.equalPeopleValue(score, driver.Decimal("0.1"), driver.Float(0.1)) {
		t.Error("no override: Decimal 0.1 must not equal Float 0.1")
	}
}

func TestTypeOverridesTime(t *testing.T) {
	created := driver.Column{Name: "created_at", Type: driver.TypeTime}
	ts := time.Date(2024, 3, 5, 12, 30, 45, 123456789, time.UTC)
	want := driver.Time(ts)
	asInt := Fixture{TypeOverrides: map[string]driver.Type{"created_at": driver.TypeInt}}
	asStr := Fixture{TypeOverrides: map[string]driver.Type{"created_at": driver.TypeString}}
	zone := time.FixedZone("x", 5*3600+1800)
	for _, tc := range []struct {
		name string
		fx   Fixture
		got  driver.Value
		ok   bool
	}{
		{"int exact", asInt, driver.Int(ts.UnixNano()), true},
		{"int off by one ns", asInt, driver.Int(ts.UnixNano() + 1), false},
		{"int off by minus one ns", asInt, driver.Int(ts.UnixNano() - 1), false},
		{"int wrong type", asInt, driver.Float(float64(ts.UnixNano())), false},
		{"int override but time got", asInt, want, false},
		{"string nano", asStr, driver.String(ts.Format(time.RFC3339Nano)), true},
		{"string other zone same instant", asStr, driver.String(ts.In(zone).Format(time.RFC3339Nano)), true},
		{"string plain RFC3339 drops nanos", asStr, driver.String(ts.Format(time.RFC3339)), false},
		{"string off by one ns", asStr, driver.String(ts.Add(time.Nanosecond).Format(time.RFC3339Nano)), false},
		{"string wrong instant", asStr, driver.String("2024-03-05T12:30:46Z"), false},
		{"string unparsable", asStr, driver.String("yesterday"), false},
		{"string empty", asStr, driver.String(""), false},
		{"string wrong type", asStr, driver.Decimal(ts.Format(time.RFC3339Nano)), false},
		{"null vs value", asStr, driver.Null(), false},
	} {
		if got := tc.fx.equalPeopleValue(created, want, tc.got); got != tc.ok {
			t.Errorf("%s: equalPeopleValue = %v, want %v", tc.name, got, tc.ok)
		}
	}
	whole := driver.Time(time.Date(2024, 3, 5, 12, 30, 45, 0, time.UTC))
	if !asStr.equalPeopleValue(created, whole, driver.String("2024-03-05T12:30:45Z")) {
		t.Error("string RFC3339 without fraction must match a whole-second time")
	}
	if got := asInt.wantType(created); got != driver.TypeInt {
		t.Errorf("wantType = %s, want Int", got)
	}
}

func TestValidCollectionType(t *testing.T) {
	for _, ct := range []driver.CollectionType{
		driver.CollectionTable, driver.CollectionView, driver.CollectionMaterializedView,
		driver.CollectionCollection, driver.CollectionMeasurement, driver.CollectionIndex,
		driver.CollectionKeyPattern, driver.CollectionOther,
	} {
		if !validCollectionType(ct) {
			t.Errorf("%q must be valid", ct)
		}
	}
	for _, ct := range []driver.CollectionType{"", "tabel", "TABLE"} {
		if validCollectionType(ct) {
			t.Errorf("%q must be invalid", ct)
		}
	}
}

func runFake(t *testing.T, f *Fake, fx Fixture) {
	t.Helper()
	open := func(cfg driver.ConnConfig) func(testing.TB) driver.Conn {
		return func(tb testing.TB) driver.Conn {
			tb.Helper()
			c, err := f.Driver().Open(context.Background(), cfg)
			if err != nil {
				tb.Fatal(err)
			}
			return c
		}
	}
	Run(t, Config{
		Driver: f.Driver(), Open: open(driver.ConnConfig{}),
		OpenReadOnly: open(driver.ConnConfig{ReadOnly: true}), Fixture: fx,
	})
}

func minimalFake() *Fake {
	return NewFake(WithPeople(), WithoutCapabilities(
		driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor,
		driver.CapVectorSearcher, driver.CapTimeSeriesReader, driver.CapGraphReader,
		driver.CapCompleter, driver.CapExporter, driver.CapWatcher, driver.CapReadOnly))
}

// A Fake storing Float scores and String tags passes the suite once the
// fixture declares those overrides.
func TestFakeWithTypeOverrides(t *testing.T) {
	f := minimalFake()
	cols, rows := influxLikePeople(t)
	f.AddCollection("main", peopleCollection, cols, rows, People.Key...)
	runFake(t, f, overrideFixture())
}

// Without the overrides the same Fake is reported as a type mismatch.
func TestFakeWithoutOverridesMismatches(t *testing.T) {
	f := minimalFake()
	cols, rows := influxLikePeople(t)
	f.AddCollection("main", peopleCollection, cols, rows, People.Key...)
	c, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	sch, err := c.Describe(context.Background(), overrideFixture().ref())
	if err != nil {
		t.Fatal(err)
	}
	var plain Fixture
	bad := 0
	for _, want := range People.Columns {
		for _, fld := range sch.Fields {
			if fld.Name == want.Name && fld.Type != plain.wantType(want) {
				bad++
			}
		}
	}
	if bad != 2 {
		t.Errorf("mismatched columns without overrides = %d, want 2 (score, tags)", bad)
	}
}

// The default Fake needs no overrides.
func TestFakeDefaultNeedsNoOverrides(t *testing.T) {
	runFake(t, minimalFake(), overrideFixture0())
}

// A measurement-typed collection passes the Collections check.
func TestFakeMeasurementTypedPeople(t *testing.T) {
	f := minimalFake()
	f.setType("main", peopleCollection, driver.CollectionMeasurement)
	runFake(t, f, overrideFixture0())
}

func overrideFixture0() Fixture {
	return baseFixture()
}

func baseFixture() Fixture {
	return Fixture{
		Namespace:        "main",
		Collection:       peopleCollection,
		SelectAll:        driver.Query{Text: "select * from tobira_people order by id"},
		Missing:          driver.Query{Text: "select * from nope"},
		Write:            driver.Query{Text: "delete from tobira_people where id = 1"},
		AutoIncrementKey: Bool(true),
		Slow:             driver.Query{Text: "sleep 10s"},
		ParamQuery:       driver.Query{Text: "select * from tobira_people where id = $1", Params: []driver.Value{driver.Int(7)}},
	}
}

// Engine storage limits are expressible with the existing overrides:
// Oracle before 23ai has no BOOLEAN (active is NUMBER(1), an Int) and an unconstrained NUMBER key reads as a Decimal; DynamoDB and
// Snowflake numbers read as Decimal; Cassandra, DynamoDB and Firestore
// hold tags as a native list; Cosmos DB numbers are Float.
func TestTypeOverridesCoverEngineStorageLimits(t *testing.T) {
	col := func(name string) driver.Column {
		i := slices.IndexFunc(People.Columns, func(c driver.Column) bool { return c.Name == name })
		return People.Columns[i]
	}
	fx := Fixture{TypeOverrides: map[string]driver.Type{
		"id": driver.TypeDecimal, "active": driver.TypeInt, "age": driver.TypeFloat, "tags": driver.TypeList,
	}}
	for _, tc := range []struct {
		name      string
		col       string
		want, got driver.Value
		ok        bool
	}{
		{"bool as int 1", "active", driver.Bool(true), driver.Int(1), true},
		{"bool as int 0", "active", driver.Bool(false), driver.Int(0), true},
		{"bool as wrong int", "active", driver.Bool(true), driver.Int(0), false},
		{"bool as int 2", "active", driver.Bool(true), driver.Int(2), false},
		{"bool not the override type", "active", driver.Bool(true), driver.Bool(true), false},
		{"int as decimal", "id", driver.Int(7), driver.Decimal("7"), true},
		{"int as decimal with scale", "id", driver.Int(7), driver.Decimal("7.000"), true},
		{"int as wrong decimal", "id", driver.Int(7), driver.Decimal("7.5"), false},
		{"int as float", "age", driver.Int(41), driver.Float(41), true},
		{"int as wrong float", "age", driver.Int(41), driver.Float(41.5), false},
		{"null int stays null", "age", driver.Null(), driver.Null(), true},
		{"json as list", "tags", driver.JSON([]byte(`["a","b"]`)), driver.List(driver.String("a"), driver.String("b")), true},
		{"json as wrong list", "tags", driver.JSON([]byte(`["a","b"]`)), driver.List(driver.String("b"), driver.String("a")), false},
		{"empty json as empty list", "tags", driver.JSON([]byte(`[]`)), driver.List(), true},
	} {
		if got := fx.equalPeopleValue(col(tc.col), tc.want, tc.got); got != tc.ok {
			t.Errorf("%s: equalPeopleValue = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

// The Transactor's commit check compares People with the overrides too:
// unchanged data stored with declared limits must read as unchanged, or
// a Commit that persisted nothing would pass.
func TestMatchesPeopleHonoursOverrides(t *testing.T) {
	cols, rows := influxLikePeople(t)
	if !matchesPeople(overrideFixture(), cols, rows) {
		t.Error("unchanged People stored with the declared overrides must match")
	}
	if matchesPeople(Fixture{}, cols, rows) {
		t.Error("without the overrides the Float scores and String tags must not match")
	}
	changed := slices.Clone(rows)
	changed = changed[1:]
	if matchesPeople(overrideFixture(), cols, changed) {
		t.Error("a missing row must not match")
	}
	renamed := make([]driver.Row, len(rows))
	for i := range rows {
		renamed[i] = slices.Clone(rows[i])
	}
	renamed[1][1] = driver.String("tx")
	if matchesPeople(overrideFixture(), cols, renamed) {
		t.Error("a renamed row must not match")
	}
}

// A store whose keys must be strings (Cosmos DB's id) declares People's id
// as String: the kit compares the decimal text, reads ids back from it and
// writes its probe ids as text.
func TestTypeOverridesTextKey(t *testing.T) {
	id := People.Columns[slices.IndexFunc(People.Columns, func(c driver.Column) bool { return c.Name == "id" })]
	fx := Fixture{TypeOverrides: map[string]driver.Type{"id": driver.TypeString}}
	for _, tc := range []struct {
		name      string
		want, got driver.Value
		ok        bool
	}{
		{"int as its text", driver.Int(7), driver.String("7"), true},
		{"int as padded text", driver.Int(7), driver.String("07"), false},
		{"int as other text", driver.Int(7), driver.String("8"), false},
		{"int as signed text", driver.Int(7), driver.String("+7"), false},
		{"int not the override type", driver.Int(7), driver.Int(7), false},
		{"int as decimal", driver.Int(7), driver.Decimal("7"), false},
	} {
		if got := fx.equalPeopleValue(id, tc.want, tc.got); got != tc.ok {
			t.Errorf("%s: equalPeopleValue = %v, want %v", tc.name, got, tc.ok)
		}
	}
	for _, tc := range []struct {
		fx   Fixture
		v    driver.Value
		want int64
		ok   bool
	}{
		{Fixture{}, driver.Int(5), 5, true},
		{Fixture{}, driver.String("5"), 0, false},
		{fx, driver.String("5"), 5, true},
		{fx, driver.Int(5), 5, true},
		{fx, driver.String("05"), 0, false},
		{fx, driver.String("x"), 0, false},
		{fx, driver.Null(), 0, false},
	} {
		n, ok := tc.fx.peopleID(tc.v)
		if n != tc.want || ok != tc.ok {
			t.Errorf("peopleID(%s) with overrides %v = %d, %v; want %d, %v", tc.v, tc.fx.TypeOverrides, n, ok, tc.want, tc.ok)
		}
	}
	if v := fx.idValue(1000); !EqualValue(driver.String("1000"), v) || v.Type() != driver.TypeString {
		t.Errorf("idValue(1000) under a String override = %s (%s), want the String \"1000\"", v, v.Type())
	}
	if v := (Fixture{}).idValue(1000); v.Type() != driver.TypeInt {
		t.Errorf("idValue(1000) without overrides = %s (%s), want an Int", v, v.Type())
	}
	row := fx.probeRow(1000, "p")
	if v, _ := row.Get("id"); v.Type() != driver.TypeString {
		t.Errorf("probeRow under a String id override has id %s (%s), want a String", v, v.Type())
	}
}
