//go:build integration

package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivers/mysql"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

// O'Hara holds the quote character, which defeated pairwise scrubbing.
// Under send_rows: never no part of it reaches the model.
func TestMySQLValueWithQuoteIsNotLeaked(t *testing.T) {
	bg := context.Background()
	ctr, err := tcmysql.Run(bg, "mysql:8.4", tcmysql.WithDatabase("tobira"), tcmysql.WithUsername("root"), tcmysql.WithPassword("tobira"))
	if err != nil {
		t.Fatalf("start mysql: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	hostPort, err := ctr.PortEndpoint(bg, "3306/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	dsn := "mysql://root:tobira@" + hostPort + "/tobira"
	c, err := mysql.New().Open(bg, driver.ConnConfig{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"create table people (id int primary key, name text)",
		"insert into people values (1, 'O''Hara')",
		"create table nums (n int)",
	} {
		rs, err := c.Query(bg, driver.Query{Text: s})
		if err != nil {
			t.Fatal(err)
		}
		_ = rs.Close()
	}
	_ = c.Close()

	reg := driver.NewRegistry()
	if err := reg.Add(mysql.New()); err != nil {
		t.Fatal(err)
	}
	mgr := session.New(&config.Config{Connections: []config.Connection{{Name: "my", Driver: "mysql", DSN: dsn}}}, reg, secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	turn := WithEvents(ctx, assistant.Events{ConfirmExecute: func(context.Context, string, string) (bool, error) { return true, nil }})

	never := NewToolset(ToolDeps{Manager: mgr, Policy: RowsNever})
	x, _ := never.Exec(WithExecute(turn), ExecuteInput{Connection: "my", Statement: "insert into nums (n) select name from people"})
	if x.Error != "error: query failed (error 1366)" {
		t.Errorf("execute under never: %q", x.Error)
	}
	q, _ := never.Query(turn, QueryInput{Connection: "my", Query: "select from people where name = 'O''Hara'"})
	if !strings.HasPrefix(q.Error, "invalid query: Error 1064 (42000): You have an error in your SQL syntax") || !strings.HasSuffix(q.Error, "… (error 1064)") {
		t.Errorf("syntax error under never lost its code: %q", q.Error)
	}
	for _, msg := range []string{x.Error, q.Error} {
		if strings.Contains(msg, "Hara") || strings.Contains(msg, "O'H") {
			t.Errorf("under never the error leaks the value: %q", msg)
		}
	}
	always := NewToolset(ToolDeps{Manager: mgr, Policy: RowsAlways})
	a, _ := always.Exec(WithExecute(turn), ExecuteInput{Connection: "my", Statement: "insert into nums (n) select name from people"})
	if !strings.Contains(a.Error, "Hara") {
		t.Errorf("always: want the full error, got %q", a.Error)
	}
}
