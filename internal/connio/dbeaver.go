package connio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
)

// dbeaverConn is the part of a DBeaver data-sources.json connection that
// import reads.
type dbeaverConn struct {
	Provider      string `json:"provider"`
	Driver        string `json:"driver"`
	Name          string `json:"name"`
	Configuration struct {
		Host     flexString `json:"host"`
		Port     flexString `json:"port"`
		Database string     `json:"database"`
		URL      string     `json:"url"`
		User     string     `json:"user"`
		Password string     `json:"password"`
	} `json:"configuration"`
}

// flexString is a JSON string or number, as DBeaver writes ports either way.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("want a string or a number")
	}
	*f = flexString(n.String())
	return nil
}

// dbeaverDriver maps a DBeaver provider id (and, for the generic and
// postgresql providers, the driver id) to a tobira driver, or "". The
// provider ids are DBeaver's plugin ids; the ones only DBeaver PRO has
// (cassandra, dynamodb, elasticsearch) are mapped too.
func dbeaverDriver(provider, driverID string) string {
	id := strings.ToLower(driverID)
	switch p := strings.ToLower(provider); p {
	case "postgresql":
		switch {
		case strings.Contains(id, "redshift"):
			return "redshift"
		case strings.Contains(id, "cockroach"):
			return "cockroachdb"
		case strings.Contains(id, "greenplum"):
			return "greenplum"
		}
		return "postgres"
	case "mysql":
		if strings.Contains(id, "singlestore") || strings.Contains(id, "memsql") {
			return "singlestore"
		}
		if strings.Contains(id, "mariadb") {
			return "mariadb"
		}
		return "mysql"
	case "mariadb", "singlestore", "sqlite", "mongodb", "clickhouse", "redis", "oracle", "cassandra",
		"bigquery", "dynamodb", "elasticsearch", "opensearch", "libsql", "snowflake", "duckdb", "vertica",
		"redshift", "greenplum":
		return p
	case "sqlserver", "mssql":
		return "mssql"
	case "cockroachdb", "cockroach":
		return "cockroachdb"
	case "generic":
		switch {
		case strings.Contains(id, "sqlite"):
			return "sqlite"
		case strings.Contains(id, "duckdb"):
			return "duckdb"
		}
	}
	return ""
}

// ParseDBeaver reads DBeaver's data-sources.json, in file order. The
// password is read only from a plain configuration.password; the encrypted
// credentials-config.json is never read. A connection whose provider has
// no tobira driver is skipped with the reason.
func ParseDBeaver(data []byte) ([]Candidate, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			if strings.Contains(se.Error(), "max depth") {
				return nil, errors.New("not a DBeaver data-sources.json: JSON too deeply nested")
			}
			return nil, fmt.Errorf("not valid JSON (byte %d)", se.Offset)
		}
		return nil, errors.New("not a DBeaver data-sources.json (want a JSON object)")
	}
	raw, ok := top["connections"]
	if !ok {
		return nil, errors.New("no connections object (is this DBeaver's data-sources.json?)")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("connections is not an object")
	}
	var out []Candidate
	for i := 1; dec.More(); i++ {
		// The document already parsed as a whole, so these reads fail only
		// on something the first pass could not see.
		if _, err := dec.Token(); err != nil { // the connection id
			return nil, fmt.Errorf("malformed connections object at entry %d", i)
		}
		var dc dbeaverConn
		if err := dec.Decode(&dc); err != nil {
			var se *json.SyntaxError
			if errors.As(err, &se) {
				return nil, fmt.Errorf("malformed connections object at entry %d", i)
			}
			out = append(out, Candidate{Label: fmt.Sprintf("entry %d", i), Skip: "malformed entry"})
			continue
		}
		out = append(out, dbeaverCandidate(dc, i))
	}
	return out, nil
}

// hostless are the drivers whose DSN needs no host from the connection.
var hostless = map[string]bool{"bigquery": true}

func dbeaverCandidate(dc dbeaverConn, i int) Candidate {
	c := Candidate{Label: dc.Name}
	if c.Label == "" {
		c.Label = fmt.Sprintf("entry %d", i)
	}
	drv := dbeaverDriver(dc.Provider, dc.Driver)
	if drv == "" {
		c.Skip = fmt.Sprintf("DBeaver provider %q has no tobira driver", dc.Provider)
		return c
	}
	cfg := dc.Configuration
	host, port, db, user := string(cfg.Host), string(cfg.Port), cfg.Database, cfg.User
	if cfg.Password != "" {
		c.Password = driver.NewSecret(cfg.Password)
	}
	fromURL, params := false, false
	fileDB := drv == "sqlite" || drv == "duckdb"
	if (fileDB && db == "") || (!fileDB && host == "" && !hostless[drv]) {
		j, ok := parseJDBC(cfg.URL, drv)
		if !ok {
			c.Skip = "no host or database, and no JDBC URL tobira understands"
			return c
		}
		host, port, db = j.host, j.port, j.db
		if user == "" {
			user = j.user
		}
		if c.Password.IsZero() && j.password != "" {
			c.Password = driver.NewSecret(j.password)
			fromURL = true
		}
		params = j.params
	} else if c.Password.IsZero() {
		// The URL is not used, but a password only it holds is still
		// offered rather than silently lost.
		if j, ok := parseJDBC(cfg.URL, drv); ok && j.password != "" {
			c.Password = driver.NewSecret(j.password)
			fromURL = true
		}
	}
	if fromURL {
		c.Notes = append(c.Notes, "password taken from the JDBC URL")
	}
	if params {
		c.Notes = append(c.Notes, "other JDBC URL parameters were not imported")
	}
	dsn, err := buildDSN(drv, host, port, db, user)
	if err != nil {
		c.Skip = err.Error()
		return c
	}
	c.Conn = config.Connection{Name: dc.Name, Driver: drv, DSN: dsn}
	if c.Conn.Name == "" {
		c.Conn.Name = drv
	}
	return c
}

type jdbcParts struct {
	host, port, db, user, password string
	// params is whether the URL had query parameters besides user and
	// password, which are dropped.
	params bool
}

// parseJDBC reads a jdbc:<scheme>:... URL for drv.
func parseJDBC(raw, drv string) (jdbcParts, bool) {
	rest, ok := strings.CutPrefix(raw, "jdbc:")
	if !ok {
		return jdbcParts{}, false
	}
	if drv == "sqlite" || drv == "duckdb" {
		path, ok := strings.CutPrefix(rest, drv+":")
		return jdbcParts{db: path}, ok && path != ""
	}
	u, err := url.Parse(rest)
	if err != nil || u.Host == "" {
		return jdbcParts{}, false
	}
	p := jdbcParts{host: u.Hostname(), port: u.Port(), db: strings.TrimPrefix(u.Path, "/")}
	if u.User != nil {
		p.user = u.User.Username()
		p.password, _ = u.User.Password()
	}
	q := u.Query()
	if p.user == "" {
		p.user = q.Get("user")
	}
	if p.password == "" {
		p.password = q.Get("password")
	}
	q.Del("user")
	q.Del("password")
	p.params = len(q) > 0
	return p, true
}
