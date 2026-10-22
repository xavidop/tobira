package elasticsearch

import (
	"testing"
)

// A sub-field named keyword is used only when it is keyword-typed.
func TestKeywordPathChecksSubfieldType(t *testing.T) {
	m, _, err := parseMappings([]byte(`{"x":{"mappings":{"properties":{
		"a":{"type":"text","fields":{"keyword":{"type":"text"},"raw":{"type":"keyword"}}},
		"b":{"type":"text","fields":{"keyword":{"type":"text"}}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := m.lookup("a").keywordPath("a"); !ok || p != "a.raw" {
		t.Errorf("a: %q %v, want a.raw", p, ok)
	}
	if p, ok := m.lookup("b").keywordPath("b"); ok {
		t.Errorf("b: %q, want no exact path", p)
	}
}
