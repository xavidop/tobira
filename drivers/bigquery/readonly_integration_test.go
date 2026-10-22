//go:build integration

package bigquery

import (
	"context"
	"errors"
	"strings"
	"testing"

	bq "cloud.google.com/go/bigquery"

	"github.com/xavidop/tobira/driver"
)

// readOnlyProbes are writes in every form GoogleSQL offers. Each must be
// refused with ErrReadOnly on a read-only connection and with
// Query.ReadOnly, and change nothing.
var readOnlyProbes = []string{
	// DML
	"DELETE FROM tobira_people WHERE id = 1",
	"delete tobira_people where true",
	"INSERT INTO tobira_people (id) VALUES (1000)",
	"INSERT tobira_people (id) SELECT 1001",
	"UPDATE tobira_people SET name = 'x' WHERE true",
	"MERGE tobira_people t USING (SELECT 1 AS id) s ON t.id = s.id WHEN MATCHED THEN DELETE",
	"TRUNCATE TABLE tobira_people",
	// DDL
	"CREATE TABLE tobira_probe (a INT64)",
	"CREATE OR REPLACE TABLE tobira_people AS SELECT 1 AS id",
	"CREATE TEMP TABLE probe AS SELECT 1 AS a",
	"CREATE VIEW tobira_probe_view AS SELECT 1 AS a",
	"CREATE SCHEMA tobira_probe_ds",
	"DROP TABLE tobira_people",
	"DROP SCHEMA tobira CASCADE",
	"ALTER TABLE tobira_people ADD COLUMN probe INT64",
	"ALTER TABLE tobira_people RENAME TO tobira_gone",
	"UNDROP SCHEMA tobira_probe_ds",
	// scripting, procedures and transactions
	"BEGIN DELETE FROM tobira_people WHERE true; END",
	"BEGIN TRANSACTION; DELETE FROM tobira_people WHERE true; COMMIT TRANSACTION",
	"DECLARE x INT64 DEFAULT 1; DELETE FROM tobira_people WHERE id = x",
	"SET @@dataset_id = 'tobira'",
	"EXECUTE IMMEDIATE 'DELETE FROM tobira_people WHERE true'",
	"CALL tobira.probe()",
	"ASSERT (SELECT COUNT(*) FROM tobira_people) = 0",
	"RAISE USING MESSAGE = 'x'",
	// several statements, however they are hidden
	"SELECT 1; DELETE FROM tobira_people WHERE true",
	"SELECT 1 /* ; */ ; DELETE FROM tobira_people WHERE true",
	"SELECT 1 --x\r; DELETE FROM tobira_people WHERE true",
	"SELECT 1 #x\r; DELETE FROM tobira_people WHERE true",
	"SELECT '\\''; DELETE FROM tobira_people WHERE true; SELECT '",
	"WITH x AS (SELECT 1) DELETE FROM tobira_people WHERE true",
	"(DELETE FROM tobira_people WHERE true)",
	// data movement and other engines
	"EXPORT DATA OPTIONS(uri='gs://bucket/*.csv', format='CSV') AS SELECT * FROM tobira_people",
	"LOAD DATA INTO tobira_people FROM FILES(format='CSV', uris=['gs://bucket/x.csv'])",
	"SELECT * FROM EXTERNAL_QUERY('conn', 'DELETE FROM x')",
	"SELECT * FROM `EXTERNAL_QUERY`('conn', 'DELETE FROM x')",
	"SELECT * FROM `region-us.external_query`('conn', 'DELETE FROM x')",
	"SELECT * FROM `region-us`.`External_Query`('conn', 'DELETE FROM x')",
	"SELECT * FROM `\\x45XTERNAL_QUERY`('conn', 'DELETE FROM x')",
	"GRANT `roles/bigquery.dataViewer` ON TABLE tobira_people TO 'user:a@example.com'",
	"REVOKE `roles/bigquery.dataViewer` ON TABLE tobira_people FROM 'user:a@example.com'",
	// other dialects and syntaxes
	"#legacySQL\nSELECT * FROM [tobira:tobira.tobira_people]",
	"FROM tobira_people |> WHERE true",
	// text the lexer cannot read
	"SELECT 'unterminated",
	"SELECT 1 /* unterminated",
	"SELECT 1\x00; DELETE FROM tobira_people WHERE true",
}

func TestReadOnlyRefusesEveryWrite(t *testing.T) {
	ro := open(t, "", true)
	rw := open(t, "", false)
	for _, text := range readOnlyProbes {
		for name, run := range map[string]func() error{
			"conn": func() error { _, err := ro.Query(ctxT(t), driver.Query{Text: text}); return err },
			"query": func() error {
				_, err := rw.Query(ctxT(t), driver.Query{Text: text, ReadOnly: true})
				return err
			},
		} {
			if err := run(); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("%s read-only %q: want ErrReadOnly, got %v", name, text, err)
			}
		}
	}
	if n := peopleCount(t, testDataset); n != 57 {
		t.Errorf("People has %d rows after the refused writes, want 57", n)
	}
	c, err := adminClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	md, err := c.Dataset(testDataset).Table("tobira_people").Metadata(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(md.Schema) != len(peopleSchema()) {
		t.Errorf("People has %d columns after the refused writes, want %d", len(md.Schema), len(peopleSchema()))
	}
	for _, name := range []string{"tobira_probe", "tobira_probe_view", "tobira_gone"} {
		if _, err := c.Dataset(testDataset).Table(name).Metadata(ctxT(t)); err == nil {
			t.Errorf("table %s exists after the refused writes", name)
		}
	}
}

// The emulator's dry run is not faithful: it reports statementType SELECT
// for every statement, so against it the dry-run layer of the guard
// refuses nothing and the lexical layer does all the work (the
// TestReadOnlyRefusesEveryWrite probes pass on the lexical layer alone).
// The dry-run layer is proven against the statement types real BigQuery
// returns in TestDryRunLayerRefusesNonSelect. This test pins the gap: when
// it fails, the emulator has started reporting real statement types, and
// the emulator note on the BigQuery site page should be updated.
func TestEmulatorDryRunReportsSelectForWrites(t *testing.T) {
	c, err := adminClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	q := c.Query("DELETE FROM `" + testDataset + ".tobira_people` WHERE true")
	q.DryRun = true
	job, err := q.Run(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	qs, _ := job.LastStatus().Statistics.Details.(*bq.QueryStatistics)
	if qs == nil || qs.StatementType != "SELECT" {
		t.Errorf("emulator dry run of a DELETE: statementType = %+v; the emulator gap noted on the BigQuery site page may be fixed", qs)
	}
	if n := peopleCount(t, testDataset); n != 57 {
		t.Errorf("the emulator's dry run deleted rows: %d left", n)
	}
}

// The lexer reads the tricky forms as ZetaSQL (the emulator's parser)
// does: each text below passes the guard as one SELECT, and the server
// runs it as exactly that one statement.
func TestLexerAgreesWithServer(t *testing.T) {
	ro := open(t, "", true)
	for _, tc := range []struct{ text, want string }{
		{"SELECT r'\\'; SELECT 2; --' AS s", `\'; SELECT 2; --`},
		{"SELECT 'a' --x\r || 'b' AS s", "ab"},
		{"SELECT 'a' #x\r || 'b' AS s", "ab"},
		{"SELECT '''it's ; DELETE''' AS s", "it's ; DELETE"},
		{`SELECT """say "hi"; DROP""" AS s`, `say "hi"; DROP`},
		{"SELECT 'x\\'; DELETE FROM t' AS s", "x'; DELETE FROM t"},
		{"SELECT `update`.s FROM (SELECT 'u' AS s) AS `update`", "u"},
		{"SELECT /* ; DELETE */ '#' AS s", "#"},
	} {
		if err := checkReadOnly(tc.text); err != nil {
			t.Errorf("guard refused %q: %v", tc.text, err)
			continue
		}
		_, rows, _ := queryAll(t, ro, driver.Query{Text: tc.text})
		if len(rows) != 1 || len(rows[0]) != 1 {
			t.Errorf("%q: got %v, want one value", tc.text, rows)
			continue
		}
		// Compared with runs of spaces collapsed: the emulator returns
		// "it's ;  DELETE" for the triple-quoted case (it rewrites the
		// text around ';'), which is its quirk, not a second statement.
		if s, _ := rows[0][0].AsText(); strings.Join(strings.Fields(s), " ") != tc.want {
			t.Errorf("%q: got %q, want %q", tc.text, s, tc.want)
		}
	}
}

func TestReadOnlyBrowseAndExplain(t *testing.T) {
	ro := open(t, "", true)
	b, _ := driver.As[driver.Browser](ro)
	ref := driver.CollectionRef{Namespace: testDataset, Name: "tobira_people"}
	for _, raw := range []string{
		"true); DELETE FROM tobira_people WHERE (true",
		"true) OR (true",
		"id = 1; DELETE FROM tobira_people WHERE true",
		"'unterminated",
	} {
		rs, err := b.Browse(ctxT(t), ref, driver.BrowseOptions{Limit: 5, Filter: driver.Filter{Raw: raw}})
		if err == nil {
			_ = rs.Close()
			t.Errorf("Browse with raw filter %q succeeded", raw)
		}
	}
	rs, err := b.Browse(ctxT(t), ref, driver.BrowseOptions{Limit: 100, Filter: driver.Filter{Raw: "id IN (SELECT 3) -- trailing"}})
	if err != nil {
		t.Fatalf("Browse with a read raw filter: %v", err)
	}
	rows, _, _ := driver.Collect(ctxT(t), rs, 0)
	if len(rows) != 1 {
		t.Errorf("raw filter id IN (SELECT 3): %d rows", len(rows))
	}

	rw := open(t, "", false)
	x, _ := driver.As[driver.Explainer](rw)
	if _, err := x.Explain(ctxT(t), driver.Query{Text: "DELETE FROM tobira_people WHERE true", ReadOnly: true}, driver.ExplainOptions{}); !errors.Is(err, driver.ErrReadOnly) {
		t.Errorf("read-only explain of a DELETE: want ErrReadOnly, got %v", err)
	}
	// On a writable connection, explaining a DELETE is a dry run: it never
	// deletes.
	if _, err := x.Explain(ctxT(t), driver.Query{Text: "DELETE FROM tobira_people WHERE true"}, driver.ExplainOptions{}); err != nil {
		t.Errorf("explain of a DELETE: %v", err)
	}
	if n := peopleCount(t, testDataset); n != 57 {
		t.Errorf("explaining a DELETE deleted rows: %d left", n)
	}
}
