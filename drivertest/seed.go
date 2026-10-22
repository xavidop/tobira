package drivertest

import (
	"fmt"
	"strings"
	"time"
)

const seedBatch = 20

type seedDialect struct {
	create   string
	boolLit  func(bool) string
	timeLit  func(time.Time) string
	escapeBS bool // backslash is an escape character in string literals
}

var seedDialects = map[string]seedDialect{
	"postgres": {
		create: "CREATE TABLE tobira_people (id integer PRIMARY KEY, name text NOT NULL, email text NOT NULL UNIQUE, " +
			"age integer, score numeric(10,2), active boolean, created_at timestamptz, tags jsonb)",
		boolLit: func(b bool) string {
			if b {
				return "TRUE"
			}
			return "FALSE"
		},
		timeLit: func(t time.Time) string { return "TIMESTAMPTZ '" + t.UTC().Format("2006-01-02 15:04:05") + "+00'" },
	},
	"sqlite": {
		create: "CREATE TABLE tobira_people (id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT NOT NULL UNIQUE, " +
			"age INTEGER, score DECIMAL(10,2), active BOOLEAN, created_at DATETIME, tags JSON)",
		boolLit: func(b bool) string {
			if b {
				return "1"
			}
			return "0"
		},
		timeLit: func(t time.Time) string { return "'" + t.UTC().Format("2006-01-02 15:04:05") + "'" },
	},
	"mysql": {
		create: "CREATE TABLE tobira_people (id INT PRIMARY KEY, name VARCHAR(255) NOT NULL, email VARCHAR(255) NOT NULL UNIQUE, " +
			"age INT, score DECIMAL(10,2), active TINYINT(1), created_at DATETIME, tags JSON)",
		boolLit: func(b bool) string {
			if b {
				return "1"
			}
			return "0"
		},
		timeLit:  func(t time.Time) string { return "'" + t.UTC().Format("2006-01-02 15:04:05") + "'" },
		escapeBS: true,
	},
}

// SQLSeed renders the CREATE TABLE and multi-row INSERT statements that load
// People into a fresh database. Supported dialects are "sqlite", "postgres"
// and "mysql"; any other dialect panics, since that is a test-programming
// error. No DROP is emitted: callers seed fresh databases.
func SQLSeed(dialect string) []string {
	d, ok := seedDialects[dialect]
	if !ok {
		panic(fmt.Sprintf("drivertest: unknown SQL dialect %q", dialect))
	}
	quote := func(s string) string {
		if d.escapeBS {
			s = strings.ReplaceAll(s, `\`, `\\`)
		}
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	stmts := []string{d.create}
	rows := People.Rows
	for start := 0; start < len(rows); start += seedBatch {
		end := min(start+seedBatch, len(rows))
		var sb strings.Builder
		sb.WriteString("INSERT INTO tobira_people (id, name, email, age, score, active, created_at, tags) VALUES")
		for i, r := range rows[start:end] {
			id, _ := r[0].AsInt()
			name, _ := r[1].AsText()
			email, _ := r[2].AsText()
			age := "NULL"
			if n, ok := r[3].AsInt(); ok {
				age = fmt.Sprint(n)
			}
			score, _ := r[4].AsText()
			active, _ := r[5].AsBool()
			ts, _ := r[6].AsTime()
			tags, _ := r[7].AsJSON()
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "\n(%d, %s, %s, %s, %s, %s, %s, %s)",
				id, quote(name), quote(email), age, score, d.boolLit(active), d.timeLit(ts), quote(string(tags)))
		}
		stmts = append(stmts, sb.String())
	}
	return stmts
}
