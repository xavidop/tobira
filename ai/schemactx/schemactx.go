// Package schemactx builds the schema context sent to the model with every
// question: the collection in scope, its foreign-key neighbours and the
// other collection names of its namespace, rendered as compact YAML-like
// text within an approximate token budget.
//
// The text is deterministic for a given database, so prompt goldens are
// stable. It never includes a collection's DDL (which can embed secrets in
// defaults or comments) or sample rows, and it goes through the
// connection's redactor.
package schemactx

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/session"
)

const (
	// maxDescribes bounds the Describe calls of one Build, the incoming
	// foreign-key scan included.
	maxDescribes = 50
	// scanTimeout bounds the incoming foreign-key scan.
	scanTimeout = 5 * time.Second
	// unscopedDescribes is how many collections are described when no
	// collection is in scope.
	unscopedDescribes = 5
	// commentRunes is how long a comment may be before the budget squeezes.
	commentRunes = 256
)

// Context is the schema context of one scope.
type Context struct {
	// Dialect and Kind come from the connection's driver.
	Dialect, Kind string
	// Text is the rendered context.
	Text string
	// Collections are the collections described in Text, as
	// "namespace.name", in the order they appear.
	Collections []string
}

type limits struct {
	describes int
	scan      time.Duration
}

// Build returns the schema context of scope within about budgetTokens
// tokens (estimated as characters/4). With a collection in scope it holds
// that collection's description, then its foreign-key neighbours in both
// directions, then the other collection names of the namespace. Over budget
// the tail goes first: the names, then the neighbours from the last, then
// the scoped collection's comments; the scoped collection itself is never
// dropped. Without a collection it holds the namespace's collection names
// and the descriptions of the first five collections, which go first when
// over budget.
func Build(ctx context.Context, mgr *session.Manager, scope assistant.Scope, budgetTokens int) (Context, error) {
	return buildWith(ctx, mgr, scope, budgetTokens, limits{describes: maxDescribes, scan: scanTimeout})
}

// builder carries one Build: the connection and the Describe cache.
type builder struct {
	conn   driver.Conn
	lim    limits
	cache  map[driver.CollectionRef]*driver.CollectionSchema
	calls  int
	redact func(string) string
}

func buildWith(ctx context.Context, mgr *session.Manager, scope assistant.Scope, budgetTokens int, lim limits) (Context, error) {
	if !slices.Contains(mgr.Names(), scope.Conn) {
		return Context{}, fmt.Errorf("unknown connection %q", scope.Conn)
	}
	st := mgr.State(scope.Conn)
	out := Context{Dialect: st.Info.Dialect, Kind: string(st.Info.Kind)}
	conn, err := mgr.Open(ctx, scope.Conn)
	if err != nil {
		return Context{}, err // the session redacts open errors
	}
	b := &builder{conn: conn, lim: lim, cache: map[driver.CollectionRef]*driver.CollectionSchema{},
		redact: func(s string) string { return mgr.Redact(scope.Conn, s) }}
	wrap := func(err error) error { return errors.New(b.redact(err.Error())) }

	ns := scope.Namespace
	if ns == "" {
		ns = b.defaultNamespace(ctx)
	}
	listing, err := conn.Collections(ctx, driver.NamespaceRef{Name: ns})
	if err != nil {
		return Context{}, wrap(err)
	}
	header := fmt.Sprintf("dialect: %s\nkind: %s\nnamespace: %s\n", clean(out.Dialect), clean(out.Kind), clean(ns))

	var secs []section
	var names []string
	if scope.Collection == "" {
		for i, c := range listing {
			if i < unscopedDescribes {
				if s, err := b.describe(ctx, c.Ref); err == nil {
					secs = append(secs, section{ref: normalize(s.Ref, ns), schema: s})
					continue
				}
			}
			names = append(names, c.Ref.Name)
		}
		return b.fit(out, header, secs, names, budgetTokens, false), nil
	}

	scoped, err := b.describe(ctx, driver.CollectionRef{Namespace: ns, Name: scope.Collection})
	if err != nil {
		return Context{}, wrap(err)
	}
	if scoped.Ref.Namespace == "" {
		scoped.Ref.Namespace = ns
	}
	secs = append(secs, section{ref: scoped.Ref, schema: scoped, role: "in scope"})
	seen := map[driver.CollectionRef]bool{scoped.Ref: true}
	// Outgoing: the collections the scoped one references.
	for _, fk := range scoped.ForeignKeys {
		ref := normalize(fk.References, scoped.Ref.Namespace)
		if seen[ref] {
			continue
		}
		if s, err := b.describe(ctx, ref); err == nil {
			seen[ref] = true
			secs = append(secs, section{ref: ref, schema: s, role: "referenced by " + qualified(scoped.Ref)})
		}
	}
	// Incoming: collections of the namespace that reference it, bounded.
	scan, cancel := context.WithTimeout(ctx, b.lim.scan)
	defer cancel()
	for _, c := range listing {
		ref := normalize(c.Ref, ns)
		if seen[ref] || scan.Err() != nil {
			continue
		}
		s, err := b.describe(scan, ref)
		if err != nil {
			continue
		}
		if references(s, ref.Namespace, scoped.Ref) {
			seen[ref] = true
			secs = append(secs, section{ref: ref, schema: s, role: "references " + qualified(scoped.Ref)})
		}
	}
	for _, c := range listing {
		if !seen[normalize(c.Ref, ns)] {
			names = append(names, c.Ref.Name)
		}
	}
	return b.fit(out, header, secs, names, budgetTokens, true), nil
}

func (b *builder) defaultNamespace(ctx context.Context) string {
	nss, err := b.conn.Namespaces(ctx)
	if err != nil || len(nss) == 0 {
		return ""
	}
	for _, n := range nss {
		if n.Default {
			return n.Name
		}
	}
	return nss[0].Name
}

// describe is Describe through the per-Build cache, within the Describe
// bound.
func (b *builder) describe(ctx context.Context, ref driver.CollectionRef) (*driver.CollectionSchema, error) {
	if s, ok := b.cache[ref]; ok {
		return s, nil
	}
	if b.calls >= b.lim.describes {
		return nil, fmt.Errorf("describe limit of %d reached", b.lim.describes)
	}
	b.calls++
	s, err := b.conn.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	b.cache[ref] = s
	return s, nil
}

func normalize(ref driver.CollectionRef, ns string) driver.CollectionRef {
	if ref.Namespace == "" {
		ref.Namespace = ns
	}
	return ref
}

// references reports whether s, which lives in namespace ns, has a foreign
// key to target.
func references(s *driver.CollectionSchema, ns string, target driver.CollectionRef) bool {
	for _, fk := range s.ForeignKeys {
		if normalize(fk.References, ns) == target {
			return true
		}
	}
	return false
}

func qualified(ref driver.CollectionRef) string {
	if ref.Namespace == "" {
		return ref.Name
	}
	return ref.Namespace + "." + ref.Name
}

// section is one described collection.
type section struct {
	ref    driver.CollectionRef
	schema *driver.CollectionSchema
	role   string
}

// fit renders within the budget. Scoped: names are trimmed first, then
// neighbours dropped from the last (room a dropped neighbour frees is filled
// with names again), then the scoped comments shortened.
// Unscoped: descriptions are dropped from the last (their names join the
// name list), then names are trimmed.
func (b *builder) fit(out Context, header string, secs []section, names []string, budgetTokens int, scoped bool) Context {
	maxChars := budgetTokens * 4
	render := func(secs []section, names []string, omitted, comments int) string {
		var sb strings.Builder
		sb.WriteString(b.redact(header))
		if len(secs) > 0 {
			sb.WriteString("collections:\n")
			for _, s := range secs {
				sb.WriteString(b.redact(b.renderSection(s, comments)))
			}
		}
		sb.WriteString(b.redact(renderNames(names, omitted)))
		return sb.String()
	}
	done := func(text string, secs []section) Context {
		out.Text = text
		for _, s := range secs {
			out.Collections = append(out.Collections, b.redact(qualified(s.ref)))
		}
		return out
	}
	// trimmed renders secs with the longest prefix of names that fits, and
	// reports whether it fits at all.
	trimmed := func(secs []section, names []string, comments int) (string, bool) {
		at := func(n int) string { return render(secs, names[:n], len(names)-n, comments) }
		if t := at(len(names)); len(t) <= maxChars {
			return t, true
		}
		if len(at(0)) > maxChars {
			return at(0), false
		}
		lo, hi := 0, len(names) // at(lo) fits, at(hi) does not
		for hi-lo > 1 {
			mid := (lo + hi) / 2
			if len(at(mid)) <= maxChars {
				lo = mid
			} else {
				hi = mid
			}
		}
		return at(lo), true
	}

	if scoped {
		for n := len(secs); n >= 1; n-- {
			if t, ok := trimmed(secs[:n], names, commentRunes); ok {
				return done(t, secs[:n])
			}
		}
		t, ok := trimmed(secs[:1], names, 64)
		if !ok {
			t, _ = trimmed(secs[:1], names, 0)
		}
		return done(t, secs[:1])
	}
	for n := len(secs); n > 0; n-- {
		all := append(sectionNames(secs[n:]), names...)
		if t := render(secs[:n], all, 0, commentRunes); len(t) <= maxChars {
			return done(t, secs[:n])
		}
	}
	t, _ := trimmed(nil, append(sectionNames(secs), names...), commentRunes)
	return done(t, nil)
}

func sectionNames(secs []section) []string {
	out := make([]string, 0, len(secs))
	for _, s := range secs {
		out = append(out, s.ref.Name)
	}
	return out
}

// renderNames is the bare list of the other collections.
func renderNames(names []string, omitted int) string {
	if len(names) == 0 && omitted == 0 {
		return ""
	}
	if len(names) == 0 {
		return "other_collections: " + strconv.Itoa(omitted) + " not shown\n"
	}
	cleaned := make([]string, len(names))
	for i, n := range names {
		cleaned[i] = clean(n)
	}
	line := "other_collections: [" + strings.Join(cleaned, ", ") + "]"
	if omitted > 0 {
		line += " (+" + strconv.Itoa(omitted) + " more)"
	}
	return line + "\n"
}

// renderSection renders one collection; comments longer than comments
// runes are cut, and 0 leaves them out. Comments are redacted before the
// cut: a cut through a secret would leave a prefix no redactor can match.
func (b *builder) renderSection(s section, comments int) string {
	sch := s.schema
	var sb strings.Builder
	fmt.Fprintf(&sb, "  - name: %s\n", clean(qualified(s.ref)))
	if s.role != "" {
		fmt.Fprintf(&sb, "    role: %s\n", clean(s.role))
	}
	if sch.Type != "" {
		fmt.Fprintf(&sb, "    type: %s\n", clean(string(sch.Type)))
	}
	if sch.RowEstimate >= 0 {
		fmt.Fprintf(&sb, "    rows: %d\n", sch.RowEstimate)
	}
	if c := comment(b.redact(sch.Comment), comments); c != "" {
		fmt.Fprintf(&sb, "    comment: %s\n", c)
	}
	if len(sch.Fields) > 0 {
		sb.WriteString("    fields:\n")
		b.renderFields(&sb, sch.Fields, sch.PrimaryKey, "      ", comments)
	}
	if len(sch.PartitionKey) > 0 {
		fmt.Fprintf(&sb, "    partition_key: %s\n", cleanJoin(sch.PartitionKey))
	}
	if len(sch.ClusteringKey) > 0 {
		keys := make([]string, len(sch.ClusteringKey))
		for i, k := range sch.ClusteringKey {
			keys[i] = clean(k.Field) + " asc"
			if k.Desc {
				keys[i] = clean(k.Field) + " desc"
			}
		}
		fmt.Fprintf(&sb, "    clustering_key: %s\n", strings.Join(keys, ", "))
	}
	var idx []string
	for _, ix := range sch.Indexes {
		if ix.Primary {
			continue
		}
		line := clean(ix.Name) + "(" + cleanJoin(ix.Fields) + ")"
		if ix.Unique {
			line += " unique"
		}
		idx = append(idx, line)
	}
	if len(idx) > 0 {
		sb.WriteString("    indexes:\n")
		for _, l := range idx {
			fmt.Fprintf(&sb, "      - %s\n", l)
		}
	}
	if len(sch.ForeignKeys) > 0 {
		sb.WriteString("    foreign_keys:\n")
		for _, fk := range sch.ForeignKeys {
			ref := normalize(fk.References, s.ref.Namespace)
			fmt.Fprintf(&sb, "      - %s -> %s(%s)\n", cleanJoin(fk.Fields), clean(qualified(ref)), cleanJoin(fk.RefFields))
		}
	}
	return sb.String()
}

func (b *builder) renderFields(sb *strings.Builder, fields []driver.Field, pk []string, indent string, comments int) {
	for _, f := range fields {
		typ := f.NativeType
		if typ == "" {
			typ = f.Type.String()
		}
		line := clean(f.Name) + " " + clean(typ)
		isPK := slices.Contains(pk, f.Name)
		if isPK {
			line += " pk"
		}
		if !f.Nullable && !isPK {
			line += " not null"
		}
		if f.Default != "" {
			line += " default " + clean(f.Default)
		}
		if f.Generated {
			line += " generated"
		}
		if f.AutoIncrement {
			line += " auto"
		}
		if c := comment(b.redact(f.Comment), comments); c != "" {
			line += "  # " + c
		}
		fmt.Fprintf(sb, "%s- %s\n", indent, line)
		if len(f.Fields) > 0 {
			b.renderFields(sb, f.Fields, nil, indent+"  ", comments)
		}
	}
}

func comment(s string, runes int) string {
	if runes <= 0 {
		return ""
	}
	s = clean(s)
	if utf8.RuneCountInString(s) <= runes {
		return s
	}
	n := 0
	for i := range s {
		if n == runes {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// clean puts s on one line: control characters become spaces.
func clean(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s))
}

func cleanJoin(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = clean(s)
	}
	return strings.Join(out, ", ")
}
