package tui

import (
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/tui/detail"
	"github.com/xavidop/tobira/internal/tui/grid"
)

// Kind-specific views. The document view (Object and List previews, absent
// fields, the detail's collapse and expand-all) lives in the grid and the
// detail, for every collection; this file picks the key-value, wide-column
// and search views, and chart.go and similar.go hold the time-series and
// vector ones.

// kvColumn is the index of the first column named one of names, any case,
// or -1.
func kvColumn(cols []driver.Column, names ...string) int {
	for i, c := range cols {
		for _, n := range names {
			if strings.EqualFold(c.Name, n) {
				return i
			}
		}
	}
	return -1
}

// isKeyValue reports whether a collection opens in the key-value view: its
// driver is a key-value store, or the driver listed it as a key pattern.
func isKeyValue(info driver.Info, ct driver.CollectionType) bool {
	return info.Kind == driver.KindKeyValue || ct == driver.CollectionKeyPattern
}

// kvFormat renders a key-value row's cells: the TTL in words (∞, 12s, 3h)
// and a sorted set as {member: score, …}. Everything else keeps the
// default rendering.
func kvFormat(cols []driver.Column, row driver.Row, col int) (string, bool) {
	switch {
	case col == kvColumn(cols, "ttl"):
		return detail.TTLText(row[col])
	case col == kvColumn(cols, "value"):
		ti := kvColumn(cols, "type")
		if ti < 0 || ti >= len(row) {
			return "", false
		}
		if typ, _ := row[ti].AsText(); !strings.EqualFold(typ, "zset") {
			return "", false
		}
		list, ok := row[col].AsList()
		if !ok {
			return "", false
		}
		ms := make([]driver.Member, 0, min(len(list), 64))
		for _, e := range list[:min(len(list), 64)] { // the cell shows a few
			obj, _ := e.AsObject()
			member, _ := obj.Get("member")
			score, _ := obj.Get("score")
			ms = append(ms, driver.Member{Key: member.String(), Value: score})
		}
		text := grid.Preview(driver.ObjectValue(ms...), 4*80)
		if len(list) > len(ms) {
			text = strings.TrimSuffix(text, "}") + ", …}"
		}
		return text, true
	}
	return "", false
}

// cellFormat is the grid format of a collection or result of a driver
// with info: the key-value view for a key-value collection (ct), the
// relevance score for a search engine, else none.
func cellFormat(info driver.Info, ct driver.CollectionType) grid.Format {
	switch {
	case isKeyValue(info, ct):
		return kvFormat
	case info.Kind == driver.KindSearch:
		return searchFormat
	}
	return nil
}

// resultFormat is the grid format of an editor result of a driver with
// info: a search engine's relevance score, else none.
func resultFormat(info driver.Info) grid.Format {
	if info.Kind == driver.KindSearch {
		return searchFormat
	}
	return nil
}

// searchFormat renders a search engine's _score column as a relevance
// score with three decimals (1.288, not 1.2876821). A NULL or non-numeric
// score keeps the default rendering.
func searchFormat(cols []driver.Column, row driver.Row, col int) (string, bool) {
	if col != kvColumn(cols, "_score") || col >= len(row) {
		return "", false
	}
	v := row[col]
	f, ok := v.AsFloat()
	if !ok && v.Type() == driver.TypeDecimal {
		s, _ := v.AsText()
		var err error
		f, err = strconv.ParseFloat(s, 64)
		ok = err == nil
	}
	if !ok {
		return "", false
	}
	return strconv.FormatFloat(f, 'f', 3, 64), true
}

// needsDescribe reports whether opening a collection reads its schema:
// editing needs the key and the field types, and a wide-column table's
// header shows its key badges.
func needsDescribe(info driver.Info, conn driver.Conn) bool {
	if _, ok := driver.As[driver.Mutator](conn); ok {
		return true
	}
	return info.Kind == driver.KindWideColumn
}

// keyBadges returns the header marks of a wide-column table's key, from
// its schema: [P] on the partition key columns and [C↓] or [C↑] on the
// clustering columns, by clustering order. It is nil when sch has no
// partition or clustering key.
func keyBadges(sch *driver.CollectionSchema) func(driver.Column) string {
	if sch == nil || (len(sch.PartitionKey) == 0 && len(sch.ClusteringKey) == 0) {
		return nil
	}
	badges := map[string]string{}
	for _, k := range sch.PartitionKey {
		badges[k] = "[P]"
	}
	for _, k := range sch.ClusteringKey {
		if _, ok := badges[k.Field]; ok {
			continue
		}
		badges[k.Field] = "[C↑]"
		if k.Desc {
			badges[k.Field] = "[C↓]"
		}
	}
	return func(c driver.Column) string { return badges[c.Name] }
}

// truncatedEditText refuses to edit a value the driver cut for its
// preview: writing it back would lose the rest.
const truncatedEditText = "value truncated in preview; edit it with a query"

// truncatedCol is the column a driver adds to mark a cut preview: a Bool,
// right after the column whose value it describes, true when the preview
// was cut, and described as Generated (see the driver guide). The browse
// grid hides it; the row detail shows it.
const truncatedCol = "_truncated"

// marksTruncation reports whether sch describes a _truncated field as
// Generated: only then is it the driver's marker rather than a real column
// that happens to bear the name.
func marksTruncation(sch *driver.CollectionSchema) bool {
	if sch == nil {
		return false
	}
	for _, f := range sch.Fields {
		if strings.EqualFold(f.Name, truncatedCol) {
			return f.Generated
		}
	}
	return false
}

// previewCut reports whether cell col of row holds a preview the driver
// cut: sch marks _truncated (marksTruncation), the row's _truncated column
// is true and col is the column right before it (or a column named value).
func previewCut(sch *driver.CollectionSchema, cols []driver.Column, row driver.Row, col int) bool {
	if !marksTruncation(sch) {
		return false
	}
	ti := kvColumn(cols, truncatedCol)
	if ti < 0 || ti >= len(row) || (col != ti-1 && col != kvColumn(cols, "value")) {
		return false
	}
	cut, ok := row[ti].AsBool()
	return ok && cut
}

// hiddenColumn is the columns the browse grid does not draw once the
// schema marks them (marksTruncation): the _truncated marker. It stays in
// the rows, so the row detail shows it and previewCut reads it.
func hiddenColumn(c driver.Column) bool { return strings.EqualFold(c.Name, truncatedCol) }

// kvEditField adapts the cell editor's field for a key-value value cell
// (column col of row): a non-string value is edited as JSON of its own
// type, so the driver gets back an Object or a List. A stream is refused:
// its entries are appended, not replaced. why is the refusal, if any.
func kvEditField(cols []driver.Column, row driver.Row, col int, f fieldInfo) (fieldInfo, string) {
	if col != kvColumn(cols, "value") {
		return f, ""
	}
	ti := kvColumn(cols, "type")
	if ti < 0 || ti >= len(row) || col >= len(row) {
		return f, ""
	}
	typ, _ := row[ti].AsText()
	switch strings.ToLower(typ) {
	case "string":
		f.Type = driver.TypeString
		return f, ""
	case "stream":
		return f, "a stream's entries are appended, not replaced: edit it in the query editor"
	}
	switch t := row[col].Type(); t {
	case driver.TypeObject, driver.TypeList, driver.TypeJSON:
		f.Type = t
	}
	return f, ""
}
