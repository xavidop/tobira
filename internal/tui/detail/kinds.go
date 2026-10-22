package detail

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/theme"
)

// maxExpandLines bounds the lines expand-all opens: levels open shallowest
// first until the next container would pass it.
const maxExpandLines = 5000

// maxAlign caps the label width a field/value table pads to.
const maxAlign = 24

// collapseAll closes every container and puts the cursor on the root of
// the node it was on.
func (m *Model) collapseAll() {
	sel := m.selected()
	var walk func(n *tnode)
	walk = func(n *tnode) {
		n.expanded = false
		for _, c := range n.children {
			walk(c)
		}
	}
	for _, r := range m.roots {
		walk(r)
	}
	m.flatten()
	for sel != nil && sel.parent != nil {
		sel = sel.parent
	}
	if sel != nil {
		m.selectNode(sel)
	}
}

// expandAll opens containers level by level, shallowest first, and stops
// before the lines shown would pass maxExpandLines, so a huge tree stays
// cheap. The cursor stays on its node.
func (m *Model) expandAll() {
	sel := m.selected()
	count := len(m.lines) // what is open already counts
	queue := slices.Clone(m.roots)
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if !n.container {
			continue
		}
		kids := n.children[:min(len(n.children), maxShown)]
		if n.expanded {
			queue = append(queue, kids...) // its lines are in count
			continue
		}
		add := len(kids)
		if len(n.children) > maxShown || n.hidden > 0 {
			add++ // the "… N more" line
		}
		if count+add > maxExpandLines {
			break
		}
		n.expanded = true
		count += add
		queue = append(queue, kids...)
	}
	m.flatten()
	if sel != nil {
		m.selectNode(sel)
	}
}

// WithAbsent marks the columns flagged in absent as fields the row did not
// have: they read as a dim "—" rather than NULL.
func (m Model) WithAbsent(absent []bool) Model {
	changed := false
	for i, a := range absent {
		if a && i < len(m.roots) && i < len(m.cols) {
			name := m.cols[i].Name
			m.roots[i] = &tnode{label: name, dlabel: boundedText(name), text: "—", absent: true, fromVal: true}
			m.pending = slices.DeleteFunc(m.pending, func(p int) bool { return p == i })
			changed = true
		}
	}
	if changed {
		m.flatten()
	}
	return m
}

// Column names of a key-value row: the key, its type, its TTL and its
// value.
var (
	kvTypeCols  = []string{"type"}
	kvTTLCols   = []string{"ttl"}
	kvValueCols = []string{"value"}
)

// colIndex is the index of the first column named one of names (any case),
// or -1.
func colIndex(cols []driver.Column, names []string) int {
	return slices.IndexFunc(cols, func(c driver.Column) bool {
		return slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(c.Name, n) })
	})
}

// NewKeyValue builds the detail of a key-value row (columns key, type, ttl
// and value, as a key-value driver lists them): the TTL in words and the
// value laid out by its type, open:
//   - string: its text, one line per line;
//   - hash: a field/value table;
//   - list: indexed lines;
//   - set: its members, sorted;
//   - zset: member and score;
//   - stream: each entry's id over its fields;
//   - json and anything else: the tree.
//
// A row without the type and value columns gets the plain detail.
func NewKeyValue(cols []driver.Column, row driver.Row, th theme.Theme, km keymap.Map) Model {
	ti, vi := colIndex(cols, kvTypeCols), colIndex(cols, kvValueCols)
	if ti < 0 || vi < 0 || ti >= len(row) || vi >= len(row) || isHuge(row[vi]) {
		return New(cols, row, th, km)
	}
	md := New(cols, row, th, km)
	typ, _ := row[ti].AsText()
	md.roots[vi] = kvValue(row[vi], cols[vi].Name, strings.ToLower(typ))
	if i := colIndex(cols, kvTTLCols); i >= 0 && i < len(row) {
		if s, ok := TTLText(row[i]); ok {
			md.roots[i].text = s
		}
	}
	md.flatten()
	return md
}

// kvValue is the open root node of a key-value value of type typ.
func kvValue(v driver.Value, label, typ string) *tnode {
	root := &tnode{label: label, dlabel: boundedText(label), val: v, fromVal: true}
	leaf := func(v driver.Value, label string, parent *tnode) *tnode {
		n := buildValue(v, label, parent, parent.depth+1, false)
		return n
	}
	bare := func(text string, parent *tnode) *tnode {
		return &tnode{parent: parent, depth: parent.depth + 1, text: boundedText(text), bare: true,
			val: driver.String(text), fromVal: true}
	}
	open := func(n *tnode, kids []*tnode, sum string) *tnode {
		n.container, n.expanded, n.children, n.sum = true, true, kids, sum
		return n
	}
	switch {
	case typ == "string" && v.Type() == driver.TypeString:
		s, _ := v.AsText()
		if !strings.Contains(s, "\n") {
			root.text = boundedText(s)
			return root
		}
		// Only the lines the view can show are built: past maxShown the
		// rest is counted, not split into nodes.
		total := strings.Count(s, "\n") + 1
		lines := strings.SplitN(s, "\n", maxShown+2)
		if len(lines) > maxShown+1 {
			lines = lines[:maxShown+1]
		}
		kids := make([]*tnode, len(lines))
		for i, l := range lines {
			kids[i] = bare(l, root)
		}
		root.hidden = total - len(lines)
		return open(root, kids, strconv.Itoa(total)+" lines")
	case typ == "hash" && v.Type() == driver.TypeObject:
		obj, _ := v.AsObject()
		root.isObject = true
		kids := make([]*tnode, len(obj))
		for i, mem := range obj {
			kids[i] = leaf(mem.Value, mem.Key, root)
		}
		return align(open(root, kids, ""))
	case (typ == "list" || typ == "set") && v.Type() == driver.TypeList:
		list, _ := v.AsList()
		kids := make([]*tnode, len(list))
		for i, e := range list {
			if typ == "set" {
				kids[i] = bare(e.String(), root)
			} else {
				kids[i] = leaf(e, "["+strconv.Itoa(i)+"]", root)
			}
		}
		if typ == "set" {
			slices.SortStableFunc(kids, func(a, b *tnode) int { return cmp.Compare(a.text, b.text) })
			return open(root, kids, "")
		}
		return align(open(root, kids, ""))
	case typ == "zset" && v.Type() == driver.TypeList:
		list, _ := v.AsList()
		kids := make([]*tnode, len(list))
		for i, e := range list {
			obj, _ := e.AsObject()
			member, _ := obj.Get("member")
			score, _ := obj.Get("score")
			kids[i] = leaf(score, member.String(), root)
		}
		return align(open(root, kids, ""))
	case typ == "stream" && v.Type() == driver.TypeList:
		list, _ := v.AsList()
		kids := make([]*tnode, len(list))
		for i, e := range list {
			obj, _ := e.AsObject()
			id, _ := obj.Get("id")
			entry := &tnode{label: id.String(), dlabel: boundedText(id.String()), parent: root, depth: 1,
				val: e, fromVal: true, isObject: true}
			fields, ok := obj.Get("fields")
			fo, _ := fields.AsObject()
			if !ok {
				for _, mem := range obj {
					if mem.Key != "id" {
						fo = append(fo, mem)
					}
				}
			}
			fk := make([]*tnode, len(fo))
			for j, mem := range fo {
				fk[j] = leaf(mem.Value, mem.Key, entry)
			}
			kids[i] = align(open(entry, fk, ""))
		}
		sum := strconv.Itoa(len(list)) + " entries"
		if len(list) == 1 {
			sum = "1 entry"
		}
		return open(root, kids, sum)
	}
	n := buildValue(v, label, nil, 0, false)
	n.expanded = n.container
	return n
}

// align pads the labels of n's children to one width, so they read as a
// table.
func align(n *tnode) *tnode {
	w := 0
	for _, c := range n.children {
		w = max(w, query.DisplayWidth(c.dlabel))
	}
	n.alignW = min(w, maxAlign)
	return n
}

// TTLText is a time to live in seconds (as a key-value driver reports it)
// in words: ∞ for no expiry (-1), "expired" for a key gone (-2), else the
// two largest units: 12s, 1m30s, 3h, 2d1h.
func TTLText(v driver.Value) (string, bool) {
	var secs float64
	switch v.Type() {
	case driver.TypeInt:
		n, _ := v.AsInt()
		secs = float64(n)
	case driver.TypeFloat, driver.TypeDecimal:
		f, ok := v.AsFloat()
		if !ok {
			var err error
			s, _ := v.AsText()
			if f, err = strconv.ParseFloat(s, 64); err != nil {
				return "", false
			}
		}
		secs = f
	default:
		return "", false
	}
	switch {
	case math.IsNaN(secs):
		return "", false
	case secs == -2:
		return "expired", true
	case secs < 0, secs > math.MaxInt64/1e9:
		return "∞", true
	}
	n := int64(secs)
	units := []struct {
		size int64
		name string
	}{{86400, "d"}, {3600, "h"}, {60, "m"}, {1, "s"}}
	var parts []string
	for _, u := range units {
		if q := n / u.size; q > 0 && len(parts) < 2 {
			parts = append(parts, strconv.FormatInt(q, 10)+u.name)
			n -= q * u.size
		} else if len(parts) > 0 {
			break
		}
	}
	if len(parts) == 0 {
		return "0s", true
	}
	return strings.Join(parts, ""), true
}
