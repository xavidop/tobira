package memdriver

import (
	"context"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

func TestConformance(t *testing.T) {
	open := func(readOnly bool) func(testing.TB) driver.Conn {
		return func(t testing.TB) driver.Conn {
			t.Helper()
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: "memdriver:", ReadOnly: readOnly})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	drivertest.Run(t, drivertest.Config{
		Driver:       New(),
		Open:         open(false),
		OpenReadOnly: open(true),
		Strict:       true,
		Fixture: drivertest.Fixture{
			Namespace:  namespace,
			Collection: drivertest.PeopleCollection(),
			SelectAll:  driver.Query{Text: "select * from tobira_people"},
			Missing:    driver.Query{Text: "select * from nope"},
			Write:      driver.Query{Text: "delete from tobira_people"},
		},
	})
}
