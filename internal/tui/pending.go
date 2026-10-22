package tui

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/grid"
)

// ---- pending changes ---------------------------------------------------------

// Change kinds, as they appear in messages ("update row id=3").
const (
	kindUpdate = "update"
	kindInsert = "insert"
	kindDelete = "delete"
)

// editRow is the pending state of one row: a loaded row (identified by its
// key as loaded) with edited fields and maybe a delete mark, or an inserted
// row.
type editRow struct {
	key     driver.Object // the row's key values as loaded; nil for an insert
	id      int           // an insert's number: "+1", "+2", …
	values  driver.Object // changed fields (update) or fields set (insert)
	deleted bool
	seq     int // when the row was first edited (or inserted)
	delSeq  int // when it was marked for deletion
}

// order places the row's change in :apply: a delete when it was marked, an
// update or insert when the row was first edited.
func (r *editRow) order() int {
	if r.deleted {
		return r.delSeq
	}
	return r.seq
}

// pendingSet holds the pending inline edits of the open collection. Loaded
// rows are keyed by their key values as loaded, which do not change with
// paging, sort or filter, so the edits survive all three. It lives on the
// Update goroutine; :apply works on a snapshot (changes).
//
// count is what the status bar shows as "N pending": every edited cell of a
// row that is not marked for deletion, plus one per inserted row and one
// per row marked for deletion.
type pendingSet struct {
	keyFields []string
	rows      map[string]*editRow
	inserts   []*editRow
	nextID    int
	seq       int
}

func newPendingSet(keyFields []string) *pendingSet {
	return &pendingSet{keyFields: slices.Clone(keyFields), rows: map[string]*editRow{}}
}

// change is one write of an :apply, in the order the user made them.
type change struct {
	kind   string
	key    driver.Object // update and delete: the key as loaded
	values driver.Object // update: changes; insert: values
	label  string        // "id=3" or "+2", escaped
	row    *editRow
}

func (p *pendingSet) empty() bool { return p == nil || len(p.rows) == 0 && len(p.inserts) == 0 }

func (p *pendingSet) count() int {
	if p == nil {
		return 0
	}
	n := len(p.inserts)
	for _, r := range p.rows {
		if r.deleted {
			n++
		} else {
			n += len(r.values)
		}
	}
	return n
}

func (p *pendingSet) next() int { p.seq++; return p.seq }

// row returns the pending state of the loaded row with key k, if any.
func (p *pendingSet) row(k driver.Object) *editRow {
	if p == nil {
		return nil
	}
	return p.rows[keyID(k)]
}

// setCell records field = v on the loaded row with key k, whose value as
// loaded is orig. A value equal to orig removes the cell from pending.
func (p *pendingSet) setCell(k driver.Object, field string, orig, v driver.Value) {
	id := keyID(k)
	r := p.rows[id]
	if sameValue(orig, v) {
		if r != nil {
			r.values = without(r.values, field)
			p.dropIfClean(id, r)
		}
		return
	}
	if r == nil {
		r = &editRow{key: slices.Clone(k), seq: p.next()}
		p.rows[id] = r
	}
	r.values = with(r.values, field, v)
}

// toggleDelete marks the loaded row with key k for deletion, or unmarks it.
// A marked row keeps its edits, which come back when it is unmarked.
func (p *pendingSet) toggleDelete(k driver.Object) {
	id := keyID(k)
	r := p.rows[id]
	switch {
	case r == nil:
		n := p.next()
		p.rows[id] = &editRow{key: slices.Clone(k), deleted: true, seq: n, delSeq: n}
	case r.deleted:
		r.deleted = false
		p.dropIfClean(id, r)
	default:
		r.deleted, r.delSeq = true, p.next()
	}
}

func (p *pendingSet) dropIfClean(id string, r *editRow) {
	if !r.deleted && len(r.values) == 0 {
		delete(p.rows, id)
	}
}

// addInsert adds a blank inserted row. Its fields are unset until edited, so
// the database fills them (defaults, auto-increment).
func (p *pendingSet) addInsert() *editRow {
	p.nextID++
	r := &editRow{id: p.nextID, seq: p.next()}
	p.inserts = append(p.inserts, r)
	return r
}

// setInsertCell sets field = v on inserted row r. Later edits of an inserted
// row are merged into its values, so :apply needs no reference to the key
// the insert returns.
func (p *pendingSet) setInsertCell(r *editRow, field string, v driver.Value) {
	r.values = with(r.values, field, v)
}

func (p *pendingSet) removeInsert(r *editRow) {
	p.inserts = slices.DeleteFunc(p.inserts, func(x *editRow) bool { return x == r })
}

// drop removes the row a change came from, once that change is applied.
func (p *pendingSet) drop(r *editRow) {
	if r.key == nil {
		p.removeInsert(r)
		return
	}
	delete(p.rows, keyID(r.key))
}

// changes is the snapshot :apply writes, in the order the rows were first
// changed. A row marked for deletion is only deleted.
func (p *pendingSet) changes() []change {
	var out []change
	for _, r := range p.rows {
		c := change{key: r.key, label: keyLabel(r.key), row: r}
		if r.deleted {
			c.kind = kindDelete
		} else {
			c.kind, c.values = kindUpdate, slices.Clone(r.values)
		}
		out = append(out, c)
	}
	for _, r := range p.inserts {
		out = append(out, change{kind: kindInsert, values: slices.Clone(r.values), label: "+" + strconv.Itoa(r.id), row: r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].row.order() < out[j].row.order() })
	return out
}

// missing names the first inserted row that lacks a field the database
// cannot fill: not nullable, no default, not generated and not
// auto-increment ("row +2 needs:
// name, email"). It is "" when every insert can run.
func (p *pendingSet) missing(sch *driver.CollectionSchema) string {
	for _, r := range p.inserts {
		var need []string
		for _, f := range sch.Fields {
			if f.Nullable || f.Default != "" || f.Generated || f.AutoIncrement {
				continue
			}
			if _, ok := r.values.Get(f.Name); !ok {
				need = append(need, query.EscapeControls(f.Name))
			}
		}
		if len(need) > 0 {
			return fmt.Sprintf("row +%d needs: %s", r.id, strings.Join(need, ", "))
		}
	}
	return ""
}

// insertIndex is the position of r in the pinned block.
func (p *pendingSet) insertIndex(r *editRow) int { return slices.Index(p.inserts, r) }

// pinned is the inserted rows as the grid draws them.
func (p *pendingSet) pinned() []grid.Pinned {
	out := make([]grid.Pinned, len(p.inserts))
	for i, r := range p.inserts {
		out[i] = grid.Pinned{Values: slices.Clone(r.values)}
	}
	return out
}

// keyFrom reads the key fields out of a loaded row with columns cols.
func keyFrom(fields []string, cols []driver.Column, row driver.Row) (driver.Object, bool) {
	k := make(driver.Object, 0, len(fields))
	for _, f := range fields {
		i := slices.IndexFunc(cols, func(c driver.Column) bool { return c.Name == f })
		if i < 0 || i >= len(row) {
			return nil, false
		}
		k = append(k, driver.Member{Key: f, Value: row[i]})
	}
	return k, true
}

// pendingMarks draws the pending edits in the grid.
type pendingMarks struct{ p *pendingSet }

func (m pendingMarks) Row(cols []driver.Column, row driver.Row) grid.RowMark {
	k, ok := keyFrom(m.p.keyFields, cols, row)
	if !ok {
		return grid.RowMark{}
	}
	r := m.p.row(k)
	if r == nil {
		return grid.RowMark{}
	}
	if r.deleted {
		return grid.RowMark{Deleted: true}
	}
	vals, changed := slices.Clone(row), make([]bool, len(cols))
	for i, c := range cols {
		if v, ok := r.values.Get(c.Name); ok && i < len(vals) {
			vals[i], changed[i] = v, true
		}
	}
	return grid.RowMark{Values: vals, Changed: changed}
}

func with(o driver.Object, field string, v driver.Value) driver.Object {
	for i, m := range o {
		if m.Key == field {
			o = slices.Clone(o)
			o[i].Value = v
			return o
		}
	}
	return append(slices.Clone(o), driver.Member{Key: field, Value: v})
}

func without(o driver.Object, field string) driver.Object {
	return slices.DeleteFunc(slices.Clone(o), func(m driver.Member) bool { return m.Key == field })
}

// keyID encodes a key exactly, types included, for map lookups: Int(1) and
// String("1") differ.
func keyID(k driver.Object) string {
	var sb strings.Builder
	for _, m := range k {
		j, _ := m.Value.MarshalJSON() // never fails
		fmt.Fprintf(&sb, "%q:%s:%s;", m.Key, m.Value.Type(), j)
	}
	return sb.String()
}

// keyLabel is how messages name a row: "id=3", escaped.
func keyLabel(k driver.Object) string {
	parts := make([]string, len(k))
	for i, m := range k {
		parts[i] = query.EscapeControls(m.Key + "=" + m.Value.String())
	}
	return strings.Join(parts, ",")
}

// sameValue reports whether a and b are the same value of the same type.
func sameValue(a, b driver.Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	switch a.Type() {
	case driver.TypeTime:
		ta, _ := a.AsTime()
		tb, _ := b.AsTime()
		return ta.Equal(tb)
	case driver.TypeJSON:
		return a.String() == b.String() // compacted
	}
	ja, _ := a.MarshalJSON()
	jb, _ := b.MarshalJSON()
	return bytes.Equal(ja, jb)
}

// ---- parsing cell edits -------------------------------------------------------

// fieldInfo is what the cell editor knows about the edited field.
type fieldInfo struct {
	Name      string
	Type      driver.Type
	Native    string
	Nullable  bool
	Generated bool
}

// multiline reports whether the field is edited as pretty JSON.
func (f fieldInfo) multiline() bool {
	return f.Type == driver.TypeJSON || f.Type == driver.TypeObject || f.Type == driver.TypeList
}

// isUUID reports a UUID column: tobira has no UUID type, the native type
// says so.
func (f fieldInfo) isUUID() bool {
	n := strings.ToLower(f.Native)
	return strings.Contains(n, "uuid") || n == "uniqueidentifier"
}

var (
	decimalRE = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)
	uuidRE    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// timeLayouts are the forms a Time cell accepts, all read as UTC unless
// they carry an offset.
var timeLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02"}

// parseCell turns the edited text back into a value of the field's type.
func parseCell(text string, f fieldInfo) (driver.Value, error) {
	t := strings.TrimSpace(text)
	switch f.Type {
	case driver.TypeInt:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return driver.Value{}, errors.New("not a whole number")
		}
		return driver.Int(n), nil
	case driver.TypeFloat:
		x, err := strconv.ParseFloat(t, 64)
		if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
			return driver.Value{}, errors.New("not a number")
		}
		return driver.Float(x), nil
	case driver.TypeDecimal:
		if !decimalRE.MatchString(t) {
			return driver.Value{}, errors.New("not a number")
		}
		return driver.Decimal(t), nil
	case driver.TypeBool:
		switch strings.ToLower(t) {
		case "true", "t", "1", "yes":
			return driver.Bool(true), nil
		case "false", "f", "0", "no":
			return driver.Bool(false), nil
		}
		return driver.Value{}, errors.New("not a boolean (true/false, t/f, 1/0, yes/no)")
	case driver.TypeTime:
		for _, l := range timeLayouts {
			if tm, err := time.ParseInLocation(l, t, time.UTC); err == nil {
				return driver.Time(tm), nil
			}
		}
		return driver.Value{}, errors.New("not a time (RFC 3339, 2006-01-02 15:04:05 or 2006-01-02)")
	case driver.TypeBytes:
		h, ok := strings.CutPrefix(t, `\x`)
		b, err := hex.DecodeString(h)
		if !ok || err != nil {
			return driver.Value{}, errors.New(`not \x hex (like \xdeadbeef)`)
		}
		return driver.Bytes(b), nil
	case driver.TypeVector:
		var xs []float32
		if err := json.Unmarshal([]byte(t), &xs); err != nil {
			return driver.Value{}, errors.New("not a JSON array of numbers")
		}
		return driver.Vector(xs), nil
	case driver.TypeJSON, driver.TypeObject, driver.TypeList:
		return parseJSONCell(t, f.Type)
	}
	if f.isUUID() {
		if !uuidRE.MatchString(t) {
			return driver.Value{}, errors.New("not a canonical UUID (8-4-4-4-12 hex digits)")
		}
		return driver.String(t), nil
	}
	return driver.String(text), nil // strings, and types tobira cannot model, as typed
}

// parseJSONCell validates JSON text and makes it a JSON, Object or List
// value; objects keep their key order.
func parseJSONCell(t string, typ driver.Type) (driver.Value, error) {
	if !json.Valid([]byte(t)) {
		return driver.Value{}, errors.New("not valid JSON")
	}
	if typ == driver.TypeJSON {
		var buf bytes.Buffer
		_ = json.Compact(&buf, []byte(t)) // valid, so it cannot fail
		return driver.JSON(buf.Bytes()), nil
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	v, err := decodeJSON(dec)
	if err != nil {
		return driver.Value{}, errors.New("not valid JSON")
	}
	switch {
	case typ == driver.TypeObject && v.Type() != driver.TypeObject:
		return driver.Value{}, errors.New("not a JSON object")
	case typ == driver.TypeList && v.Type() != driver.TypeList:
		return driver.Value{}, errors.New("not a JSON array")
	}
	return v, nil
}

// decodeJSON reads one JSON value as a driver Value, keeping object key
// order. Plain int64 numbers become Int, any other number Decimal with its
// exact text.
func decodeJSON(dec *json.Decoder) (driver.Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return driver.Value{}, err
	}
	switch t := tok.(type) {
	case nil:
		return driver.Null(), nil
	case bool:
		return driver.Bool(t), nil
	case string:
		return driver.String(t), nil
	case json.Number:
		// A number keeps its exact text: Int when it is a plain int64,
		// otherwise Decimal, so an untouched element never loses precision.
		if n, err := t.Int64(); err == nil && strconv.FormatInt(n, 10) == t.String() {
			return driver.Int(n), nil
		}
		return driver.Decimal(t.String()), nil
	case json.Delim:
		if t == '[' {
			var vs []driver.Value
			for dec.More() {
				v, err := decodeJSON(dec)
				if err != nil {
					return driver.Value{}, err
				}
				vs = append(vs, v)
			}
			_, err := dec.Token()
			return driver.List(vs...), err
		}
		var ms []driver.Member
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return driver.Value{}, err
			}
			v, err := decodeJSON(dec)
			if err != nil {
				return driver.Value{}, err
			}
			ms = append(ms, driver.Member{Key: k.(string), Value: v})
		}
		_, err := dec.Token()
		return driver.ObjectValue(ms...), err
	}
	return driver.Value{}, io.ErrUnexpectedEOF
}

// editText is the text the cell editor starts with: the full value (the
// grid's text cuts bytes), pretty JSON for JSON, objects and lists, and ""
// for NULL.
func editText(v driver.Value) string {
	switch v.Type() {
	case driver.TypeNull:
		return ""
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return `\x` + hex.EncodeToString(b)
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.Format(time.RFC3339Nano)
	case driver.TypeJSON, driver.TypeObject, driver.TypeList:
		raw, _ := v.MarshalJSON()
		var buf bytes.Buffer
		if json.Indent(&buf, raw, "", "  ") == nil {
			return buf.String()
		}
		return string(raw)
	case driver.TypeString:
		s, _ := v.AsText()
		return s
	}
	return v.String()
}
