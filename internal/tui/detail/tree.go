package detail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/query"
)

const (
	maxDepth    = 64   // containers nested deeper are shown as "…"
	maxShown    = 1000 // children rendered per container; the rest are counted
	maxTextSize = 4096 // bytes of a scalar kept for display
)

type leafKind int

const (
	leafValue  leafKind = iota // text of a driver value, shown as is
	leafString                 // a string inside a container, shown quoted
	leafLit                    // a JSON number, true, false or null
	leafCut                    // a subtree below maxDepth, shown as "…"
)

// tnode is one node of the detail tree: a column, or a member or element of
// an Object, List or JSON value. A tree is built once; only expanded changes.
type tnode struct {
	label    string // raw key, used for copy
	dlabel   string // escaped, bounded label for display
	parent   *tnode
	depth    int
	expanded bool

	container bool
	isObject  bool
	children  []*tnode

	leaf    leafKind
	text    string // display text of a leaf, escaped, cut to maxTextSize
	null    bool   // an SQL NULL
	loading bool   // placeholder for a tree still being built
	plain   string // copy text of a JSON-sourced leaf: raw string, or literal / compact JSON
	val     driver.Value
	fromVal bool // copy through val (a driver value that was not parsed into children)

	// Kind views (kinds.go): a bare line shows its text without a label,
	// sum replaces a container's summary, and alignW pads the children's
	// labels so they read as a table.
	bare   bool
	sum    string
	alignW int
	absent bool // a field the row did not have, shown as a dim dash
	hidden int  // children not built (counted in the "… N more" line)
}

// builder builds trees. A builder with a context stops early, with the
// context's error, once it is cancelled; checks are spread out so they cost
// next to nothing.
type builder struct {
	ctx   context.Context
	steps int
	err   error
}

// alive reports whether building should go on, checking the context every
// 256 nodes.
func (b *builder) alive() bool {
	if b.err != nil {
		return false
	}
	if b.steps++; b.ctx != nil && b.steps&0xff == 0 {
		b.err = b.ctx.Err()
	}
	return b.err == nil
}

// buildValue builds the node for v. A JSON value that parses to an object or
// array becomes a container; anything else is a leaf.
func buildValue(v driver.Value, label string, parent *tnode, depth int, inContainer bool) *tnode {
	n, _ := (&builder{}).build(v, label, parent, depth, inContainer)
	return n
}

func (b *builder) build(v driver.Value, label string, parent *tnode, depth int, inContainer bool) (*tnode, error) {
	n := &tnode{label: label, dlabel: boundedText(label), parent: parent, depth: depth, val: v, fromVal: true}
	switch v.Type() {
	case driver.TypeObject, driver.TypeList:
		if depth >= maxDepth {
			n.leaf, n.text = leafCut, "…"
			return n, nil
		}
		n.container = true
		if obj, ok := v.AsObject(); ok {
			n.isObject = true
			n.children = make([]*tnode, len(obj))
			for i, m := range obj {
				if !b.alive() {
					return nil, b.err
				}
				c, err := b.build(m.Value, m.Key, n, depth+1, true)
				if err != nil {
					return nil, err
				}
				n.children[i] = c
			}
		} else if list, ok := v.AsList(); ok {
			n.children = make([]*tnode, len(list))
			for i, e := range list {
				if !b.alive() {
					return nil, b.err
				}
				c, err := b.build(e, fmt.Sprintf("[%d]", i), n, depth+1, true)
				if err != nil {
					return nil, err
				}
				n.children[i] = c
			}
		}
		return n, nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		jn, err := parseJSON(b, raw, depth)
		if b.err != nil {
			return nil, b.err
		}
		if err == nil && jn.container {
			jn.label, jn.dlabel, jn.parent, jn.depth = label, boundedText(label), parent, depth
			jn.val, jn.fromVal = v, true // copy through the value's own text
			rebase(jn, depth)
			return jn, nil
		}
	}
	switch {
	case v.IsNull():
		n.text, n.null = "NULL", true
	case inContainer && v.Type() == driver.TypeString:
		s, _ := v.AsText()
		n.leaf, n.text = leafString, boundedText(s)
	default:
		n.text = boundedText(v.String())
	}
	return n, nil
}

// rebase sets the depth of n's descendants from n's depth.
func rebase(n *tnode, depth int) {
	n.depth = depth
	for _, c := range n.children {
		c.parent = n
		rebase(c, depth+1)
	}
}

// boundedText is the one-line escaped text of s, cut to maxTextSize bytes so
// that a huge value is never escaped in full.
func boundedText(s string) string {
	cut := false
	if len(s) > maxTextSize {
		i := maxTextSize
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s, cut = s[:i], true
	}
	s = query.EscapeControls(s)
	if cut {
		s += "…"
	}
	return s
}

// parseJSON decodes raw into a tree, keeping object key order. Subtrees
// deeper than maxDepth (counting from depth) stay as compact text.
func parseJSON(b *builder, raw []byte, depth int) (*tnode, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := b.parseValue(dec, "", depth)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return n, nil
}

func (b *builder) parseValue(dec *json.Decoder, label string, depth int) (*tnode, error) {
	n := &tnode{label: label, dlabel: boundedText(label), depth: depth}
	if depth >= maxDepth {
		var rm json.RawMessage
		if err := dec.Decode(&rm); err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, rm); err != nil {
			return nil, err
		}
		n.leaf, n.text, n.plain = leafCut, "…", buf.String()
		return n, nil
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		n.container, n.isObject = true, t == '{'
		for dec.More() {
			if !b.alive() {
				return nil, b.err
			}
			childLabel := ""
			if n.isObject {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				childLabel, _ = k.(string)
			} else {
				childLabel = fmt.Sprintf("[%d]", len(n.children))
			}
			c, err := b.parseValue(dec, childLabel, depth+1)
			if err != nil {
				return nil, err
			}
			c.parent = n
			n.children = append(n.children, c)
		}
		if _, err := dec.Token(); err != nil { // the closing delimiter
			return nil, err
		}
	case string:
		n.leaf, n.plain, n.text = leafString, t, boundedText(t)
	case json.Number:
		n.leaf, n.plain, n.text = leafLit, t.String(), t.String()
	case bool:
		n.leaf, n.plain = leafLit, fmt.Sprint(t)
		n.text = n.plain
	case nil:
		n.leaf, n.plain, n.text = leafLit, "null", "null"
	}
	return n, nil
}

// valueText is the text `y` copies for a driver value: a scalar's text, or
// compact JSON for objects and lists. A JSON value is already compact in its
// String.
func valueText(v driver.Value) string {
	switch v.Type() {
	case driver.TypeObject, driver.TypeList:
		if b, err := v.MarshalJSON(); err == nil {
			return string(b)
		}
	}
	return v.String()
}

// copyText is what `y` copies for n: a scalar's text, or a container's
// compact JSON.
func (n *tnode) copyText() string {
	if n.fromVal {
		if n.depth == 0 && n.val.IsNull() {
			return "" // a NULL cell copies as nothing; Y keeps the JSON null
		}
		return valueText(n.val)
	}
	if n.container {
		var b bytes.Buffer
		n.writeJSON(&b)
		return b.String()
	}
	return n.plain
}

// writeJSON writes a JSON-sourced node as compact JSON.
func (n *tnode) writeJSON(b *bytes.Buffer) {
	switch {
	case !n.container:
		switch n.leaf {
		case leafString:
			b.WriteString(quote(n.plain))
		default:
			b.WriteString(n.plain)
		}
		return
	}
	open, closing := byte('['), byte(']')
	if n.isObject {
		open, closing = '{', '}'
	}
	b.WriteByte(open)
	for i, c := range n.children {
		if i > 0 {
			b.WriteByte(',')
		}
		if n.isObject {
			b.WriteString(quote(c.label))
			b.WriteByte(':')
		}
		c.writeJSON(b)
	}
	b.WriteByte(closing)
}

func quote(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// summary is the one-line description of a container.
func (n *tnode) summary() string {
	if n.sum != "" {
		return n.sum
	}
	c := len(n.children)
	switch {
	case n.isObject && c == 0:
		return "{}"
	case n.isObject:
		return fmt.Sprintf("{%d %s}", c, plural(c, "key"))
	case c == 0:
		return "[]"
	}
	return fmt.Sprintf("[%d %s]", c, plural(c, "item"))
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
