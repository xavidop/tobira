package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// flavor names a PostgreSQL-compatible database the driver adapts to. The
// wire protocol is the same; the catalog, the features and the SQL accepted
// differ. "" means "not chosen yet": Open asks the server.
type flavor string

const (
	flavorPostgres  flavor = "postgres"
	flavorRedshift  flavor = "redshift"
	flavorCockroach flavor = "cockroachdb"
	flavorGreenplum flavor = "greenplum"
)

// flavorParam is the DSN parameter that forces a flavor. tobira strips it
// before pgx parses the DSN, so it never reaches the server.
const flavorParam = "flavor"

// flavorSchemes are the DSN schemes that imply a flavor; they are rewritten
// to postgres:// before pgx parses the DSN.
var flavorSchemes = map[string]flavor{
	"redshift":    flavorRedshift,
	"cockroachdb": flavorCockroach,
	"cockroach":   flavorCockroach,
	"greenplum":   flavorGreenplum,
}

// parseFlavor reads a flavor name, case-insensitively, with a few aliases.
func parseFlavor(s string) (flavor, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "postgres", "postgresql":
		return flavorPostgres, nil
	case "redshift":
		return flavorRedshift, nil
	case "cockroachdb", "cockroach", "crdb":
		return flavorCockroach, nil
	case "greenplum", "gpdb":
		return flavorGreenplum, nil
	}
	return "", driver.Wrap(driver.ErrUnsupported, Name, "open",
		fmt.Errorf("unknown flavor %q (have postgres, redshift, cockroachdb, greenplum)", s))
}

// detectFlavor names the flavor from select version() and, for CockroachDB,
// the crdb_version setting (nil or empty when the server has none).
func detectFlavor(version string, crdbVersion *string) flavor {
	switch {
	case strings.Contains(version, "CockroachDB"), crdbVersion != nil && *crdbVersion != "":
		return flavorCockroach
	case strings.Contains(version, "Redshift"):
		return flavorRedshift
	case strings.Contains(version, "Greenplum"):
		return flavorGreenplum
	}
	return flavorPostgres
}

// detectServerFlavor asks the server what it is. A failing version() fails
// Open; crdb_version is only a fallback for a version() string that names
// none of the flavors, and a server that cannot answer it is PostgreSQL.
func detectServerFlavor(ctx context.Context, c *conn) (flavor, error) {
	var version string
	if err := c.pool.QueryRow(ctx, "select version()").Scan(&version); err != nil {
		return "", err
	}
	if f := detectFlavor(version, nil); f != flavorPostgres {
		return f, nil
	}
	var crdb *string
	if err := c.pool.QueryRow(ctx, "select current_setting('crdb_version', true)").Scan(&crdb); err != nil {
		return flavorPostgres, nil //nolint:nilerr // a server without the setting is not CockroachDB
	}
	return detectFlavor(version, crdb), nil
}

// splitFlavor removes the flavor parameter from dsn and rewrites a flavored
// scheme to postgres://. It returns the flavor the DSN or the driver (forced)
// asks for, or "" when nothing does. Two sources that disagree, a repeated
// or empty parameter and an unknown name are ErrUnsupported.
func splitFlavor(dsn string, forced flavor) (string, flavor, error) {
	fail := func(err error) (string, flavor, error) {
		return "", "", driver.Wrap(driver.ErrUnsupported, Name, "open", err)
	}
	var wants []flavor
	if forced != "" {
		wants = append(wants, forced)
	}
	sc := driver.Scheme(dsn)
	isURL := sc != "" && strings.HasPrefix(dsn[len(sc):], "://")
	if f, ok := flavorSchemes[sc]; ok && isURL {
		dsn = "postgres" + dsn[len(sc):]
		wants = append(wants, f)
	}
	var values []string
	if isURL {
		dsn, values = stripURLParam(dsn)
	} else {
		dsn, values = stripKeywordParam(dsn)
	}
	if len(values) > 1 {
		return fail(errors.New("the flavor parameter is given more than once"))
	}
	for _, v := range values {
		f, err := parseFlavor(v)
		if err != nil {
			return "", "", err
		}
		wants = append(wants, f)
	}
	if len(wants) == 0 {
		return dsn, "", nil
	}
	for _, f := range wants[1:] {
		if f != wants[0] {
			return fail(fmt.Errorf("conflicting flavors %q and %q", wants[0], f))
		}
	}
	return dsn, wants[0], nil
}

// stripURLParam removes every flavor=… pair from the query of a URL DSN,
// read the way net/url and pgx read it: the query runs from the first '?'
// to the first '#' after it, and the fragment is kept as it is. A password
// with an unescaped '?' or '#' is not a URL pgx accepts anyway; such a DSN
// keeps its text and fails to parse, without reaching the server.
func stripURLParam(dsn string) (string, []string) {
	q := strings.IndexByte(dsn, '?')
	if q < 0 {
		return dsn, nil
	}
	if h := strings.IndexByte(dsn, '#'); h >= 0 && h < q {
		return dsn, nil // the '?' is inside the fragment
	}
	head, query, frag := dsn[:q], dsn[q+1:], ""
	if h := strings.IndexByte(query, '#'); h >= 0 {
		query, frag = query[:h], query[h:]
	}
	var kept, values []string
	for pair := range strings.SplitSeq(query, "&") {
		k, v, _ := strings.Cut(pair, "=")
		if key, err := url.QueryUnescape(k); err == nil && strings.EqualFold(key, flavorParam) {
			if val, err := url.QueryUnescape(v); err == nil {
				v = val
			}
			values = append(values, v)
			continue
		}
		kept = append(kept, pair)
	}
	if values == nil {
		return dsn, nil
	}
	if len(kept) == 0 {
		return head + frag, values
	}
	return head + "?" + strings.Join(kept, "&") + frag, values
}

// keywordValue matches one key = value pair of a libpq keyword/value DSN,
// with an optional single-quoted value.
var keywordValue = regexp.MustCompile(`([A-Za-z_]+)\s*=\s*('(?:[^'\\]|\\.)*'|\S*)`)

// stripKeywordParam removes every flavor=… pair from a keyword/value DSN.
// Pairs are matched whole, so a password whose quoted value contains
// "flavor=" is left alone.
func stripKeywordParam(dsn string) (string, []string) {
	var values []string
	var sb strings.Builder
	last := 0
	for _, m := range keywordValue.FindAllStringSubmatchIndex(dsn, -1) {
		if !strings.EqualFold(dsn[m[2]:m[3]], flavorParam) {
			continue
		}
		v := dsn[m[4]:m[5]]
		if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = strings.ReplaceAll(v[1:len(v)-1], `\'`, `'`)
		}
		values = append(values, v)
		sb.WriteString(dsn[last:m[0]])
		last = m[1]
		for last < len(dsn) && strings.ContainsRune(" \t\r\n", rune(dsn[last])) {
			last++ // the separator after the pair goes with it
		}
	}
	if values == nil {
		return dsn, nil
	}
	sb.WriteString(dsn[last:])
	return strings.TrimSpace(sb.String()), values
}

// allCapabilities is what the PostgreSQL flavor offers.
var allCapabilities = []driver.Capability{
	driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor, driver.CapCompleter,
	driver.CapExporter, driver.CapWatcher, driver.CapVectorSearcher, driver.CapReadOnly,
}

// lacks lists what each flavor cannot do:
//   - CockroachDB has no LISTEN/NOTIFY and no pgvector extension.
//   - Redshift has no LISTEN/NOTIFY, no pgvector, no COPY TO STDOUT, no
//     savepoints and no RETURNING, so neither Mutator nor Transactor (both
//     rely on savepoints) nor Exporter.
var lacks = map[flavor][]driver.Capability{
	flavorCockroach: {driver.CapWatcher, driver.CapVectorSearcher},
	flavorRedshift:  {driver.CapMutator, driver.CapTransactor, driver.CapExporter, driver.CapWatcher, driver.CapVectorSearcher},
}

// capabilities returns what f offers, in allCapabilities order.
func (f flavor) capabilities() []driver.Capability {
	out := make([]driver.Capability, 0, len(allCapabilities))
	for _, cp := range allCapabilities {
		if f.has(cp) {
			out = append(out, cp)
		}
	}
	return out
}

func (f flavor) has(cp driver.Capability) bool { return !slices.Contains(lacks[f], cp) }

// unsupported is the refusal of a capability f lacks, for calls made without
// driver.As.
func (f flavor) unsupported(op string, cp driver.Capability) error {
	return driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("%s is not available on %s", cp, f))
}

// startupReadOnly reports whether a read-only connection sends
// default_transaction_read_only=on in the startup packet. Redshift does not
// list that setting, so it relies on BEGIN READ ONLY and the lexical guard.
func (f flavor) startupReadOnly() bool { return f != flavorRedshift }

// resetStatements reset a connection returned to the pool. Redshift has no
// DISCARD; RESET ALL is the closest it has.
func (f flavor) resetStatements() []string {
	if f == flavorRedshift {
		return []string{"RESET ALL"}
	}
	return []string{"DISCARD ALL"}
}

// declareCursor is the DECLARE prefix of the server-side cursor. Redshift's
// DECLARE takes no NO SCROLL.
func (f flavor) declareCursor() string {
	if f == flavorRedshift {
		return "DECLARE " + cursorName + " CURSOR FOR "
	}
	return "DECLARE " + cursorName + " NO SCROLL CURSOR FOR "
}

// savepoints reports whether the flavor has SAVEPOINT.
func (f flavor) savepoints() bool { return f != flavorRedshift }

// textExplain reports whether the flavor's EXPLAIN only speaks text: no
// (FORMAT JSON) option.
func (f flavor) textExplain() bool { return f == flavorCockroach || f == flavorRedshift }

// textExplainPrefix is the EXPLAIN prefix of a text-only flavor. Redshift has
// no EXPLAIN ANALYZE.
func (f flavor) textExplainPrefix(analyze bool) (string, error) {
	if !analyze {
		return "EXPLAIN ", nil
	}
	if f == flavorRedshift {
		return "", driver.Wrap(driver.ErrUnsupported, Name, "explain", errors.New("EXPLAIN ANALYZE is not available on redshift"))
	}
	return "EXPLAIN ANALYZE ", nil
}
