package mongodb

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Explainer = (*conn)(nil)

// Explain runs the explain command around q's command document, with
// verbosity "queryPlanner", or "executionStats" for ExplainOptions.Analyze.
// MongoDB never applies a write it explains, executionStats included. On a
// read-only call the explain command must pass the read-only guard, so only
// find, aggregate, count and distinct (without $out or $merge) can be
// explained; anything else is ErrReadOnly. The plan's text is the reply as
// indented extended JSON; its tree is the winning plan.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if len(q.Params) > 0 {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("MongoDB commands take no parameters"))
	}
	inner, err := parseCommand(q.Text)
	if err != nil {
		return nil, err
	}
	if inner[0].Key == "explain" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("give the command to explain, not an explain command"))
	}
	verbosity := "queryPlanner"
	if opts.Analyze {
		verbosity = "executionStats"
	}
	cmd := bson.D{{Key: "explain", Value: inner}, {Key: "verbosity", Value: verbosity}}
	if q.ReadOnly || c.readOnly {
		if err := checkReadOnly(cmd); err != nil {
			return nil, err
		}
	}
	if inner, err = prepareCommand(inner, false); err != nil {
		return nil, err
	}
	cmd[0].Value = inner
	reply, err := c.database(q.Namespace).RunCommand(ctx, cmd).Raw()
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	docs, err := replyDocs("explain", cmd, reply)
	if err != nil {
		return nil, driver.Wrap(nil, Name, op, err)
	}
	text, err := bson.MarshalExtJSONIndent(docs[0], false, false, "", "  ")
	if err != nil {
		return nil, driver.Wrap(nil, Name, op, err)
	}
	return &driver.Plan{Format: "json", Text: string(text), Root: planTree(reply)}, nil
}

// planTree finds the first winningPlan in an explain reply (at the top, in
// an aggregate's $cursor stage or under shards) and turns it into nodes:
// the stage as label, the index name or filter as detail, and inputStage
// and inputStages as children. A slot-based plan's queryPlan is used.
func planTree(reply bson.Raw) *driver.PlanNode {
	wp, ok := findKey(bson.RawValue{Type: bson.TypeEmbeddedDocument, Value: reply}, "winningPlan", 0)
	if !ok {
		return nil
	}
	doc, ok := wp.DocumentOK()
	if !ok {
		return nil
	}
	if qp, ok := doc.Lookup("queryPlan").DocumentOK(); ok {
		doc = qp
	}
	return planNode(doc, 0)
}

// findKey searches a value depth-first for the first field named key.
func findKey(v bson.RawValue, key string, depth int) (bson.RawValue, bool) {
	if depth > 32 {
		return bson.RawValue{}, false
	}
	var elems []bson.RawElement
	switch v.Type {
	case bson.TypeEmbeddedDocument:
		elems, _ = v.Document().Elements()
	case bson.TypeArray:
		vals, _ := v.Array().Values()
		for _, e := range vals {
			if r, ok := findKey(e, key, depth+1); ok {
				return r, true
			}
		}
		return bson.RawValue{}, false
	default:
		return bson.RawValue{}, false
	}
	for _, e := range elems {
		if e.Key() == key {
			return e.Value(), true
		}
	}
	for _, e := range elems {
		if r, ok := findKey(e.Value(), key, depth+1); ok {
			return r, true
		}
	}
	return bson.RawValue{}, false
}

func planNode(doc bson.Raw, depth int) *driver.PlanNode {
	n := &driver.PlanNode{Rows: -1}
	n.Label, _ = doc.Lookup("stage").StringValueOK()
	if n.Label == "" {
		n.Label = "stage"
	}
	if ix, ok := doc.Lookup("indexName").StringValueOK(); ok {
		n.Detail = ix
	} else if f, ok := doc.Lookup("filter").DocumentOK(); ok {
		n.Detail = f.String()
	}
	if depth > 32 {
		return n
	}
	if in, ok := doc.Lookup("inputStage").DocumentOK(); ok {
		n.Children = append(n.Children, planNode(in, depth+1))
	}
	if arr, ok := doc.Lookup("inputStages").ArrayOK(); ok {
		vals, _ := arr.Values()
		for _, v := range vals {
			if d, ok := v.DocumentOK(); ok {
				n.Children = append(n.Children, planNode(d, depth+1))
			}
		}
	}
	return n
}
