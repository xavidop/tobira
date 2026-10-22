package dynamodb

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
)

// maxIn is DynamoDB's limit on the operands of IN in an expression.
const maxIn = 100

// filterExpr is a FilterExpression with its placeholders.
type filterExpr struct {
	expr   string
	names  map[string]string
	values map[string]types.AttributeValue
}

// buildFilter translates conditions to a FilterExpression, ANDed. Each
// field is one top-level attribute name (a dot is part of the name), bound
// as #fN; each value is bound as :vN.
//
//   - eq, ne, lt, le, gt, ge as =, <>, <, <=, >, >= (DynamoDB compares only
//     values of the same type); eq NULL is is_null and ne NULL is not_null;
//   - in as IN (…), at most 100 values; an empty list matches nothing;
//   - contains as contains() (a substring of a string, a member of a set
//     or list), prefix as begins_with(); both are ErrUnsupported on a
//     number key attribute, where DynamoDB never matches;
//   - is_null as missing or of type NULL, not_null as present and not NULL.
//
// Any other operator is ErrUnsupported.
func buildFilter(conds []driver.Condition, meta *tableMeta) (filterExpr, error) {
	f := filterExpr{names: map[string]string{}, values: map[string]types.AttributeValue{}}
	fieldRef := map[string]string{}
	name := func(field string) string {
		if p, ok := fieldRef[field]; ok {
			return p
		}
		p := "#f" + strconv.Itoa(len(fieldRef))
		fieldRef[field] = p
		f.names[p] = field
		return p
	}
	value := func(av types.AttributeValue) string {
		p := ":v" + strconv.Itoa(len(f.values))
		f.values[p] = av
		return p
	}
	valueOfCond := func(v driver.Value) (string, error) {
		av, err := toAV(v)
		if err != nil {
			return "", browseErr(driver.KindOf(err), err)
		}
		return value(av), nil
	}
	nullType := func() string { return value(&types.AttributeValueMemberS{Value: "NULL"}) }
	var parts []string
	for _, c := range conds {
		if c.Field == "" {
			return filterExpr{}, browseErr(driver.ErrSyntax, errors.New("a condition needs a field name"))
		}
		op := c.Op
		if c.Value.IsNull() && op == driver.OpEq {
			op = driver.OpIsNull
		} else if c.Value.IsNull() && op == driver.OpNe {
			op = driver.OpNotNull
		}
		switch op {
		case driver.OpIsNull:
			n := name(c.Field)
			parts = append(parts, fmt.Sprintf("(attribute_not_exists(%s) OR attribute_type(%s, %s))", n, n, nullType()))
		case driver.OpNotNull:
			n := name(c.Field)
			parts = append(parts, fmt.Sprintf("(attribute_exists(%s) AND NOT attribute_type(%s, %s))", n, n, nullType()))
		case driver.OpEq, driver.OpNe, driver.OpLt, driver.OpLe, driver.OpGt, driver.OpGe:
			n := name(c.Field)
			v, err := valueOfCond(c.Value)
			if err != nil {
				return filterExpr{}, err
			}
			parts = append(parts, n+" "+comparison[op]+" "+v)
		case driver.OpIn:
			list, ok := c.Value.AsList()
			if !ok {
				return filterExpr{}, browseErr(driver.ErrSyntax, fmt.Errorf("in on %q needs a list", c.Field))
			}
			if len(list) > maxIn {
				return filterExpr{}, browseErr(driver.ErrUnsupported, fmt.Errorf("in takes at most %d values in DynamoDB", maxIn))
			}
			n := name(c.Field)
			if len(list) == 0 {
				parts = append(parts, fmt.Sprintf("(attribute_exists(%s) AND attribute_not_exists(%s))", n, n))
				continue
			}
			vs := make([]string, len(list))
			for i, e := range list {
				v, err := valueOfCond(e)
				if err != nil {
					return filterExpr{}, err
				}
				vs[i] = v
			}
			parts = append(parts, n+" IN ("+strings.Join(vs, ", ")+")")
		case driver.OpContains, driver.OpPrefix:
			if meta != nil && meta.keyTypes[c.Field] == "N" && isKey(meta, c.Field) {
				return filterExpr{}, browseErr(driver.ErrUnsupported, fmt.Errorf("%s does not apply to the number key %q in DynamoDB", op, c.Field))
			}
			n := name(c.Field)
			v, err := valueOfCond(c.Value)
			if err != nil {
				return filterExpr{}, err
			}
			fn := "contains"
			if op == driver.OpPrefix {
				fn = "begins_with"
			}
			parts = append(parts, fn+"("+n+", "+v+")")
		default:
			return filterExpr{}, browseErr(driver.ErrUnsupported, fmt.Errorf("filter operator %q", c.Op))
		}
	}
	f.expr = strings.Join(parts, " AND ")
	return f, nil
}

var comparison = map[driver.Op]string{
	driver.OpEq: "=", driver.OpNe: "<>", driver.OpLt: "<", driver.OpLe: "<=", driver.OpGt: ">", driver.OpGe: ">=",
}

func isKey(meta *tableMeta, field string) bool {
	for _, k := range meta.keys {
		if k == field {
			return true
		}
	}
	return false
}

func browseErr(kind, err error) error {
	var de *driver.Error
	if errors.As(err, &de) {
		return driver.Wrap(de.Kind, Name, "browse", de.Err)
	}
	return driver.Wrap(kind, Name, "browse", err)
}
