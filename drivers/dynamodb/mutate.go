package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Mutator = (*conn)(nil)

// Insert writes one item with PutItem, conditioned on
// attribute_not_exists of the partition key, so an existing item is
// never replaced: that is ErrConstraint. The values must hold every key
// attribute; the returned key is those attributes, in key order. A NULL
// value is stored as a NULL attribute.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	meta, err := c.mutation(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	it := make(item, len(values))
	for _, m := range values {
		if _, dup := it[m.Key]; dup {
			return nil, mutateErr(op, driver.ErrSyntax, fmt.Errorf("attribute %q appears twice", m.Key))
		}
		av, err := toAV(m.Value)
		if err != nil {
			return nil, mutateErr(op, driver.KindOf(err), err)
		}
		it[m.Key] = av
	}
	key := make(driver.Object, 0, len(meta.keys))
	for _, k := range meta.keys {
		v, ok := values.Get(k)
		if !ok || v.IsNull() {
			return nil, mutateErr(op, driver.ErrSyntax, fmt.Errorf("the item needs its key attribute %q", k))
		}
		key = append(key, driver.Member{Key: k, Value: v})
	}
	_, err = c.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                aws.String(ref.Name),
		Item:                     it,
		ConditionExpression:      aws.String("attribute_not_exists(#k0)"),
		ExpressionAttributeNames: map[string]string{"#k0": meta.keys[0]},
	})
	if conditionFailed(err) {
		return nil, mutateErr(op, driver.ErrConstraint, errors.New("an item with this key already exists"))
	}
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	return key, nil
}

// Update sets each change with UpdateItem (SET #a = :a; a change to NULL
// stores NULL), conditioned on the item existing and on any non-key
// fields of key still holding their values: no match is ErrNotFound. A key
// attribute cannot change (ErrUnsupported), except to its current value.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	meta, err := c.mutation(ctx, op, ref)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return mutateErr(op, driver.ErrSyntax, errors.New("no changes"))
	}
	t, err := target(op, key, meta)
	if err != nil {
		return err
	}
	var sets []string
	seen := map[string]bool{}
	for _, ch := range changes {
		if seen[ch.Key] {
			return mutateErr(op, driver.ErrSyntax, fmt.Errorf("attribute %q changes twice", ch.Key))
		}
		seen[ch.Key] = true
		av, err := toAV(ch.Value)
		if err != nil {
			return mutateErr(op, driver.KindOf(err), err)
		}
		if isKey(meta, ch.Key) {
			if compareAV(av, t.key[ch.Key]) == 0 && nativeType(av) == nativeType(t.key[ch.Key]) {
				continue
			}
			return mutateErr(op, driver.ErrUnsupported, fmt.Errorf("key attribute %q cannot change: delete the item and insert a new one", ch.Key))
		}
		sets = append(sets, t.name(ch.Key)+" = "+t.value(av))
	}
	if len(sets) == 0 {
		return c.exists(ctx, op, ref.Name, t)
	}
	_, err = c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(ref.Name),
		Key:                       t.key,
		UpdateExpression:          aws.String("SET " + strings.Join(sets, ", ")),
		ConditionExpression:       aws.String(t.condition()),
		ExpressionAttributeNames:  t.names,
		ExpressionAttributeValues: t.valuesOrNil(),
	})
	return c.notFoundOnCondition(ctx, op, err)
}

// Delete removes one item with DeleteItem, conditioned as Update is: no
// match is ErrNotFound.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	meta, err := c.mutation(ctx, op, ref)
	if err != nil {
		return err
	}
	t, err := target(op, key, meta)
	if err != nil {
		return err
	}
	_, err = c.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                 aws.String(ref.Name),
		Key:                       t.key,
		ConditionExpression:       aws.String(t.condition()),
		ExpressionAttributeNames:  t.names,
		ExpressionAttributeValues: t.valuesOrNil(),
	})
	return c.notFoundOnCondition(ctx, op, err)
}

// mutation runs the checks every edit shares and returns the table's key
// schema.
func (c *conn) mutation(ctx context.Context, op string, ref driver.CollectionRef) (*tableMeta, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := c.writable(op); err != nil {
		return nil, err
	}
	if err := checkNamespace(ref.Namespace, op); err != nil {
		return nil, err
	}
	return c.catalog.meta(ctx, op, ref.Name)
}

// exists checks that the target item exists (an Update whose changes were
// all no-ops).
func (c *conn) exists(ctx context.Context, op, table string, t *itemTarget) error {
	out, err := c.db.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(table), Key: t.key, ConsistentRead: aws.Bool(true)})
	if err != nil {
		return c.fail(ctx, op, err)
	}
	if len(out.Item) == 0 {
		return mutateErr(op, driver.ErrNotFound, errors.New("no item matches the key"))
	}
	for i, n := range t.extraNames {
		if !matchesAny(out.Item[n], t.extraValues[i]) {
			return mutateErr(op, driver.ErrNotFound, errors.New("no item matches the key"))
		}
	}
	return nil
}

func (c *conn) notFoundOnCondition(ctx context.Context, op string, err error) error {
	if conditionFailed(err) {
		return mutateErr(op, driver.ErrNotFound, errors.New("no item matches the key"))
	}
	return c.fail(ctx, op, err)
}

// itemTarget is the item an Update or Delete addresses: its primary key,
// plus conditions on any other fields the key object holds, with the
// expression placeholders they use.
type itemTarget struct {
	key         item
	conds       []string
	names       map[string]string
	values      map[string]types.AttributeValue
	extraNames  []string
	extraValues [][]types.AttributeValue // per extra field, the values it may hold (nil = missing or NULL)
}

func (t *itemTarget) name(attr string) string {
	for p, n := range t.names {
		if n == attr {
			return p
		}
	}
	p := "#a" + strconv.Itoa(len(t.names))
	t.names[p] = attr
	return p
}

func (t *itemTarget) value(av types.AttributeValue) string {
	p := ":a" + strconv.Itoa(len(t.values))
	t.values[p] = av
	return p
}

func (t *itemTarget) condition() string { return strings.Join(t.conds, " AND ") }

func (t *itemTarget) valuesOrNil() map[string]types.AttributeValue {
	if len(t.values) == 0 {
		return nil
	}
	return t.values
}

// target reads a key object: every key attribute must be there, not NULL;
// other fields become equality conditions (NULL: missing or NULL).
func target(op string, key driver.Object, meta *tableMeta) (*itemTarget, error) {
	t := &itemTarget{key: item{}, names: map[string]string{}, values: map[string]types.AttributeValue{}}
	seen := map[string]bool{}
	for _, m := range key {
		if seen[m.Key] {
			return nil, mutateErr(op, driver.ErrSyntax, fmt.Errorf("key field %q appears twice", m.Key))
		}
		seen[m.Key] = true
	}
	for _, k := range meta.keys {
		v, ok := key.Get(k)
		if !ok || v.IsNull() {
			return nil, mutateErr(op, driver.ErrUnsupported, fmt.Errorf("the key needs the key attribute %q: DynamoDB edits one item by its full primary key", k))
		}
		av, err := toAV(v)
		if err != nil {
			return nil, mutateErr(op, driver.KindOf(err), err)
		}
		t.key[k] = av
	}
	t.conds = append(t.conds, "attribute_exists("+t.name(meta.keys[0])+")")
	for _, m := range key {
		if isKey(meta, m.Key) {
			continue
		}
		n := t.name(m.Key)
		if m.Value.IsNull() {
			t.conds = append(t.conds, fmt.Sprintf("(attribute_not_exists(%s) OR attribute_type(%s, %s))", n, n, t.value(&types.AttributeValueMemberS{Value: "NULL"})))
			t.extraNames = append(t.extraNames, m.Key)
			t.extraValues = append(t.extraValues, []types.AttributeValue{nil})
			continue
		}
		av, err := toAV(m.Value)
		if err != nil {
			return nil, mutateErr(op, driver.KindOf(err), err)
		}
		alts := []types.AttributeValue{av}
		cond := n + " = " + t.value(av)
		if set, ok := setAlternative(m.Value); ok {
			// A set shows as a List in tobira: match either form.
			alts = append(alts, set)
			cond = "(" + cond + " OR " + n + " = " + t.value(set) + ")"
		}
		t.conds = append(t.conds, cond)
		t.extraNames = append(t.extraNames, m.Key)
		t.extraValues = append(t.extraValues, alts)
	}
	return t, nil
}

// setAlternative returns the set a List may stand for: an SS, NS or BS when
// the list is non-empty, holds only strings, only numbers or only bytes,
// and has no duplicates (a set cannot be empty or repeat a member). Numbers
// are compared by value, as DynamoDB does: [1, 1.0] has no NS alternative,
// so it matches only a stored List (and no match is ErrNotFound), instead
// of sending an NS that DynamoDB rejects as holding duplicates.
func setAlternative(v driver.Value) (types.AttributeValue, bool) {
	list, ok := v.AsList()
	if !ok || len(list) == 0 {
		return nil, false
	}
	seen := map[string]bool{}
	var ss, ns []string
	var bs [][]byte
	for _, e := range list {
		switch e.Type() {
		case driver.TypeString:
			s, _ := e.AsText()
			ss = append(ss, s)
		case driver.TypeInt, driver.TypeDecimal, driver.TypeFloat:
			av, err := toAV(e)
			if err != nil {
				return nil, false
			}
			n := av.(*types.AttributeValueMemberN).Value
			canon, ok := canonicalNumber(n)
			if !ok || seen["N:"+canon] {
				return nil, false // 1 and 1.0 are one DynamoDB number
			}
			seen["N:"+canon] = true
			ns = append(ns, n)
			continue
		case driver.TypeBytes:
			b, _ := e.AsBytes()
			bs = append(bs, b)
		default:
			return nil, false
		}
		k := e.Type().String() + ":" + e.String()
		if seen[k] {
			return nil, false
		}
		seen[k] = true
	}
	switch {
	case len(ss) == len(list):
		return &types.AttributeValueMemberSS{Value: ss}, true
	case len(ns) == len(list):
		return &types.AttributeValueMemberNS{Value: ns}, true
	case len(bs) == len(list):
		return &types.AttributeValueMemberBS{Value: bs}, true
	}
	return nil, false
}

// matchesAny reports whether a stored attribute equals one of alts, with
// the same DynamoDB type; a nil alternative matches a missing or NULL
// attribute.
func matchesAny(stored types.AttributeValue, alts []types.AttributeValue) bool {
	for _, a := range alts {
		if a == nil {
			if typeRank(stored) == 0 {
				return true
			}
			continue
		}
		if nativeType(stored) == nativeType(a) && compareAV(stored, a) == 0 {
			return true
		}
	}
	return false
}

func conditionFailed(err error) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "ConditionalCheckFailedException"
}

func mutateErr(op string, kind, err error) error {
	var de *driver.Error
	if errors.As(err, &de) {
		return driver.Wrap(de.Kind, Name, op, de.Err)
	}
	return driver.Wrap(kind, Name, op, err)
}

// canonicalNumber spells the decimal number s so that two spellings of one
// value are equal: sign, significant digits without leading or trailing
// zeros, then the exponent ("1.0" and "10e-1" are both "1e0"; every zero is
// "0"). ok is false for text that is not a decimal number or whose exponent
// has more than six digits.
func canonicalNumber(s string) (string, bool) {
	sign := ""
	switch {
	case strings.HasPrefix(s, "-"):
		sign, s = "-", s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	mant, expText, hasExp := strings.Cut(strings.ToLower(s), "e")
	exp := 0
	if hasExp {
		if len(strings.TrimLeft(strings.TrimLeft(expText, "+-"), "0")) > 6 {
			return "", false
		}
		e, err := strconv.Atoi(expText)
		if err != nil {
			return "", false
		}
		exp = e
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := intPart + frac
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return "", false
	}
	exp -= len(frac)
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", true
	}
	trimmed := strings.TrimRight(digits, "0")
	exp += len(digits) - len(trimmed)
	return sign + trimmed + "e" + strconv.Itoa(exp), true
}
