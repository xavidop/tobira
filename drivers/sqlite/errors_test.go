package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		msg  string
		want error
	}{
		{"SQL logic error: no such table: x (1)", driver.ErrNotFound},
		{"no such column: y", driver.ErrNotFound},
		{"attempt to write a readonly database (8)", driver.ErrReadOnly},
		{"UNIQUE constraint failed: t.id (1555)", driver.ErrConstraint},
		{`near "selec": syntax error (1)`, driver.ErrSyntax},
		{"database is locked (5)", driver.ErrConflict},
		{"not authorized (23)", driver.ErrPermission},
		{"unable to open database file: out of memory (14)", driver.ErrConnection},
	}
	for _, tt := range tests {
		err := mapError("query", errors.New(tt.msg))
		if !errors.Is(err, tt.want) {
			t.Errorf("%q: kind = %v, want %v", tt.msg, driver.KindOf(err), tt.want)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != "sqlite" || de.Op != "query" {
			t.Errorf("%q: bad wrap %#v", tt.msg, err)
		}
	}
	if mapError("query", nil) != nil {
		t.Error("nil must stay nil")
	}
	if err := mapError("query", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx error lost: %v", err)
	}
}

func TestErrorCodeFromDriver(t *testing.T) {
	c := openTemp(t, false)
	_, err := c.Query(context.Background(), driver.Query{Text: "select * from nope"})
	var de *driver.Error
	if !errors.As(err, &de) || !errors.Is(err, driver.ErrNotFound) || de.Code != "1" {
		t.Fatalf("got %#v", err)
	}
	_, err = c.Query(context.Background(), driver.Query{Text: "insert into tobira_people(id,name,email) values (1,'a','a@x')"})
	if !errors.Is(err, driver.ErrConstraint) {
		t.Fatalf("got %v", err)
	}
	if errors.As(err, &de); de.Code != "1555" {
		t.Errorf("code = %q", de.Code)
	}
}
