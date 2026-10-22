package dynamodb

import (
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
)

// A key object may carry a set attribute shown as a List; the condition
// must match the stored set as well as a stored list.
func TestTargetSetFields(t *testing.T) {
	meta := &tableMeta{name: "t", keys: []string{"pk"}, keyTypes: map[string]string{"pk": "S"}}
	for _, tc := range []struct {
		name    string
		value   driver.Value
		setType string // "" = no set alternative
	}{
		{"strings", driver.List(driver.String("b"), driver.String("a")), "SS"},
		{"numbers", driver.List(driver.Int(1), driver.Decimal("2.5")), "NS"},
		{"bytes", driver.List(driver.Bytes([]byte{1})), "BS"},
		{"empty", driver.List(), ""},
		{"mixed", driver.List(driver.String("a"), driver.Int(1)), ""},
		{"duplicates", driver.List(driver.String("a"), driver.String("a")), ""},
		// Equal as numbers, so no NS can hold both: the list matches only
		// as a List (no match is ErrNotFound), never a ValidationException.
		{"numeric duplicates", driver.List(driver.Int(1), driver.Decimal("1.0")), ""},
		{"numeric duplicates by exponent", driver.List(driver.Decimal("1e2"), driver.Int(100)), ""},
		{"zero and minus zero", driver.List(driver.Decimal("-0"), driver.Int(0)), ""},
		{"distinct numbers", driver.List(driver.Decimal("1.5"), driver.Int(15), driver.Decimal("0.15")), "NS"},
		{"nested", driver.List(driver.List()), ""},
	} {
		key := driver.Object{{Key: "pk", Value: driver.String("k")}, {Key: "tags", Value: tc.value}}
		tg, err := target("update", key, meta)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		cond := tg.condition()
		var nts []string
		for _, v := range tg.values {
			nts = append(nts, nativeType(v))
		}
		hasSet := tc.setType != "" && slices.Contains(nts, tc.setType)
		if tc.setType != "" && (!hasSet || !strings.Contains(cond, " OR ")) {
			t.Errorf("%s: want a %s alternative, got %q with %v", tc.name, tc.setType, cond, nts)
		}
		if tc.setType == "" && strings.Contains(cond, " OR ") {
			t.Errorf("%s: no set alternative expected, got %q", tc.name, cond)
		}
		if len(tg.extraValues) != 1 || len(tg.extraValues[0]) != 1+btoi(hasSet) {
			t.Errorf("%s: extra values %v", tc.name, tg.extraValues)
		}
	}
	// The no-op existence check accepts the stored set.
	tg, _ := target("update", driver.Object{{Key: "pk", Value: driver.String("k")},
		{Key: "tags", Value: driver.List(driver.String("b"), driver.String("a"))}}, meta)
	stored := &types.AttributeValueMemberSS{Value: []string{"a", "b"}}
	if !matchesAny(stored, tg.extraValues[0]) {
		t.Error("a stored string set must match the list from the grid")
	}
	if matchesAny(&types.AttributeValueMemberSS{Value: []string{"a"}}, tg.extraValues[0]) {
		t.Error("a different set must not match")
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestCanonicalNumber(t *testing.T) {
	for in, want := range map[string]string{
		"1": "1e0", "1.0": "1e0", "10e-1": "1e0", "+1": "1e0", "100": "1e2", "1e2": "1e2",
		"0": "0", "-0": "0", "0.000": "0", "-1.50": "-15e-1", ".5": "5e-1", "5.": "5e0",
	} {
		if got, ok := canonicalNumber(in); !ok || got != want {
			t.Errorf("canonicalNumber(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "abc", "1e", "1e9999999", "1.2.3", "--1", "."} {
		if _, ok := canonicalNumber(bad); ok {
			t.Errorf("canonicalNumber(%q) ok, want refused", bad)
		}
	}
}
