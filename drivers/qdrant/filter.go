package qdrant

import (
	"fmt"
	"math"

	pb "github.com/qdrant/go-client/qdrant"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/xavidop/tobira/driver"
)

// buildFilter translates a driver filter to a Qdrant filter, or nil when it
// is empty. Raw is a Qdrant Filter message in its JSON form, ANDed with the
// conditions. Conditions Qdrant cannot express exactly are ErrUnsupported:
// substring and prefix matches (Qdrant's text match is word-based), order
// comparisons on the point id, and comparisons with decimals, lists or
// objects.
func buildFilter(f driver.Filter, s *collSchema) (*pb.Filter, error) {
	out := &pb.Filter{}
	if f.Raw != "" {
		raw := &pb.Filter{}
		if err := protojson.Unmarshal([]byte(f.Raw), raw); err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "filter", fmt.Errorf("invalid raw filter (a Qdrant Filter in JSON): %v", err))
		}
		out.Must = append(out.Must, pb.NewFilterAsCondition(raw))
	}
	for _, c := range f.Conditions {
		must, mustNot, err := condition(c, s)
		if err != nil {
			return nil, err
		}
		if must != nil {
			out.Must = append(out.Must, must)
		}
		if mustNot != nil {
			out.MustNot = append(out.MustNot, mustNot)
		}
	}
	if len(out.Must) == 0 && len(out.MustNot) == 0 {
		return nil, nil
	}
	return out, nil
}

// condition translates one condition into a must or a must_not clause.
func condition(c driver.Condition, s *collSchema) (must, mustNot *pb.Condition, err error) {
	unsupported := func(why string) (*pb.Condition, *pb.Condition, error) {
		return nil, nil, driver.Wrap(driver.ErrUnsupported, Name, "filter", fmt.Errorf("%s %s: %s", c.Field, c.Op, why))
	}
	if c.Field == idColumn {
		switch c.Op {
		case driver.OpEq, driver.OpNe, driver.OpIn:
			ids, err := idList(c)
			if err != nil {
				return nil, nil, err
			}
			if c.Op == driver.OpNe {
				return nil, pb.NewHasID(ids...), nil
			}
			return pb.NewHasID(ids...), nil, nil
		case driver.OpNotNull:
			return nil, nil, nil // every point has an id
		}
		return unsupported("the point id can only be matched by value")
	}
	key := s.payloadKey(c.Field)
	switch c.Op {
	case driver.OpIsNull:
		return pb.NewIsEmpty(key), nil, nil
	case driver.OpNotNull:
		return nil, pb.NewIsEmpty(key), nil
	case driver.OpEq, driver.OpNe:
		m, err := match(key, c.Value)
		if err != nil {
			return unsupported(err.Error())
		}
		if c.Op == driver.OpNe {
			return nil, m, nil
		}
		return m, nil, nil
	case driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
		r, err := rangeCond(key, c.Op, c.Value)
		if err != nil {
			return unsupported(err.Error())
		}
		return r, nil, nil
	case driver.OpIn:
		items, _ := c.Value.AsList()
		var strs []string
		var ints []int64
		for _, it := range items {
			switch it.Type() {
			case driver.TypeString:
				s, _ := it.AsText()
				strs = append(strs, s)
			case driver.TypeInt:
				i, _ := it.AsInt()
				ints = append(ints, i)
			default:
				return unsupported("in takes strings or integers")
			}
		}
		switch {
		case len(strs) > 0 && len(ints) > 0:
			return unsupported("in takes strings or integers, not both")
		case len(ints) > 0:
			return pb.NewMatchInts(key, ints...), nil, nil
		}
		return pb.NewMatchKeywords(key, strs...), nil, nil
	case driver.OpContains, driver.OpPrefix:
		return unsupported("qdrant matches whole words, not substrings or prefixes")
	}
	return unsupported("unknown operator")
}

// match is an equality condition on a payload key.
func match(key string, v driver.Value) (*pb.Condition, error) {
	switch v.Type() {
	case driver.TypeBool:
		b, _ := v.AsBool()
		return pb.NewMatchBool(key, b), nil
	case driver.TypeInt:
		i, _ := v.AsInt()
		return pb.NewMatchInt(key, i), nil
	case driver.TypeString:
		s, _ := v.AsText()
		return pb.NewMatchKeyword(key, s), nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%v cannot be matched", f)
		}
		return pb.NewRange(key, &pb.Range{Gte: &f, Lte: &f}), nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		ts := timestamppb.New(t)
		return pb.NewDatetimeRange(key, &pb.DatetimeRange{Gte: ts, Lte: ts}), nil
	}
	return nil, fmt.Errorf("cannot compare with a %s", v.Type())
}

// rangeCond is an order comparison on a payload key: numbers and times.
func rangeCond(key string, op driver.Op, v driver.Value) (*pb.Condition, error) {
	if v.Type() == driver.TypeTime {
		t, _ := v.AsTime()
		ts := timestamppb.New(t)
		r := &pb.DatetimeRange{}
		switch op {
		case driver.OpLt:
			r.Lt = ts
		case driver.OpLe:
			r.Lte = ts
		case driver.OpGt:
			r.Gt = ts
		case driver.OpGe:
			r.Gte = ts
		}
		return pb.NewDatetimeRange(key, r), nil
	}
	if v.Type() != driver.TypeInt && v.Type() != driver.TypeFloat {
		return nil, fmt.Errorf("order comparisons take a number or a time, not a %s", v.Type())
	}
	f, _ := v.AsFloat()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("%v cannot be compared", f)
	}
	r := &pb.Range{}
	switch op {
	case driver.OpLt:
		r.Lt = &f
	case driver.OpLe:
		r.Lte = &f
	case driver.OpGt:
		r.Gt = &f
	case driver.OpGe:
		r.Gte = &f
	}
	return pb.NewRange(key, r), nil
}

// idList converts the value of an id condition (a List for OpIn) to point
// ids.
func idList(c driver.Condition) ([]*pb.PointId, error) {
	vals := []driver.Value{c.Value}
	if c.Op == driver.OpIn {
		vals, _ = c.Value.AsList()
	}
	ids := make([]*pb.PointId, 0, len(vals))
	for _, v := range vals {
		id, err := pointID(v)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
