package qdrant

import (
	"errors"
	"testing"

	pb "github.com/qdrant/go-client/qdrant"

	"github.com/xavidop/tobira/driver"
)

func TestCursorRoundTrip(t *testing.T) {
	for _, c := range []browseCursor{
		{offset: pb.NewIDNum(21)},
		{offset: pb.NewIDUUID("5c56c793-69f3-4fbf-87e6-c4bf54c28c26")},
		{sorted: true, key: "age", skip: 40},
		{sorted: true, key: "age", desc: true, skip: 40},
		{sorted: true, key: "a|b:c%d", nulls: true},
		{sorted: true, key: "age", desc: true, nulls: true, offset: pb.NewIDNum(9)},
	} {
		s := c.encode()
		got, err := decodeCursor(s)
		if err != nil {
			t.Fatalf("decode(%q): %v", s, err)
		}
		if got.encode() != s || got.key != c.key || got.desc != c.desc {
			t.Errorf("round trip %q -> %q (%+v)", s, got.encode(), got)
		}
	}
	if c, err := decodeCursor(""); err != nil || c.sorted || c.offset != nil {
		t.Errorf("empty cursor = %+v, %v", c, err)
	}
	for _, bad := range []string{"x", "o|", "o|num:-1", "o|uuid:zz", "s|a|abc|age", "s|a|-1|age", "s|x|1|age", "s|a|1|",
		"n|a|age|num:", "n|a||", "garbage|1", "s|a|1|age|extra"} {
		if _, err := decodeCursor(bad); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("decode(%q): err = %v, want ErrSyntax", bad, err)
		}
	}
}

// TestCursorMatchesBrowse: a cursor is only valid for the sort it came from.
func TestCursorMatchesBrowse(t *testing.T) {
	sorted := browseCursor{sorted: true, key: "age", skip: 20}
	for _, tt := range []struct {
		cur  browseCursor
		key  string
		desc bool
		ok   bool
	}{
		{sorted, "age", false, true},
		{sorted, "age", true, false},
		{sorted, "score", false, false},
		{sorted, "", false, false},
		{browseCursor{offset: pb.NewIDNum(3)}, "", false, true},
		{browseCursor{offset: pb.NewIDNum(3)}, "age", false, false},
	} {
		err := tt.cur.matches(tt.key, tt.desc)
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, driver.ErrSyntax)) {
			t.Errorf("%+v matches(%q, %v) = %v, want ok %v", tt.cur, tt.key, tt.desc, err, tt.ok)
		}
	}
}
