package federate

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// scratchDriver is the registry name of the engine that joins the staged
// rows. It is reached through the registry, as internal/history reaches it,
// so this package needs no database SDK.
const scratchDriver = "sqlite"

// The bounds of one multi-row INSERT into the scratch database. SQLite
// allows 32766 bound variables per statement.
const (
	insertBatchRows = 500
	maxBindVars     = 32000
)

// scratch is the temporary SQLite database of one run: a 0600 file that is
// removed when the run ends. The sqlite driver refuses in-memory databases,
// and a file keeps a large staging off the heap.
type scratch struct {
	conn driver.Conn
	path string
}

func openScratch(ctx context.Context, reg *driver.Registry) (*scratch, error) {
	if reg == nil {
		reg = driver.Default
	}
	if _, ok := reg.Lookup(scratchDriver); !ok {
		return nil, fmt.Errorf("cross-source queries need the sqlite driver: %w", driver.ErrUnsupported)
	}
	f, err := os.CreateTemp("", "tobira-federate-*.db")
	if err != nil {
		return nil, fmt.Errorf("create the scratch database: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		removeScratch(path)
		return nil, fmt.Errorf("create the scratch database: %w", err)
	}
	conn, err := reg.Open(ctx, driver.ConnConfig{Name: "federated", Driver: scratchDriver, DSN: scratchDSN(path)})
	if err != nil {
		removeScratch(path)
		return nil, fmt.Errorf("open the scratch database: %w", err)
	}
	return &scratch{conn: conn, path: path}, nil
}

// close releases the connection and removes the file with its side files.
func (s *scratch) close() {
	_ = s.conn.Close()
	removeScratch(s.path)
}

func removeScratch(path string) {
	for _, p := range []string{path, path + "-journal", path + "-wal", path + "-shm"} {
		_ = os.Remove(p)
	}
}

var dsnEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23", " ", "%20")

// scratchDSN builds a file: DSN for path. The sqlite driver splits a DSN at
// "?" and percent-decodes the path of a file: DSN, so those characters are
// escaped.
func scratchDSN(path string) string { return "file:" + dsnEscaper.Replace(path) }

// load creates the alias table for cols and fills it with rows.
func (s *scratch) load(ctx context.Context, alias string, cols []driver.Column, rows []driver.Row) error {
	names := columnNames(cols)
	types := columnTypes(cols, rows)
	defs := make([]string, len(names))
	for i := range names {
		defs[i] = quoteIdent(names[i]) + " " + types[i]
	}
	if err := s.exec(ctx, "CREATE TABLE "+quoteIdent(alias)+" ("+strings.Join(defs, ", ")+")"); err != nil {
		return err
	}
	ncols := len(names)
	batch := insertBatchRows
	if batch*ncols > maxBindVars {
		batch = max(1, maxBindVars/ncols)
	}
	marks := "(" + strings.TrimSuffix(strings.Repeat("?, ", ncols), ", ") + ")"
	head := "INSERT INTO " + quoteIdent(alias) + " VALUES "
	for start := 0; start < len(rows); start += batch {
		chunk := rows[start:min(start+batch, len(rows))]
		params := make([]driver.Value, 0, len(chunk)*ncols)
		var sb strings.Builder
		sb.WriteString(head)
		for i, r := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(marks)
			for c := range ncols {
				var v driver.Value
				if c < len(r) {
					v = r[c]
				}
				params = append(params, scratchValue(v))
			}
		}
		if err := s.exec(ctx, sb.String(), params...); err != nil {
			return err
		}
	}
	return nil
}

func (s *scratch) exec(ctx context.Context, text string, params ...driver.Value) error {
	rs, err := s.conn.Query(ctx, driver.Query{Text: text, Params: params})
	if err != nil {
		return err
	}
	_, _, err = driver.Collect(ctx, rs, 0)
	return err
}

// columnNames returns one distinct, non-empty name per column: an empty
// name becomes column<n>, and a repeated name (SQLite compares them without
// case) gets a _2, _3 suffix.
func columnNames(cols []driver.Column) []string {
	names := make([]string, len(cols))
	seen := map[string]bool{}
	for i, c := range cols {
		n := c.Name
		if strings.TrimSpace(n) == "" {
			n = "column" + strconv.Itoa(i+1)
		}
		base := n
		for k := 2; seen[strings.ToLower(n)]; k++ {
			n = base + "_" + strconv.Itoa(k)
		}
		seen[strings.ToLower(n)] = true
		names[i] = n
	}
	return names
}

// columnTypes returns the declared SQLite type of each column: from the
// column's own type, else from its first non-null value, else TEXT.
func columnTypes(cols []driver.Column, rows []driver.Row) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		t := c.Type
		if t == driver.TypeNull || t == driver.TypeUnknown {
			for _, r := range rows {
				if i < len(r) && !r[i].IsNull() {
					t = r[i].Type()
					break
				}
			}
		}
		out[i] = sqliteType(t)
	}
	return out
}

// sqliteType maps a driver type to the declared type of a scratch column.
// Decimals are NUMERIC so arithmetic works on them; times, JSON, lists,
// objects and vectors are TEXT (RFC 3339 or JSON text).
func sqliteType(t driver.Type) string {
	switch t {
	case driver.TypeInt, driver.TypeBool:
		return "INTEGER"
	case driver.TypeFloat:
		return "REAL"
	case driver.TypeDecimal:
		return "NUMERIC"
	case driver.TypeBytes:
		return "BLOB"
	}
	return "TEXT"
}

// scratchValue converts a staged value to one the scratch database stores:
// booleans as 0 and 1, decimals as their text (NUMERIC affinity turns it
// into a number), times as RFC 3339 in UTC, and JSON, lists, objects and
// vectors as JSON text. Null, integers, floats, strings and bytes pass
// through.
func scratchValue(v driver.Value) driver.Value {
	switch v.Type() {
	case driver.TypeNull, driver.TypeInt, driver.TypeFloat, driver.TypeString, driver.TypeBytes:
		return v
	case driver.TypeBool:
		if b, _ := v.AsBool(); b {
			return driver.Int(1)
		}
		return driver.Int(0)
	case driver.TypeDecimal:
		s, _ := v.AsText()
		return driver.String(s)
	case driver.TypeTime:
		t, _ := v.AsTime()
		return driver.String(t.UTC().Format(time.RFC3339Nano))
	case driver.TypeJSON:
		b, _ := v.AsJSON()
		return driver.String(string(b))
	case driver.TypeUnknown:
		s, _ := v.Any().(string)
		return driver.String(s)
	}
	// Lists, objects and vectors become JSON text through the Value's own
	// encoder, which keeps object keys in order and spells NaN and Inf.
	b, _ := v.MarshalJSON()
	return driver.String(string(b))
}

// quoteIdent quotes a SQLite identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
