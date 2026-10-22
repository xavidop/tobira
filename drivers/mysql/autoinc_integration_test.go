//go:build integration

package mysql

import (
	"fmt"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestDescribeAutoIncrement(t *testing.T) {
	db := admin(t)
	c := openMy(t, false)
	name := fmt.Sprintf("tobira_autoinc_%d", scratchSeq.Add(1))
	mustExec(t, db, "CREATE TABLE "+name+" (id BIGINT AUTO_INCREMENT PRIMARY KEY, p INT, x INT DEFAULT 7)")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE IF EXISTS "+name) })
	sch, err := c.Describe(ctxT(t), driver.CollectionRef{Namespace: "tobira", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"id": true, "p": false, "x": false}
	for _, f := range sch.Fields {
		if f.AutoIncrement != want[f.Name] {
			t.Errorf("field %s: AutoIncrement = %v, want %v", f.Name, f.AutoIncrement, want[f.Name])
		}
	}
}
