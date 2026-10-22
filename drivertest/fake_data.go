package drivertest

import (
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// table is one in-memory collection. Values are immutable, so rows are copied
// by cloning the slice.
type table struct {
	cols []driver.Column
	rows []driver.Row
	key  []string
	// ctype is what Collections and Describe report; "" means a table.
	ctype driver.CollectionType
	// schemaless marks a document collection (AddDocuments): a result lists
	// only the columns that hold a value in one of its rows.
	schemaless bool
	// partition and clustering are a wide-column table's keys
	// (addWideColumns), reported by Describe.
	partition  []string
	clustering []driver.SortKey
}

func (t *table) colIndex(name string) int {
	return slices.IndexFunc(t.cols, func(c driver.Column) bool { return c.Name == name })
}

func (t *table) clone() *table {
	n := &table{
		cols: slices.Clone(t.cols), key: slices.Clone(t.key), rows: make([]driver.Row, len(t.rows)), ctype: t.ctype, schemaless: t.schemaless,
		partition: slices.Clone(t.partition), clustering: slices.Clone(t.clustering),
	}
	for i, r := range t.rows {
		n.rows[i] = slices.Clone(r)
	}
	return n
}

// columns returns the result columns for this table, with Source filled in.
func (t *table) columns(ref driver.CollectionRef) []driver.Column {
	out := slices.Clone(t.cols)
	for i := range out {
		r := ref
		out[i].Source = &r
	}
	return out
}

// collectionType is the type Collections and Describe report.
func (t *table) collectionType() driver.CollectionType {
	if t.ctype == "" {
		return driver.CollectionTable
	}
	return t.ctype
}

// result returns the columns and copied rows of a result over this table.
// For a schemaless table only the columns with a non-NULL value in one of
// rows are kept, in the table's (first-seen) order, so pages of one
// collection can have different columns.
func (t *table) result(ref driver.CollectionRef, rows []driver.Row) ([]driver.Column, []driver.Row) {
	cols := t.columns(ref)
	if !t.schemaless {
		return cols, cloneRows(rows)
	}
	var keep []int
	for i := range cols {
		if slices.ContainsFunc(rows, func(r driver.Row) bool { return !r[i].IsNull() }) {
			keep = append(keep, i)
		}
	}
	out := make([]driver.Column, len(keep))
	for j, i := range keep {
		out[j] = cols[i]
	}
	proj := make([]driver.Row, len(rows))
	for k, r := range rows {
		pr := make(driver.Row, len(keep))
		for j, i := range keep {
			pr[j] = r[i]
		}
		proj[k] = pr
	}
	return out, proj
}

// tables is a dataset: collections in creation order.
type tables struct {
	order []driver.CollectionRef
	m     map[driver.CollectionRef]*table
}

func newTables() *tables { return &tables{m: map[driver.CollectionRef]*table{}} }

func (d *tables) put(ref driver.CollectionRef, t *table) {
	if _, ok := d.m[ref]; !ok {
		d.order = append(d.order, ref)
	}
	d.m[ref] = t
}

// clone deep-copies the dataset (the copy-on-write snapshot used by Tx).
func (d *tables) clone() *tables {
	n := &tables{order: slices.Clone(d.order), m: make(map[driver.CollectionRef]*table, len(d.m))}
	for ref, t := range d.m {
		n.m[ref] = t.clone()
	}
	return n
}

func (d *tables) namespaces() []string {
	var out []string
	for _, ref := range d.order {
		if !slices.Contains(out, ref.Namespace) {
			out = append(out, ref.Namespace)
		}
	}
	return out
}

// defaultNamespace is "main" when present, else the first namespace, else "main".
func (d *tables) defaultNamespace() string {
	nss := d.namespaces()
	if len(nss) == 0 || slices.Contains(nss, "main") {
		return "main"
	}
	return nss[0]
}

// resolve fills in the default namespace and looks the collection up.
func (d *tables) resolve(op string, ref driver.CollectionRef) (driver.CollectionRef, *table, error) {
	if ref.Namespace == "" {
		ref.Namespace = d.defaultNamespace()
	}
	t, ok := d.m[ref]
	if !ok {
		return ref, nil, fakeErr(driver.ErrNotFound, op, "collection %s.%s not found", ref.Namespace, ref.Name)
	}
	return ref, t, nil
}

// findByName returns the first collection called name in any namespace.
func (d *tables) findByName(name string) (driver.CollectionRef, *table, bool) {
	for _, ref := range d.order {
		if ref.Name == name {
			return ref, d.m[ref], true
		}
	}
	return driver.CollectionRef{}, nil, false
}

func cloneRows(rows []driver.Row) []driver.Row {
	out := make([]driver.Row, len(rows))
	for i, r := range rows {
		out[i] = slices.Clone(r)
	}
	return out
}

// numeric converts a numeric (or bool) Value to a rational.
func numeric(v driver.Value) (*big.Rat, bool) {
	switch v.Type() {
	case driver.TypeInt:
		n, _ := v.AsInt()
		return new(big.Rat).SetInt64(n), true
	case driver.TypeBool:
		b, _ := v.AsBool()
		if b {
			return big.NewRat(1, 1), true
		}
		return new(big.Rat), true
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		r := new(big.Rat)
		if r.SetFloat64(f) == nil {
			return nil, false
		}
		return r, true
	case driver.TypeDecimal:
		s, _ := v.AsText()
		return new(big.Rat).SetString(s)
	}
	return nil, false
}

// compareValues orders two non-null Values: numbers numerically, times by
// instant, everything else by display text.
func compareValues(a, b driver.Value) int {
	if x, ok := numeric(a); ok {
		if y, ok := numeric(b); ok {
			return x.Cmp(y)
		}
	}
	if x, ok := a.AsTime(); ok {
		if y, ok := b.AsTime(); ok {
			return x.Compare(y)
		}
	}
	return strings.Compare(a.String(), b.String())
}

// compareNullsHigh is compareValues with NULL greater than everything, so
// ascending sorts put nulls last and descending sorts put them first.
func compareNullsHigh(a, b driver.Value) int {
	switch {
	case a.IsNull() && b.IsNull():
		return 0
	case a.IsNull():
		return 1
	case b.IsNull():
		return -1
	}
	return compareValues(a, b)
}

// idText renders a graph node id: ints as decimals, text as-is.
func idText(v driver.Value) string {
	if s, ok := v.AsText(); ok {
		return s
	}
	return v.String()
}

func itoa(n int) string { return strconv.Itoa(n) }
