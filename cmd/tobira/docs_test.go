package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// capabilityWords maps each optional capability to the word the driver tables
// use for it in their "Other capabilities" cell. Browse, Explain, Vector
// search and Time series have columns of their own.
var capabilityWords = map[driver.Capability]string{
	driver.CapMutator:    "editing",
	driver.CapTransactor: "transactions",
	driver.CapCompleter:  "completion",
	driver.CapExporter:   "export",
	driver.CapWatcher:    "watch",
	driver.CapReadOnly:   "read-only guard",
}

// ownColumnWords may not appear in an "Other capabilities" cell: those
// capabilities have columns of their own.
var ownColumnWords = []string{"vector", "similarity", "time series", "time-series"}

// hostedRows maps a table row that is not a driver of its own, by its
// lower-cased Driver cell, to the driver that provides it.
var hostedRows = map[string]string{
	"pgvector (part of postgresql)": "postgres",
}

// pluginRows are the Driver cells, lower-cased, of drivers that ship as
// external plugins, not in the binary.
var pluginRows = map[string]bool{
	"duckdb (plugin)": true,
}

// Column indexes of a drivers table row once tableRows has put its cells in
// this order. A table may have more columns (a "Tested against" column, say);
// they are not checked.
const (
	colDriver = iota
	colKind
	colBrowse
	colExplain
	colVector
	colTimeSeries
	colOther
	numCols
)

// columnNames are the headers of the checked columns, in column order.
var columnNames = [numCols]string{
	"Driver", "Kind", "Browse", "Explain", "Vector search", "Time series", "Other capabilities",
}

// splitRow returns the trimmed cells of a Markdown table row, with link
// markup stripped.
func splitRow(line string) []string {
	link := regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	cells := strings.Split(strings.Trim(line, "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(link.ReplaceAllString(cells[i], "$1"))
	}
	return cells
}

// driverTableRows returns every row of the drivers table in text (the one
// whose header starts with "| Driver"), header included, as all its cells
// with link markup stripped.
func driverTableRows(text string) [][]string {
	var rows [][]string
	inside := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "| Driver") {
			inside = true
		}
		if !inside {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		if cells := splitRow(line); !strings.HasPrefix(cells[0], "---") {
			rows = append(rows, cells)
		}
	}
	return rows
}

// TestDriverTablesAreIdentical checks that the README and the drivers page
// show the same drivers table, cell for cell, once links are stripped.
func TestDriverTablesAreIdentical(t *testing.T) {
	var tables [2][][]string
	for i, file := range []string{"README.md", "site/src/pages/docs/drivers/index.md"} {
		b, err := os.ReadFile("../../" + file)
		if err != nil {
			t.Fatal(err)
		}
		tables[i] = driverTableRows(string(b))
		if len(tables[i]) == 0 {
			t.Fatalf("%s has no drivers table", file)
		}
	}
	readme, site := tables[0], tables[1]
	if len(readme) != len(site) {
		t.Fatalf("README.md has %d table rows, the drivers page %d", len(readme), len(site))
	}
	for i := range readme {
		if !slices.Equal(readme[i], site[i]) {
			t.Errorf("row %d differs:\n README.md:  %q\n drivers page: %q", i, readme[i], site[i])
		}
	}
}

// tableRows returns the checked cells of every row of the Markdown table in
// text whose header starts with "| Driver", in column order, keyed by the
// lower-cased driver name. It fails the test when a checked column is
// missing or a row has the wrong number of cells.
func tableRows(t *testing.T, file, text string) map[string][]string {
	t.Helper()
	rows := map[string][]string{}
	var index [numCols]int
	width := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "| Driver") {
			header := splitRow(line)
			width = len(header)
			for i, name := range columnNames {
				index[i] = slices.Index(header, name)
				if index[i] < 0 {
					t.Fatalf("%s: the drivers table has no %q column", file, name)
				}
			}
			continue
		}
		if width == 0 {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := splitRow(line)
		if strings.HasPrefix(cells[0], "---") {
			continue
		}
		if len(cells) != width {
			t.Errorf("%s: row %q has %d cells, want %d", file, cells[0], len(cells), width)
			continue
		}
		picked := make([]string, numCols)
		for i, at := range index {
			picked[i] = cells[at]
		}
		rows[strings.ToLower(picked[colDriver])] = picked
	}
	return rows
}

// TestDriverTablesMatchTheBinary checks the drivers matrix of the README and
// of the docs site against the drivers this binary registers: every
// registered driver is listed with the capabilities it really has, and
// nothing is listed that is neither registered, a part of a registered
// driver, nor a plugin.
func TestDriverTablesMatchTheBinary(t *testing.T) {
	registered := map[string]driver.Info{}
	for _, info := range driver.Drivers() {
		registered[strings.ToLower(info.Name)] = info
	}
	// Display names of the tables, lower-cased, to driver names.
	display := map[string]string{
		"sqlite": "sqlite", "postgresql": "postgres", "mysql": "mysql",
		"mongodb": "mongodb", "redis": "redis", "clickhouse": "clickhouse",
		"qdrant": "qdrant", "influxdb": "influxdb", "sql server": "mssql", "cassandra": "cassandra", "neo4j": "neo4j", "oracle": "oracle",
		"bigquery": "bigquery", "amazon dynamodb": "dynamodb",
		"libsql / turso": "libsql", "elasticsearch / opensearch (2 drivers)": "elasticsearch",
		"amazon redshift": "redshift", "cockroachdb": "cockroachdb", "greenplum": "greenplum",
		"mariadb": "mariadb", "singlestore": "singlestore",
		"snowflake": "snowflake", "cloud firestore": "firestore", "cloudflare d1": "d1",
		"pinecone": "pinecone", "azure cosmos db": "cosmos", "weaviate": "weaviate", "chroma": "chroma", "vertica": "vertica",
		"milvus": "milvus", "github": "github",
	}

	for _, file := range []string{"README.md", "site/src/pages/docs/drivers/index.md"} {
		b, err := os.ReadFile("../../" + file)
		if err != nil {
			t.Fatal(err)
		}
		rows := tableRows(t, file, string(b))
		if len(rows) == 0 {
			t.Fatalf("%s has no drivers table", file)
		}
		for shown, cells := range rows {
			if host, ok := hostedRows[shown]; ok {
				checkHostedRow(t, file, shown, cells, host, registered)
				continue
			}
			if pluginRows[shown] {
				if _, inBinary := registered[strings.TrimSuffix(shown, " (plugin)")]; inBinary {
					t.Errorf("%s lists %q as a plugin, but the binary registers it", file, shown)
				}
				continue
			}
			name, isDriver := display[shown]
			if _, isRegistered := registered[name]; !isDriver || !isRegistered {
				t.Errorf("%s lists %q, but the binary does not register it", file, shown)
				continue
			}
			if shown == "elasticsearch / opensearch (2 drivers)" {
				// One row for two drivers: the vector cell describes
				// Elasticsearch first and OpenSearch (no kNN) after it.
				cells = append([]string(nil), cells...)
				cells[colVector] = strings.TrimPrefix(cells[colVector], "Elasticsearch: ")
			}
			checkCapabilityCells(t, file, shown, cells, registered[name], false)
		}
		for display, name := range display {
			if _, ok := registered[name]; ok {
				if _, listed := rows[display]; !listed {
					t.Errorf("%s does not list the registered driver %q", file, display)
				}
			}
		}
		for name := range registered {
			found := false
			for _, n := range display {
				found = found || n == name
			}
			found = found || name == "opensearch" // shares the Elasticsearch row
			if !found {
				t.Errorf("driver %q is registered but the test knows no display name for it; add one", name)
			}
		}
	}
}

// capsOf returns the capabilities info declares.
func capsOf(info driver.Info) map[driver.Capability]bool {
	caps := map[driver.Capability]bool{}
	for _, c := range info.Capabilities {
		caps[c] = true
	}
	return caps
}

// checkCapabilityCells checks a row's Browse, Explain, Vector search, Time
// series and Other capabilities cells against info. With subset, the Other
// cell may leave out capabilities info has (a row for part of a driver) but
// may not name one it lacks.
func checkCapabilityCells(t *testing.T, file, shown string, cells []string, info driver.Info, subset bool) {
	t.Helper()
	caps := capsOf(info)
	yesCells := []struct {
		col  int
		name string
		cap  driver.Capability
	}{
		{colBrowse, "Browse", driver.CapBrowser},
		{colExplain, "Explain", driver.CapExplainer},
		{colVector, "Vector search", driver.CapVectorSearcher},
		{colTimeSeries, "Time series", driver.CapTimeSeriesReader},
	}
	for _, c := range yesCells {
		if got := strings.HasPrefix(cells[c.col], "yes"); got != caps[c.cap] {
			t.Errorf("%s: %s %s = %q, but %s capability = %v", file, shown, c.name, cells[c.col], c.cap, caps[c.cap])
		}
	}
	other := cells[colOther]
	for c, word := range capabilityWords {
		got := strings.Contains(other, word)
		if got && !caps[c] || !subset && got != caps[c] {
			t.Errorf("%s: %s other capabilities %q: %q mentioned = %v, capability = %v", file, shown, other, word, got, caps[c])
		}
	}
	for _, word := range ownColumnWords {
		if strings.Contains(strings.ToLower(other), word) {
			t.Errorf("%s: %s other capabilities %q mention %q, which has its own column", file, shown, other, word)
		}
	}
}

// checkHostedRow checks a row for a feature of another driver (pgvector in
// the PostgreSQL driver): that driver is registered, the row's cells match
// its capabilities, and it is listed for the vector search it exists for.
func checkHostedRow(t *testing.T, file, shown string, cells []string, host string, registered map[string]driver.Info) {
	t.Helper()
	info, ok := registered[host]
	if !ok {
		t.Errorf("%s lists %q, but the binary does not register %q", file, shown, host)
		return
	}
	if !strings.HasPrefix(cells[colVector], "yes") {
		t.Errorf("%s: %q Vector search = %q, want yes", file, shown, cells[colVector])
	}
	checkCapabilityCells(t, file, shown, cells, info, true)
}

// countWords are the spelled-out numbers the docs use for the driver count.
var countWords = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight",
	"nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen",
	"nineteen", "twenty", "twenty-one", "twenty-two", "twenty-three", "twenty-four", "twenty-five", "twenty-six", "twenty-seven", "twenty-eight", "twenty-nine", "thirty",
	"thirty-one", "thirty-two", "thirty-three", "thirty-four", "thirty-five", "thirty-six", "thirty-seven", "thirty-eight", "thirty-nine", "forty"}

// TestDocsDriverCountMatchesTheBinary checks every "<N> drivers" claim of the
// README, the docs index and the agent skill against the number of drivers
// this binary registers.
func TestDocsDriverCountMatchesTheBinary(t *testing.T) {
	n := len(driver.Drivers())
	if n >= len(countWords) {
		t.Fatalf("%d drivers: extend countWords", n)
	}
	claim := regexp.MustCompile(`(?i)\b(` + strings.Join(countWords, "|") + `)\]?(?:\([^)]*\))? drivers\b`)
	for _, file := range []string{"README.md", "site/src/pages/docs/index.md", "skills/tobira/SKILL.md"} {
		b, err := os.ReadFile("../../" + file)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, m := range claim.FindAllStringSubmatch(string(b), -1) {
			found++
			if got := strings.ToLower(m[1]); got != countWords[n] {
				t.Errorf("%s says %q drivers; the binary registers %d", file, m[1], n)
			}
		}
		if found == 0 {
			t.Errorf("%s states no driver count (\"%s drivers\")", file, countWords[n])
		}
	}
}

// TestLLMSDescriptionNamesEveryDriver checks that the description llms.txt
// opens with names every driver the binary registers, and the DuckDB plugin.
func TestLLMSDescriptionNamesEveryDriver(t *testing.T) {
	b, err := os.ReadFile("../../site/src/lib/docs-content.ts")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	start := strings.Index(text, "export const LLMS_DESCRIPTION =")
	if start < 0 {
		t.Fatal("docs-content.ts has no LLMS_DESCRIPTION")
	}
	desc := text[start:]
	desc = desc[:strings.Index(desc, ";\n")]
	// Spelling in the description of each driver whose name is not it.
	spelled := map[string]string{
		"postgres": "PostgreSQL", "mssql": "SQL Server", "libsql": "libSQL", "d1": "Cloudflare D1",
		"cosmos": "Cosmos DB", "dynamodb": "DynamoDB", "firestore": "Firestore", "redshift": "Redshift",
		"cockroachdb": "CockroachDB", "bigquery": "BigQuery", "singlestore": "SingleStore", "mariadb": "MariaDB",
	}
	names := []string{"duckdb"}
	for _, info := range driver.Drivers() {
		names = append(names, info.Name)
	}
	for _, name := range names {
		want := name
		if s, ok := spelled[name]; ok {
			want = s
		}
		if !strings.Contains(strings.ToLower(desc), strings.ToLower(want)) {
			t.Errorf("LLMS_DESCRIPTION does not name %q (as %q)", name, want)
		}
	}
}
