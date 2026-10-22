package sqlitecore

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// Column resolves a caller's field name to the quoted column of schema: the
// exact name wins, otherwise a unique case-insensitive match. Unknown or
// ambiguous → ErrNotFound.
func Column(name, op string, schema *driver.CollectionSchema, field string) (string, error) {
	var folded []string
	for _, f := range schema.Fields {
		if f.Name == field {
			return QuoteIdent(f.Name), nil
		}
		if strings.EqualFold(f.Name, field) {
			folded = append(folded, f.Name)
		}
	}
	if len(folded) == 1 {
		return QuoteIdent(folded[0]), nil
	}
	if len(folded) > 1 {
		return "", driver.Wrap(driver.ErrNotFound, name, op, fmt.Errorf("field %q is ambiguous in %s", field, schema.Ref.Name))
	}
	return "", driver.Wrap(driver.ErrNotFound, name, op, fmt.Errorf("no field %q in %s", field, schema.Ref.Name))
}

// WhereKey renders the " WHERE …" clause matching key, with its bound
// values; a null value matches IS NULL. An empty key is ErrSyntax.
func WhereKey(name, op string, schema *driver.CollectionSchema, key driver.Object) (string, []driver.Value, error) {
	if len(key) == 0 {
		return "", nil, driver.Wrap(driver.ErrSyntax, name, op, errors.New("the key is empty"))
	}
	var parts []string
	var params []driver.Value
	for _, m := range key {
		col, err := Column(name, op, schema, m.Key)
		if err != nil {
			return "", nil, err
		}
		if m.Value.Type() == driver.TypeNull {
			parts = append(parts, col+" IS NULL")
			continue
		}
		parts = append(parts, col+" = ?")
		params = append(params, m.Value)
	}
	return " WHERE " + strings.Join(parts, " AND "), params, nil
}
