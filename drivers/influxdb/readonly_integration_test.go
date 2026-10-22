//go:build integration

package influxdb

import (
	"context"
	"errors"
	"testing"

	"github.com/InfluxCommunity/influxdb3-go/v2/influxdb3"

	"github.com/xavidop/tobira/driver"
)

// writeProbes are the write families of InfluxDB 3's two query languages,
// plus the compound and hidden forms the guard must catch.
var writeProbes = map[language][]string{
	langSQL: {
		"INSERT INTO cpu (time, host, usage_user) VALUES (now(), 'x', 1.0)",
		"INSERT INTO cpu SELECT * FROM cpu",
		"UPDATE cpu SET usage_user = 0",
		"DELETE FROM cpu",
		"TRUNCATE cpu",
		"DROP TABLE cpu",
		"DROP DATABASE tobira",
		"CREATE TABLE t AS SELECT * FROM cpu",
		"CREATE VIEW v AS SELECT * FROM cpu",
		"CREATE EXTERNAL TABLE t STORED AS CSV LOCATION '/tmp/tobira'",
		"COPY (SELECT * FROM cpu) TO '/tmp/tobira.csv'",
		"ALTER TABLE cpu ADD COLUMN x INT",
		"SET datafusion.execution.batch_size = 1",
		"SELECT * INTO cpu_copy FROM cpu",
		"SELECT 1; DELETE FROM cpu",
		"WITH x AS (SELECT 1) DELETE FROM cpu",
		"WITH d AS (DELETE FROM cpu RETURNING *) SELECT * FROM d",
		"SELECT * FROM (DELETE FROM cpu RETURNING *) d",
		"EXPLAIN ANALYZE DELETE FROM cpu",
		"/* SELECT */ DELETE FROM cpu",
		"SELECT $$ ' $$ INTO cpu_copy, '",
		"SELECT $1$'$1$ FROM (DELETE FROM cpu RETURNING *) d --'",
		"SELECT $1$'$1$ INTO x FROM cpu --'",
		"SELECT $é$'$é$ INTO x FROM cpu --'",
		"SELECT * FROM (\u00a0DELETE FROM cpu RETURNING *) d",
		"SELECT *\u00a0INTO\u00a0x FROM cpu",
		"PREPARE p AS SELECT 1",
	},
	langInfluxQL: {
		"SELECT * INTO cpu_copy FROM cpu",
		"DELETE FROM cpu",
		"DROP MEASUREMENT cpu",
		"DROP SERIES FROM cpu",
		"DROP DATABASE tobira",
		"CREATE DATABASE tobira_new",
		"CREATE RETENTION POLICY rp ON tobira DURATION 1d REPLICATION 1",
		"CREATE CONTINUOUS QUERY cq ON tobira BEGIN SELECT mean(usage_user) INTO m FROM cpu GROUP BY time(1h) END",
		"ALTER RETENTION POLICY autogen ON tobira DURATION 1d",
		"GRANT ALL TO someone",
		"KILL QUERY 1",
		"SELECT 1; DROP MEASUREMENT cpu",
		"SELECT * FROM cpu WHERE host =~ /'/; DROP MEASUREMENT cpu '",
		"SELECT /'/ INTO x FROM cpu WHERE t = 'a' --'",
		"SELECT * FROM cpu GROUP BY /'/ INTO x --'",
		"SELECT count(/'/) FROM cpu WHERE t = 'a'; DROP MEASUREMENT cpu --'",
		"SELECT DISTINCT /'/ INTO x FROM cpu --'",
	},
}

// counts is the number of rows of the tables the probes target.
func counts(t *testing.T, c driver.Conn) [2]int64 {
	t.Helper()
	var out [2]int64
	for i, tb := range []string{"cpu", "tobira_people"} {
		_, rows, _ := query(t, c, driver.Query{Text: "SELECT count(*) FROM " + tb})
		out[i], _ = rows[0][0].AsInt()
	}
	return out
}

// TestReadOnlyProbes sends every write probe on writable and read-only
// connections, with and without Query.ReadOnly: each is ErrReadOnly before
// anything is sent, and the data is unchanged.
func TestReadOnlyProbes(t *testing.T) {
	verify := open(t, driver.ConnConfig{})
	before := counts(t, verify)
	if before != [2]int64{120, 57} {
		t.Fatalf("seeded counts = %v", before)
	}
	for lang, probes := range writeProbes {
		dsn := testDSN("language=" + lang.String())
		for _, ro := range []bool{false, true} {
			c := open(t, driver.ConnConfig{DSN: dsn, ReadOnly: ro})
			for _, text := range probes {
				for _, qro := range []bool{false, true} {
					_, err := c.Query(ctxT(t), driver.Query{Text: text, ReadOnly: qro})
					if !errors.Is(err, driver.ErrReadOnly) {
						t.Errorf("%s (conn read-only %v, Query.ReadOnly %v) %q = %v, want ErrReadOnly", lang, ro, qro, text, err)
					}
				}
			}
			if err := c.Ping(ctxT(t)); err != nil {
				t.Errorf("Ping after the probes: %v", err)
			}
		}
	}
	if after := counts(t, verify); after != before {
		t.Errorf("row counts changed from %v to %v", before, after)
	}
}

// TestServerRefusesWrites sends the write probes straight through the
// client library, bypassing the guard, to show the second layer: the query
// API itself refuses every one of them.
func TestServerRefusesWrites(t *testing.T) {
	client, err := influxdb3.New(influxdb3.ClientConfig{Host: "http://" + testHostPort, Token: testToken, Database: testDB})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	verify := open(t, driver.ConnConfig{})
	before := counts(t, verify)
	for lang, probes := range writeProbes {
		qt := influxdb3.SQL
		if lang == langInfluxQL {
			qt = influxdb3.InfluxQL
		}
		for _, text := range probes {
			err := func() error {
				ctx, cancel := context.WithCancel(ctxT(t))
				defer cancel()
				it, err := client.Query(ctx, text, influxdb3.WithQueryType(qt))
				if err != nil {
					return err
				}
				r := it.Raw()
				defer r.Release()
				for r.Next() {
				}
				return r.Err()
			}()
			if err == nil {
				t.Errorf("%s %q: the server ran it", lang, text)
			}
		}
	}
	if after := counts(t, verify); after != before {
		t.Errorf("row counts changed from %v to %v", before, after)
	}
}
