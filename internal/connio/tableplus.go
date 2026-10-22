package connio

import (
	"errors"
	"fmt"
	"strings"

	"howett.net/plist"

	"github.com/xavidop/tobira/internal/config"
)

// TablePlusPath is where TablePlus keeps its connections on macOS, relative
// to the home directory.
const TablePlusPath = "Library/Application Support/com.tinyapp.TablePlus/Data/Connections.plist"

// tablePlusDriver maps a TablePlus Driver value to a tobira driver, or "".
// The format is undocumented, so the match ignores case, spaces, '-' and
// '_', and takes the spellings TablePlus is known to use or likely uses
// ("SQLServer", "Microsoft SQL Server", "Cloudflare D1", "Amazon Redshift").
func tablePlusDriver(d string) string {
	key := strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '_' {
			return -1
		}
		return r
	}, strings.ToLower(d))
	switch key {
	case "postgresql", "postgres":
		return "postgres"
	case "mysql":
		return "mysql"
	case "mariadb":
		return "mariadb"
	case "singlestore", "memsql":
		return "singlestore"
	case "sqlite":
		return "sqlite"
	case "mongodb", "mongo":
		return "mongodb"
	case "clickhouse":
		return "clickhouse"
	case "redis":
		return "redis"
	case "sqlserver", "mssql", "microsoftsqlserver":
		return "mssql"
	case "oracle":
		return "oracle"
	case "cassandra", "scylladb":
		return "cassandra"
	case "bigquery", "googlebigquery":
		return "bigquery"
	case "dynamodb", "amazondynamodb":
		return "dynamodb"
	case "libsql", "turso":
		return "libsql"
	case "cloudflared1", "d1":
		return "d1"
	case "snowflake":
		return "snowflake"
	case "duckdb":
		return "duckdb"
	case "vertica":
		return "vertica"
	case "redshift", "amazonredshift":
		return "redshift"
	case "cockroachdb", "cockroach":
		return "cockroachdb"
	case "greenplum":
		return "greenplum"
	case "elasticsearch":
		return "elasticsearch"
	}
	return ""
}

// ParseTablePlus reads TablePlus's Connections.plist (XML or binary), best
// effort: the format is not documented. TablePlus keeps passwords in the
// macOS keychain, which is never read, and any password key in the plist is
// ignored too, so no candidate has a password.
func ParseTablePlus(data []byte) ([]Candidate, error) {
	var v any
	if _, err := plist.Unmarshal(data, &v); err != nil {
		return nil, errors.New("not a property list")
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("not a TablePlus connections list (want an array of connections)")
	}
	var out []Candidate
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			out = append(out, Candidate{Label: fmt.Sprintf("entry %d", i+1), Skip: "malformed entry"})
			continue
		}
		out = append(out, tablePlusCandidate(m, i+1))
	}
	return out, nil
}

func tablePlusCandidate(m map[string]any, i int) Candidate {
	str := func(k string) string {
		switch v := m[k].(type) {
		case string:
			return v
		case uint64, int64, float64:
			return fmt.Sprint(v)
		}
		return ""
	}
	name := str("ConnectionName")
	c := Candidate{Label: name}
	if c.Label == "" {
		c.Label = fmt.Sprintf("entry %d", i)
	}
	drv := tablePlusDriver(str("Driver"))
	if drv == "" {
		c.Skip = fmt.Sprintf("TablePlus driver %q has no tobira driver", str("Driver"))
		return c
	}
	db := str("DatabaseName")
	if drv == "sqlite" || drv == "duckdb" {
		db = str("DatabasePath")
	}
	dsn, err := buildDSN(drv, str("DatabaseHost"), str("DatabasePort"), db, str("DatabaseUser"))
	if err != nil {
		c.Skip = err.Error()
		return c
	}
	if name == "" {
		name = drv
	}
	c.Conn = config.Connection{Name: name, Driver: drv, DSN: dsn}
	return c
}
