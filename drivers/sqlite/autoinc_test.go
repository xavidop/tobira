package sqlite

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestDescribeAutoIncrement(t *testing.T) {
	c := openTemp(t, false)
	ctx := context.Background()
	for _, ddl := range []string{
		"CREATE TABLE ai_alias (id INTEGER PRIMARY KEY, v TEXT)",
		"CREATE TABLE ai_auto (id integer primary key autoincrement, v TEXT)",
		"CREATE TABLE ai_int (id INT PRIMARY KEY, v TEXT)",
		"CREATE TABLE ai_pair (a INTEGER, b INTEGER, PRIMARY KEY (a, b))",
		"CREATE TABLE ai_norowid (id INTEGER PRIMARY KEY, v TEXT) WITHOUT ROWID",
		"CREATE TABLE ai_tablepk (id INTEGER, v TEXT, PRIMARY KEY (id))",
		"CREATE TABLE ai_desc (id INTEGER PRIMARY KEY DESC, v TEXT)",
		"CREATE TABLE ai_tabledesc (id INTEGER, v TEXT, PRIMARY KEY (id DESC))",
		"CREATE TABLE ai_nndesc (id INTEGER NOT NULL PRIMARY KEY DESC, v TEXT)",
		"CREATE TABLE ai_cdesc (id INTEGER CONSTRAINT pk PRIMARY KEY DESC, v TEXT)",
		`CREATE TABLE ai_qdesc ("id" INTEGER NOT NULL /* x */ PRIMARY  KEY   desc, v TEXT)`,
		"CREATE TABLE ai_nn (id INTEGER NOT NULL PRIMARY KEY, v TEXT)",
		"CREATE TABLE ai_asc (id INTEGER CONSTRAINT pk PRIMARY KEY ASC, v TEXT)",
		"CREATE TABLE ai_trap (id INTEGER PRIMARY KEY /* DESC */, v TEXT DEFAULT 'id INTEGER PRIMARY KEY DESC')",
		"CREATE TABLE ai_other (v TEXT PRIMARY KEY DESC, id INTEGER)",
	} {
		if _, err := c.Query(ctx, driver.Query{Text: ddl}); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	for table, want := range map[string]bool{
		"ai_alias": true, "ai_auto": true, "ai_int": false, "ai_pair": false, "ai_norowid": false, "ai_tablepk": true,
		"ai_desc": false, "ai_tabledesc": true,
		"ai_nndesc": false, "ai_cdesc": false, "ai_qdesc": false, "ai_nn": true, "ai_asc": true, "ai_trap": true,
		"ai_other": false,
	} {
		sch, err := c.Describe(ctx, driver.CollectionRef{Namespace: "main", Name: table})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range sch.Fields {
			key := f.Name == "id" || f.Name == "a"
			if got := f.AutoIncrement; got != (want && key) {
				t.Errorf("%s.%s: AutoIncrement = %v, want %v", table, f.Name, got, want && key)
			}
		}
	}
}
