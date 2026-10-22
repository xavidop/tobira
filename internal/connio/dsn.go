package connio

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// StripDSNPassword removes the password from dsn; see driver.StripPassword.
// had reports whether dsn carried one; a "" result with had true means it
// could not be removed safely and the DSN must not be used.
func StripDSNPassword(dsn string) (stripped string, had bool) {
	return driver.StripPassword(dsn)
}

// schemes is the DSN scheme each importable driver gets: every built-in
// driver's (and the duckdb plugin's) main scheme, so an import produces a
// DSN the driver's registry entry recognises.
var schemes = map[string]string{
	"postgres": "postgres", "mysql": "mysql", "mongodb": "mongodb",
	"clickhouse": "clickhouse", "redis": "redis",
	"mariadb": "mariadb", "singlestore": "singlestore",
	"redshift": "redshift", "cockroachdb": "cockroachdb", "greenplum": "greenplum",
	"mssql": "sqlserver", "oracle": "oracle", "cassandra": "cassandra", "vertica": "vertica",
	"snowflake": "snowflake", "bigquery": "bigquery", "dynamodb": "dynamodb",
	"elasticsearch": "elasticsearch", "opensearch": "opensearch",
	"libsql": "libsql", "d1": "d1", "duckdb": "duckdb", "firestore": "firestore",
	"cosmos": "cosmos", "pinecone": "pinecone", "weaviate": "weaviate", "chroma": "chroma",
	"milvus": "milvus",
}

// awsRegionHost matches a DynamoDB endpoint host, capturing its region.
var awsRegionHost = regexp.MustCompile(`^dynamodb\.([a-z]{2}(?:-[a-z]+)+-\d+)\.amazonaws\.com$`)

// buildDSN makes a password-free DSN for drv from its parts. sqlite and
// duckdb take db as the file path, bigquery as the project, mssql as the
// database= parameter; libsql, elasticsearch and opensearch drop it (their
// DSNs have no database path); snowflake takes the account from host (without
// .snowflakecomputing.com) and dynamodb the region from its endpoint host.
// A driver whose DSN needs more than these parts (d1's account and
// database ids) is refused with a reason.
func buildDSN(drv, host, port, db, user string) (string, error) {
	switch drv {
	case "sqlite":
		if db == "" {
			return "", errors.New("no database file")
		}
		return "sqlite:" + db, nil
	case "duckdb":
		if db == "" {
			return "", errors.New("no database file")
		}
		return "duckdb://" + db, nil
	case "bigquery":
		if db == "" || strings.ContainsAny(db, "/?#@ ") {
			return "", errors.New("no BigQuery project (the database field)")
		}
		return "bigquery://" + db, nil
	case "dynamodb":
		m := awsRegionHost.FindStringSubmatch(strings.ToLower(host))
		if m == nil {
			return "", errors.New("no AWS region: a DynamoDB host must be dynamodb.<region>.amazonaws.com; add it with tobira connections add")
		}
		return "dynamodb://" + m[1], nil
	case "d1":
		return "", errors.New("a Cloudflare D1 connection needs the account and database ids, which the import does not carry; add it with tobira connections add")
	case "snowflake":
		host = strings.TrimSuffix(strings.ToLower(host), ".snowflakecomputing.com")
		port = ""
	case "libsql":
		db = "" // a libsql server has no database path
	case "elasticsearch", "opensearch":
		db = "" // the driver refuses a path: indexes are named per query
	}
	if host == "" {
		return "", errors.New("no host")
	}
	if port != "" && strings.Trim(port, "0123456789") != "" {
		return "", errors.New("port is not a number")
	}
	u := url.URL{Scheme: schemes[drv]}
	if user != "" {
		u.User = url.User(user)
	}
	switch {
	case drv == "postgres" && strings.HasPrefix(host, "/"):
		// A Unix socket directory goes in the host parameter.
		v := url.Values{"host": {host}}
		if port != "" {
			v.Set("port", port)
		}
		u.RawQuery = v.Encode()
	case port != "":
		u.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		u.Host = "[" + host + "]"
	default:
		u.Host = host
	}
	if db != "" {
		switch {
		case drv == "redis" && strings.Trim(db, "0123456789") != "":
			return "", errors.New("redis database is not a number")
		case drv == "mssql":
			u.RawQuery = url.Values{"database": {db}}.Encode()
		default:
			u.Path = "/" + db
		}
	}
	return u.String(), nil
}
