//go:build integration

package oracle

import (
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestTypes(t *testing.T) {
	db := admin(t)
	mustExec(t, db, `CREATE TABLE tobira_types (id NUMBER(10) PRIMARY KEY, n NUMBER, d NUMBER(10,2), big NUMBER(30),
	  b BOOLEAN, j JSON, v VARCHAR2(20), nv NVARCHAR2(10), ch CHAR(3), c CLOB, bl BLOB, r RAW(8), dt DATE,
	  ts TIMESTAMP, tz TIMESTAMP WITH TIME ZONE, ltz TIMESTAMP WITH LOCAL TIME ZONE, f BINARY_FLOAT,
	  bd BINARY_DOUBLE, iv INTERVAL DAY TO SECOND, ym INTERVAL YEAR TO MONTH, fl FLOAT, vec VECTOR(3, FLOAT32))`)
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE tobira_types PURGE") })
	mustExec(t, db, `INSERT INTO tobira_types VALUES (1, 123456789012345678901234567890, 12.50, 123456789012345678901234567890,
	  TRUE, JSON('{"a":[1,2]}'), 'hi', N'ñü', 'ab', 'clob text', HEXTORAW('DEADBEEF'), HEXTORAW('0102'),
	  DATE '2026-01-02', TIMESTAMP '2026-01-02 03:04:05.123456', TIMESTAMP '2026-01-02 03:04:05 +02:00',
	  TIMESTAMP '2026-01-02 03:04:05 +00:00', 1.5, 2.25, INTERVAL '1 02:03:04' DAY TO SECOND,
	  INTERVAL '1-2' YEAR TO MONTH, 3.14, '[1,2,3]')`)
	mustExec(t, db, `INSERT INTO tobira_types (id, b) VALUES (2, FALSE)`)
	c := openOra(t, false)
	rows, _, cols := collect(t, c, driver.Query{Text: "SELECT * FROM tobira_types ORDER BY id"})
	if len(rows) != 2 {
		t.Fatalf("rows: %d", len(rows))
	}
	want := map[string]driver.Value{
		"ID":  driver.Int(1),
		"N":   driver.Decimal("123456789012345678901234567890"),
		"D":   driver.Decimal("12.50"),
		"BIG": driver.Decimal("123456789012345678901234567890"),
		"B":   driver.Int(1), // BOOLEAN reaches go-ora as NUMBER
		"J":   driver.JSON([]byte(`{"a":[1,2]}`)),
		"V":   driver.String("hi"),
		"NV":  driver.String("ñü"),
		"CH":  driver.String("ab "),
		"C":   driver.String("clob text"),
		"BL":  driver.Bytes([]byte{0xde, 0xad, 0xbe, 0xef}),
		"R":   driver.Bytes([]byte{1, 2}),
		"DT":  driver.Time(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
		"TS":  driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)),
		"TZ":  driver.Time(time.Date(2026, 1, 2, 1, 4, 5, 0, time.UTC)),
		"LTZ": driver.Time(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)),
		"F":   driver.Float(1.5),
		"BD":  driver.Float(2.25),
		"IV":  driver.String("+01 02:03:04.000000"),
		"YM":  driver.String("+01-02"),
		"FL":  driver.Int(3), // replaced below: 3.14 is a Decimal
		"VEC": driver.Vector([]float32{1, 2, 3}),
	}
	want["FL"] = driver.Decimal("3.14")
	for i, col := range cols {
		w, ok := want[col.Name]
		if !ok {
			t.Errorf("unexpected column %s", col.Name)
			continue
		}
		got := rows[0][i]
		if got.Type() != w.Type() || !drivertest.EqualValue(w, got) {
			t.Errorf("%s (%s): got %s (%s), want %s (%s)", col.Name, col.NativeType, got, got.Type(), w, w.Type())
		}
		if col.Name != "ID" && col.Name != "B" && !rows[1][i].IsNull() {
			t.Errorf("%s: row 2 should be NULL, got %s", col.Name, rows[1][i])
		}
	}
	if !drivertest.EqualValue(driver.Bool(false), rows[1][4]) {
		t.Errorf("B row 2: %s", rows[1][4])
	}
	// Describe sees the dictionary types, so BOOLEAN is Bool there, and
	// Browse turns its 1 and 0 into Bools.
	s, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "TOBIRA_TYPES"})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]driver.Type{}
	for _, f := range s.Fields {
		types[f.Name] = f.Type
	}
	for name, typ := range map[string]driver.Type{"B": driver.TypeBool, "J": driver.TypeJSON, "D": driver.TypeDecimal, "ID": driver.TypeInt,
		"C": driver.TypeString, "BL": driver.TypeBytes, "TZ": driver.TypeTime, "VEC": driver.TypeVector, "FL": driver.TypeDecimal, "F": driver.TypeFloat} {
		if types[name] != typ {
			t.Errorf("Describe %s: %s, want %s", name, types[name], typ)
		}
	}
	rs, err := c.Browse(ctxT(t), driver.CollectionRef{Name: "TOBIRA_TYPES"}, driver.BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bcols := rs.Columns()
	brows, _, err := driver.Collect(ctxT(t), rs, 0)
	if err != nil {
		t.Fatal(err)
	}
	bi := 4
	if bcols[bi].Name != "B" || bcols[bi].Type != driver.TypeBool {
		t.Errorf("Browse column B: %+v", bcols[bi])
	}
	if b, ok := brows[0][bi].AsBool(); !ok || !b {
		t.Errorf("Browse B row 1: %s", brows[0][bi])
	}
	if b, ok := brows[1][bi].AsBool(); !ok || b {
		t.Errorf("Browse B row 2: %s", brows[1][bi])
	}
}

// Parameters of every supported type round-trip.
func TestParams(t *testing.T) {
	c := openOra(t, false)
	ts := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	rows, _, _ := collect(t, c, driver.Query{
		Text:   "SELECT :1, :2, :3, :4, :5, :6, CAST(:7 AS TIMESTAMP WITH TIME ZONE), :8 FROM dual",
		Params: []driver.Value{driver.Int(5), driver.Float(1.5), driver.Decimal("12.50"), driver.String("x"), driver.Bytes([]byte{7}), driver.Null(), driver.Time(ts), driver.Bool(true)},
	})
	got := rows[0]
	for i, w := range []driver.Value{driver.Int(5), driver.Float(1.5), driver.Decimal("12.5"), driver.String("x"), driver.Bytes([]byte{7}), driver.Null(), driver.Time(ts), driver.Int(1)} {
		if !drivertest.EqualValue(w, got[i]) {
			t.Errorf("param %d: got %s (%s), want %s", i+1, got[i], got[i].Type(), w)
		}
	}
	if err := queryErr(t, c, driver.Query{Text: "SELECT :1 FROM dual", Params: []driver.Value{driver.List()}}); err == nil {
		t.Error("a List parameter: want an error")
	}
}

// LOB values over maxLOBBytes are cut and the result says so.
func TestLOBCap(t *testing.T) {
	db := admin(t)
	mustExec(t, db, "CREATE TABLE tobira_lobs (id NUMBER(10) PRIMARY KEY, c CLOB, b BLOB)")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE tobira_lobs PURGE") })
	mustExec(t, db, `DECLARE c CLOB; b BLOB; BEGIN
	  DBMS_LOB.CREATETEMPORARY(c, TRUE); DBMS_LOB.CREATETEMPORARY(b, TRUE);
	  FOR i IN 1..1100 LOOP
	    DBMS_LOB.APPEND(c, RPAD('x', 1000, 'x'));
	    DBMS_LOB.APPEND(b, UTL_RAW.CAST_TO_RAW(RPAD('y', 1000, 'y')));
	  END LOOP;
	  INSERT INTO tobira_lobs VALUES (1, c, b); COMMIT; END;`)
	settle(t, db, "tobira_lobs")
	c := openOra(t, true)
	rows, st, _ := collect(t, c, driver.Query{Text: "SELECT c, b FROM tobira_lobs"})
	s, _ := rows[0][0].AsText()
	b, _ := rows[0][1].AsBytes()
	if len(s) != maxLOBBytes || len(b) != maxLOBBytes {
		t.Errorf("cut lengths: clob %d, blob %d, want %d", len(s), len(b), maxLOBBytes)
	}
	if len(st.Warnings) != 2 || !strings.Contains(st.Warnings[0], "were cut") {
		t.Errorf("warnings: %q", st.Warnings)
	}
}
