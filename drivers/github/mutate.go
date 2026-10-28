package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// The Mutator edits one item of a collection, keyed by its key column.
// Each collection's mutation table (schema.go) says which members a write
// accepts; GitHub applies the write without a condition, so a change
// another client made meanwhile is overwritten.

// singular names one item of a collection: "issue", "pull request".
func singular(col *collection) string {
	return strings.ReplaceAll(strings.TrimSuffix(col.name, "s"), "_", " ")
}

// refuseReadOnly fails a write on a read-only connection before any request.
func (c *conn) refuseReadOnly(ctx context.Context, op string) error {
	if err := c.check(ctx, op); err != nil {
		return err
	}
	if c.readOnly {
		return readOnlyErr(op, "the connection is read-only")
	}
	return nil
}

// Insert creates one item. The key is assigned by GitHub (except a label's
// name) and returned.
func (c *conn) Insert(ctx context.Context, ref driver.CollectionRef, values driver.Object) (driver.Object, error) {
	const op = "insert"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return nil, err
	}
	col, owner, repo, err := c.collectionRef(ctx, op, ref)
	if err != nil {
		return nil, err
	}
	if col.insert == nil {
		return nil, unsupportedErr(op, "GitHub does not create %s through tobira", col.name)
	}
	body := map[string]any{}
	pathKey := ""
	for _, m := range values {
		if m.Value.IsNull() {
			continue
		}
		if !slices.Contains(col.insert.fields, m.Key) {
			if m.Key == col.keyCol {
				return nil, syntaxErr(op, "GitHub assigns the %s of a new %s: leave it out", col.keyCol, singular(col))
			}
			if col.col(m.Key) == nil {
				return nil, syntaxErr(op, "%s have no column %q", col.name, m.Key)
			}
			return nil, unsupportedErr(op, "%q cannot be set when creating %s (it can be set with: %s)", m.Key, col.name, strings.Join(col.insert.fields, ", "))
		}
		if col.name == "issue_comments" && m.Key == "issue" {
			// The number goes into the request path: only a positive
			// integer is accepted.
			s, ok := text(m.Value)
			n, err := strconv.ParseInt(s, 10, 64)
			if !ok || err != nil || n <= 0 {
				return nil, syntaxErr(op, `"issue" must be the issue number`)
			}
			pathKey = strconv.FormatInt(n, 10)
			continue
		}
		v, err := c.bodyValue(ctx, op, col, owner, repo, m.Key, m.Value)
		if err != nil {
			return nil, err
		}
		body[m.Key] = v
	}
	for _, column := range col.columns {
		if column.required && slices.Contains(col.insert.fields, column.name) {
			if _, ok := body[column.name]; !ok && (column.name != "issue" || pathKey == "") {
				return nil, syntaxErr(op, "a new %s needs %q", singular(col), column.name)
			}
		}
	}
	if col.name == "issue_comments" && pathKey == "" {
		return nil, syntaxErr(op, `a new comment needs "issue", the issue number`)
	}
	raw, err := c.send(ctx, op, col.insert.method, col.insert.path(owner, repo, pathKey), body)
	if err != nil {
		return nil, err
	}
	row, err := col.row(raw)
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	c.catalog.Invalidate()
	return driver.Object{{Key: col.keyCol, Value: row[indexOf(col.columns, col.keyCol)]}}, nil
}

// Update changes the given members of one item; the others keep their
// values. A NULL clears a member GitHub lets go empty. A missing item is
// ErrNotFound.
func (c *conn) Update(ctx context.Context, ref driver.CollectionRef, key driver.Object, changes driver.Object) error {
	const op = "update"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return err
	}
	col, owner, repo, err := c.collectionRef(ctx, op, ref)
	if err != nil {
		return err
	}
	if col.update == nil {
		return unsupportedErr(op, "GitHub does not change %s through tobira", col.name)
	}
	if len(changes) == 0 {
		return nil
	}
	id, err := keyText(op, col, key)
	if err != nil {
		return err
	}
	body := map[string]any{}
	for _, m := range changes {
		if !slices.Contains(col.update.fields, m.Key) {
			if m.Key == col.keyCol {
				return unsupportedErr(op, "the %s of a %s cannot change", col.keyCol, singular(col))
			}
			if col.col(m.Key) == nil {
				return syntaxErr(op, "%s have no column %q", col.name, m.Key)
			}
			return unsupportedErr(op, "%q is computed by GitHub and cannot be edited (editable: %s)", m.Key, strings.Join(col.update.fields, ", "))
		}
		if m.Value.IsNull() && col.col(m.Key).required {
			return syntaxErr(op, "%q cannot be empty", m.Key)
		}
		v, err := c.bodyValue(ctx, op, col, owner, repo, m.Key, m.Value)
		if err != nil {
			return err
		}
		name := m.Key
		if col.name == "labels" && m.Key == "name" {
			name = "new_name" // a label is renamed through new_name
		}
		body[name] = v
	}
	_, err = c.send(ctx, op, col.update.method, col.update.path(owner, repo, id), body)
	if err == nil {
		c.catalog.Invalidate()
	}
	return err
}

// Delete removes one item. Issues and pull requests cannot be deleted
// through the REST API: close them instead.
func (c *conn) Delete(ctx context.Context, ref driver.CollectionRef, key driver.Object) error {
	const op = "delete"
	if err := c.refuseReadOnly(ctx, op); err != nil {
		return err
	}
	col, owner, repo, err := c.collectionRef(ctx, op, ref)
	if err != nil {
		return err
	}
	if col.delete == nil {
		switch col.name {
		case "issues", "pull_requests":
			return unsupportedErr(op, "GitHub cannot delete %s through its REST API: close it instead (set state to closed)", singular(col))
		}
		return unsupportedErr(op, "GitHub does not delete %s through tobira", col.name)
	}
	id, err := keyText(op, col, key)
	if err != nil {
		return err
	}
	_, err = c.send(ctx, op, col.delete.method, col.delete.path(owner, repo, id), nil)
	if err == nil {
		c.catalog.Invalidate()
	}
	return err
}

// send runs one write and returns the answer body.
func (c *conn) send(ctx context.Context, op, method, path string, body map[string]any) (json.RawMessage, error) {
	var payload any
	if body != nil {
		payload = body
	}
	resp, err := c.client.do(ctx, method, c.client.url(path, nil), payload)
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	if method == http.MethodDelete || resp.status == http.StatusNoContent {
		return nil, nil
	}
	return json.RawMessage(resp.body), nil
}

// keyText reads the key of an item: the key column's value, or the only
// member of a one-member key.
func keyText(op string, col *collection, key driver.Object) (string, error) {
	v, ok := key.Get(col.keyCol)
	if !ok && len(key) == 1 {
		return "", syntaxErr(op, "%s are keyed by %q, not %q", col.name, col.keyCol, key[0].Key)
	}
	if !ok {
		return "", syntaxErr(op, "the key must give %q", col.keyCol)
	}
	s, ok := text(v)
	if !ok || s == "" {
		return "", syntaxErr(op, "%q must be a string or a number", col.keyCol)
	}
	return s, nil
}

// bodyValue converts one member to what GitHub takes: lists of names for
// labels and assignees, a milestone number for a milestone title, RFC 3339
// text for a time, a boolean, a string, or null to clear.
func (c *conn) bodyValue(ctx context.Context, op string, col *collection, owner, repo, name string, v driver.Value) (any, error) {
	column := col.col(name)
	switch {
	case v.IsNull():
		return nil, nil
	case name == "labels" || name == "assignees":
		list, err := stringList(v)
		if err != nil {
			return nil, syntaxErr(op, "%q: %v", name, err)
		}
		return list, nil
	case name == "milestone":
		return c.milestoneNumber(ctx, op, owner, repo, v)
	case column != nil && column.typ == driver.TypeBool:
		if b, ok := v.AsBool(); ok {
			return b, nil
		}
		if s, ok := v.AsText(); ok {
			if b, err := strconv.ParseBool(s); err == nil {
				return b, nil
			}
		}
		return nil, syntaxErr(op, "%q must be true or false", name)
	case column != nil && column.typ == driver.TypeTime:
		if t, ok := v.AsTime(); ok {
			return t.UTC().Format("2006-01-02T15:04:05Z"), nil
		}
		if s, ok := v.AsText(); ok {
			return s, nil
		}
		return nil, syntaxErr(op, "%q must be a time", name)
	case column != nil && column.typ == driver.TypeInt:
		if i, ok := v.AsInt(); ok {
			return i, nil
		}
		if s, ok := v.AsText(); ok {
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return i, nil
			}
		}
		return nil, syntaxErr(op, "%q must be a whole number", name)
	case column != nil && column.typ == driver.TypeString:
		if s, ok := text(v); ok {
			return s, nil
		}
		return nil, syntaxErr(op, "%q must be text", name)
	}
	x, err := toJSON(v)
	if err != nil {
		return nil, syntaxErr(op, "%q: %v", name, err)
	}
	return x, nil
}

// milestoneNumber resolves a milestone given by number, or by title among
// the repository's milestones.
func (c *conn) milestoneNumber(ctx context.Context, op, owner, repo string, v driver.Value) (any, error) {
	if i, ok := v.AsInt(); ok {
		return i, nil
	}
	title, ok := v.AsText()
	if !ok {
		return nil, syntaxErr(op, `"milestone" must be a title or a number`)
	}
	if n, err := strconv.ParseInt(title, 10, 64); err == nil {
		return n, nil
	}
	req := listRequest{col: collectionByName("milestones"), owner: owner, repo: repo, query: url.Values{"state": {"all"}}, perPage: maxPerPage}
	for page := 1; page <= maxListRows/maxPerPage; page++ {
		items, next, err := c.fetchPage(ctx, op, req, page)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			var m struct {
				Number int64  `json:"number"`
				Title  string `json:"title"`
			}
			if json.Unmarshal(it, &m) == nil && m.Title == title {
				return m.Number, nil
			}
		}
		if !next {
			break
		}
	}
	return nil, notFoundErr(op, "no milestone titled %q in %s/%s", title, owner, repo)
}
