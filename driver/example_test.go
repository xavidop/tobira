package driver_test

import (
	"context"
	"fmt"

	"github.com/xavidop/tobira/driver"
)

func ExampleOpen() {
	reg := driver.NewRegistry()
	_ = reg.Add(newFake("example", "example"))

	ctx := context.Background()
	conn, err := reg.Open(ctx, driver.ConnConfig{DSN: "example://localhost/db"})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = conn.Close() }()

	rs, err := conn.Query(ctx, driver.Query{Text: "select 1"})
	if err != nil {
		fmt.Println(err)
		return
	}
	rows, _, err := driver.Collect(ctx, rs, 0)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(rows[0][0])
	// Output: 1
}
