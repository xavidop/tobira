package elasticsearch

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// mapping is the merged field mapping of one or more indexes: fields in
// the order the server lists them, with nested properties.
type mapping struct {
	order  []string
	fields map[string]*field
}

// field is one mapped field.
type field struct {
	name       string
	typ        string // the mapping type; "object" when only properties are given
	enabled    bool   // false for an object with "enabled": false
	dims       int    // dense_vector
	similarity string // dense_vector
	indexed    bool   // dense_vector: "index" (true unless false)
	props      *mapping
	multi      map[string]string // multi-fields: sub-field name -> type
}

func newMapping() *mapping { return &mapping{fields: map[string]*field{}} }

// rawField is a field's JSON mapping.
type rawField struct {
	Type       string                     `json:"type"`
	Enabled    *bool                      `json:"enabled"`
	Dims       int                        `json:"dims"`
	Similarity string                     `json:"similarity"`
	Index      *bool                      `json:"index"`
	Properties map[string]json.RawMessage `json:"properties"`
	Fields     map[string]rawField        `json:"fields"`
}

// parseMappings reads a GET <target>/_mapping response. Several indexes
// (an alias, a pattern) merge in index-name order; the first index to map
// a field wins. It returns the index names, sorted.
func parseMappings(body []byte) (*mapping, []string, error) {
	var resp map[string]struct {
		Mappings struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, fmt.Errorf("unexpected mapping response: %v", err)
	}
	indices := make([]string, 0, len(resp))
	for k := range resp {
		indices = append(indices, k)
	}
	slices.Sort(indices)
	m := newMapping()
	for _, idx := range indices {
		if err := m.merge(resp[idx].Mappings.Properties); err != nil {
			return nil, nil, err
		}
	}
	return m, indices, nil
}

// merge adds the fields of props not yet mapped, in name order (the order
// the server returns them in).
func (m *mapping) merge(props map[string]json.RawMessage) error {
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, name := range names {
		var rf rawField
		if err := json.Unmarshal(props[name], &rf); err != nil {
			return fmt.Errorf("unexpected mapping of %q: %v", name, err)
		}
		if f, ok := m.fields[name]; ok {
			if f.props != nil && rf.Properties != nil {
				if err := f.props.merge(rf.Properties); err != nil {
					return err
				}
			}
			continue
		}
		f := &field{name: name, typ: rf.Type, enabled: rf.Enabled == nil || *rf.Enabled, dims: rf.Dims, similarity: rf.Similarity, indexed: rf.Index == nil || *rf.Index}
		if f.typ == "" {
			f.typ = "object"
		}
		if rf.Properties != nil {
			f.props = newMapping()
			if err := f.props.merge(rf.Properties); err != nil {
				return err
			}
		}
		for sub, sf := range rf.Fields {
			if f.multi == nil {
				f.multi = map[string]string{}
			}
			f.multi[sub] = sf.Type
		}
		m.order = append(m.order, name)
		m.fields[name] = f
	}
	return nil
}

// lookup finds a field by its dotted path, or nil.
func (m *mapping) lookup(path string) *field {
	if m == nil {
		return nil
	}
	if f, ok := m.fields[path]; ok {
		return f
	}
	head, rest, ok := strings.Cut(path, ".")
	if !ok {
		return nil
	}
	if f, ok := m.fields[head]; ok && f.props != nil {
		return f.props.lookup(rest)
	}
	return nil
}

// stringTypes are the mapping types whose values are text.
var stringTypes = map[string]bool{
	"text": true, "keyword": true, "constant_keyword": true, "wildcard": true, "match_only_text": true,
	"search_as_you_type": true, "ip": true, "version": true, "completion": true, "annotated_text": true,
}

// exactTypes are the string types a term query matches exactly.
var exactTypes = map[string]bool{"keyword": true, "constant_keyword": true, "wildcard": true, "ip": true, "version": true}

// keywordPath returns the path to query or sort f exactly: f itself when
// it is a keyword-like field, its keyword multi-field when it is text, or
// false.
func (f *field) keywordPath(path string) (string, bool) {
	if f == nil {
		return path, false
	}
	if exactTypes[f.typ] {
		return path, true
	}
	if t, ok := f.multi["keyword"]; ok && exactTypes[t] {
		return path + ".keyword", true
	}
	subs := make([]string, 0, len(f.multi))
	for sub, typ := range f.multi {
		if exactTypes[typ] {
			subs = append(subs, sub)
		}
	}
	if len(subs) > 0 {
		slices.Sort(subs)
		return path + "." + subs[0], true
	}
	return path, false
}

// isText reports whether f is analysed text (no exact matching).
func (f *field) isText() bool { return f != nil && stringTypes[f.typ] && !exactTypes[f.typ] }

// fieldType is the tobira type of a mapped field.
func fieldType(f *field) driver.Type {
	if f == nil {
		return driver.TypeUnknown
	}
	switch f.typ {
	case "long", "integer", "short", "byte", "unsigned_long":
		return driver.TypeInt
	case "double", "float", "half_float":
		return driver.TypeFloat
	case "scaled_float":
		return driver.TypeDecimal
	case "boolean":
		return driver.TypeBool
	case "date", "date_nanos":
		return driver.TypeTime
	case "object":
		if !f.enabled {
			return driver.TypeJSON
		}
		return driver.TypeObject
	case "nested":
		return driver.TypeList
	case "dense_vector":
		return driver.TypeVector
	case "binary":
		return driver.TypeBytes
	}
	if stringTypes[f.typ] {
		return driver.TypeString
	}
	return driver.TypeJSON
}

// nativeType spells a field's mapping type for display.
func nativeType(f *field) string {
	switch {
	case f == nil:
		return ""
	case f.typ == "object" && !f.enabled:
		return "object (enabled: false)"
	case f.typ == "dense_vector":
		s := fmt.Sprintf("dense_vector(%d", f.dims)
		if f.similarity != "" {
			s += ", " + f.similarity
		}
		return s + ")"
	}
	return f.typ
}

// schemaFields converts a mapping to tobira fields, recursively.
func schemaFields(m *mapping) []driver.Field {
	if m == nil {
		return nil
	}
	out := make([]driver.Field, 0, len(m.order))
	for _, name := range m.order {
		f := m.fields[name]
		fd := driver.Field{Name: name, NativeType: nativeType(f), Type: fieldType(f), Nullable: true, Fields: schemaFields(f.props)}
		if len(f.multi) > 0 {
			subs := make([]string, 0, len(f.multi))
			for sub, typ := range f.multi {
				subs = append(subs, name+"."+sub+" ("+typ+")")
			}
			slices.Sort(subs)
			fd.Comment = "multi-fields: " + strings.Join(subs, ", ")
		}
		out = append(out, fd)
	}
	return out
}
