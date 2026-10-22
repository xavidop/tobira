// Command embedding shows tobira's driver layer used as a library inside
// another Go program: no TUI, no CLI, no config file. It builds a private
// registry with the drivers it wants, opens a connection read-only, lists
// the collections and streams one query's rows.
//
//	go run .                                 # a demo SQLite database in a temp dir
//	go run . -dsn sqlite:./app.db -query 'select * from users' -limit 20
//
// The program imports only github.com/xavidop/tobira/api (the stable,
// stdlib-only surface for embedders and plugins) and the driver modules it
// links in, here drivers/sqlite. Any other driver module, or your own
// (see ../custom-driver-module), is added to the registry the same way.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/xavidop/tobira/api"
	"github.com/xavidop/tobira/drivers/sqlite"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run parses args, opens the connection and writes its collections and
// the query's rows to w.
func run(ctx context.Context, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("embedding", flag.ContinueOnError)
	fs.SetOutput(w)
	dsn := fs.String("dsn", "", "connection DSN (default: a demo SQLite database in a temp dir)")
	text := fs.String("query", "select id, name, email from users order by id", "query to run")
	limit := fs.Int("limit", 100, "maximum rows to print; 0 means no limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// A private registry, holding only the drivers this program adds. The
	// process-wide registry that driver modules also join from init belongs
	// to the tobira binary; an embedder keeps its own.
	reg := api.NewRegistry()
	if err := reg.Add(sqlite.New()); err != nil {
		return err
	}
	if *dsn == "" {
		dir, err := os.MkdirTemp("", "tobira-embedding-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		*dsn = "sqlite:" + filepath.Join(dir, "demo.db")
		if err := seed(ctx, reg, *dsn); err != nil {
			return err
		}
	}
	return explore(ctx, w, reg, *dsn, *text, *limit)
}

// explore opens dsn read-only, prints every collection, then the rows of
// text. Errors are redacted, since a DSN may carry a password.
func explore(ctx context.Context, w io.Writer, reg *api.Registry, dsn, text string, limit int) error {
	cfg := api.ConnConfig{DSN: dsn, ReadOnly: true}
	conn, err := reg.Open(ctx, cfg)
	if err != nil {
		return errors.New(api.RedactMessage(err.Error(), cfg))
	}
	defer func() { _ = conn.Close() }()

	namespaces, err := conn.Namespaces(ctx)
	if err != nil {
		return err
	}
	for _, ns := range namespaces {
		cols, err := conn.Collections(ctx, api.NamespaceRef{Name: ns.Name})
		if err != nil {
			return err
		}
		for _, c := range cols {
			_, _ = fmt.Fprintf(w, "%s %s.%s\n", c.Type, c.Ref.Namespace, c.Ref.Name)
		}
	}

	// ReadOnly on the query too: the driver refuses anything that writes,
	// with an error that matches api.ErrReadOnly.
	rs, err := conn.Query(ctx, api.Query{Text: text, Limit: limit, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("query (%s): %s", api.KindName(err), api.RedactMessage(err.Error(), cfg))
	}
	defer func() { _ = rs.Close() }()
	names := make([]string, len(rs.Columns()))
	for i, c := range rs.Columns() {
		names[i] = c.Name
	}
	_, _ = fmt.Fprintln(w, strings.Join(names, "\t"))
	// Rows stream one at a time; api.Collect would gather them instead.
	n := 0
	for {
		row, ok, err := rs.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = v.String()
		}
		_, _ = fmt.Fprintln(w, strings.Join(cells, "\t"))
		n++
	}
	more := ""
	if rs.Stats().Truncated {
		more = ", truncated by -limit"
	}
	_, _ = fmt.Fprintf(w, "(%d rows%s)\n", n, more)
	return nil
}

// seed creates the demo users table through a writable connection.
func seed(ctx context.Context, reg *api.Registry, dsn string) error {
	conn, err := reg.Open(ctx, api.ConnConfig{DSN: dsn})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	for _, q := range []api.Query{
		{Text: "create table users (id integer primary key, name text not null, email text)"},
		{Text: "insert into users (name, email) values (?, ?), (?, ?), (?, ?)", Params: []api.Value{
			api.String("Ada"), api.String("ada@example.com"),
			api.String("Grace"), api.String("grace@example.com"),
			api.String("Linus"), api.Null(),
		}},
	} {
		rs, err := conn.Query(ctx, q)
		if err != nil {
			return err
		}
		if _, _, err := api.Collect(ctx, rs, 0); err != nil {
			return err
		}
	}
	return nil
}
