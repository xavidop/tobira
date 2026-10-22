package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// pseudoColumns are result columns that are not document fields; an
// Insert of a row read from a result leaves them out.
var pseudoColumns = map[string]bool{"_score": true, "_index": true, "_distance": true}

// writeParams make a write visible to the next search before it returns.
func writeParams() url.Values { return url.Values{"refresh": {"wait_for"}} }

func (c *conn) refuseReadOnly(op string) error {
	if c.readOnly {
		return driver.Wrap(driver.ErrReadOnly, c.name(), op, errors.New("the connection is read-only"))
	}
	return nil
}

// Insert indexes one document and returns its _id as the key. With an
// _id in values the document is created with that id, and an existing one
// is ErrConflict (op_type=create); without one the server assigns it.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if err := c.refuseReadOnly(op); err != nil {
		return nil, err
	}
	if err := c.checkIndex(op, ref); err != nil {
		return nil, err
	}
	id := ""
	if v, ok := values.Get("_id"); ok && !v.IsNull() {
		s, err := idText(v)
		if err != nil {
			return nil, c.errorf(driver.ErrSyntax, op, "%v", err)
		}
		id = s
	}
	skip := map[string]bool{"_id": true}
	for k := range pseudoColumns {
		skip[k] = true
	}
	doc, err := objectJSON(values, skip)
	if err != nil {
		return nil, c.errorf(driver.ErrSyntax, op, "%v", err)
	}
	method, path := http.MethodPost, pathOf(ref.Name, "_doc")
	if id != "" {
		method, path = http.MethodPut, pathOf(ref.Name, "_create", id)
	}
	data, err := c.do(ctx, op, method, path, writeParams(), doc)
	if err != nil {
		return nil, err
	}
	var r struct {
		ID string `json:"_id"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.ID == "" {
		return nil, c.errorf(nil, op, "the server returned no _id")
	}
	return driver.Object{{Key: "_id", Value: driver.String(r.ID)}}, nil
}

// Update changes the fields in changes on the one document key matches
// (a partial update: other fields stay). The _id cannot change.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if err := c.refuseReadOnly(op); err != nil {
		return err
	}
	if err := c.checkIndex(op, ref); err != nil {
		return err
	}
	for _, m := range changes {
		if m.Key == "_id" || pseudoColumns[m.Key] {
			return c.errorf(driver.ErrUnsupported, op, "%s is not a document field and cannot change", m.Key)
		}
	}
	if len(changes) == 0 {
		return nil
	}
	doc, err := objectJSON(changes, nil)
	if err != nil {
		return c.errorf(driver.ErrSyntax, op, "%v", err)
	}
	t, err := c.resolveKey(ctx, op, ref, key)
	if err != nil {
		return err
	}
	body := append(append([]byte(`{"doc":`), doc...), '}')
	_, err = c.do(ctx, op, http.MethodPost, pathOf(ref.Name, "_update", t.id), t.params(), body)
	return err
}

// Delete removes the one document key matches.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if err := c.refuseReadOnly(op); err != nil {
		return err
	}
	if err := c.checkIndex(op, ref); err != nil {
		return err
	}
	t, err := c.resolveKey(ctx, op, ref, key)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, op, http.MethodDelete, pathOf(ref.Name, "_doc", t.id), t.params(), nil)
	return err
}

func (c *conn) checkIndex(op string, ref driver.CollectionRef) error {
	if !validTarget(ref.Name) || strings.ContainsAny(ref.Name, "*,") || ref.Name == "_all" {
		return c.errorf(driver.ErrSyntax, op, "edits name one index, not %q", ref.Name)
	}
	return nil
}

// target is the document a key resolved to, with the sequence number and
// primary term it had, so a concurrent change makes the write fail with
// ErrConflict instead of being overwritten.
type target struct {
	id          string
	seqNo, term *int64
}

func (t target) params() url.Values {
	p := writeParams()
	if t.seqNo != nil && t.term != nil {
		p.Set("if_seq_no", strconv.FormatInt(*t.seqNo, 10))
		p.Set("if_primary_term", strconv.FormatInt(*t.term, 10))
	}
	return p
}

// idText reads a document id: a string, or an integer.
func idText(v driver.Value) (string, error) {
	switch v.Type() {
	case driver.TypeString:
		s, _ := v.AsText()
		if s == "" {
			return "", errors.New("_id is empty")
		}
		return s, nil
	case driver.TypeInt:
		n, _ := v.AsInt()
		return strconv.FormatInt(n, 10), nil
	}
	return "", errors.New("_id must be a string")
}

// resolveKey finds the one document key names: by _id alone directly,
// otherwise by searching for documents whose key fields equal the key
// (text fields through their keyword multi-field). No match is
// ErrNotFound, several are ErrConflict.
func (c *conn) resolveKey(ctx context.Context, op string, ref driver.CollectionRef, key driver.Object) (target, error) {
	if len(key) == 0 {
		return target{}, c.errorf(driver.ErrSyntax, op, "an empty key matches every document")
	}
	if v, ok := key.Get("_id"); ok && len(key) == 1 {
		id, err := idText(v)
		if err != nil {
			return target{}, c.errorf(driver.ErrSyntax, op, "%v", err)
		}
		return target{id: id}, nil
	}
	m, _, err := c.mappingOf(ctx, op, ref.Name)
	if err != nil {
		return target{}, err
	}
	var filter []any
	for _, k := range key {
		if k.Key == "_id" {
			id, err := idText(k.Value)
			if err != nil {
				return target{}, c.errorf(driver.ErrSyntax, op, "%v", err)
			}
			filter = append(filter, map[string]any{"ids": map[string]any{"values": []string{id}}})
			continue
		}
		clause, negate, err := conditionClause(c.name(), op, driver.Condition{Field: k.Key, Op: driver.OpEq, Value: k.Value}, m)
		if err != nil {
			return target{}, err
		}
		if negate { // a NULL key value
			clause = map[string]any{"bool": map[string]any{"must_not": []any{clause}}}
		}
		filter = append(filter, clause)
	}
	body, _ := json.Marshal(map[string]any{
		"size": 2, "_source": false, "seq_no_primary_term": true,
		"query": map[string]any{"bool": map[string]any{"filter": filter}},
	})
	data, err := c.do(ctx, op, http.MethodPost, pathOf(ref.Name, "_search"), nil, body)
	if err != nil {
		return target{}, err
	}
	var r struct {
		Hits struct {
			Hits []struct {
				ID    string `json:"_id"`
				SeqNo *int64 `json:"_seq_no"`
				Term  *int64 `json:"_primary_term"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return target{}, c.errorf(nil, op, "unexpected search response")
	}
	switch hits := r.Hits.Hits; len(hits) {
	case 0:
		return target{}, c.errorf(driver.ErrNotFound, op, "no document matches the key")
	case 1:
		return target{id: hits[0].ID, seqNo: hits[0].SeqNo, term: hits[0].Term}, nil
	}
	return target{}, c.errorf(driver.ErrConflict, op, "the key matches more than one document")
}
