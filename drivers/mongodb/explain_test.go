package mongodb

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPlanTree(t *testing.T) {
	classic := `{"queryPlanner":{"winningPlan":{"stage":"FETCH","filter":{"a":{"$eq":1}},"inputStage":{"stage":"IXSCAN","indexName":"a_1"}}}}`
	sbe := `{"queryPlanner":{"winningPlan":{"queryPlan":{"stage":"SORT","inputStages":[{"stage":"COLLSCAN"},{"stage":"IXSCAN","indexName":"b_1"}]},"slotBasedPlan":{}}}}`
	agg := `{"stages":[{"$cursor":{"queryPlanner":{"winningPlan":{"stage":"COLLSCAN"}}}},{"$group":{}}]}`
	parse := func(s string) bson.Raw {
		var d bson.D
		if err := bson.UnmarshalExtJSON([]byte(s), false, &d); err != nil {
			t.Fatal(err)
		}
		b, _ := bson.Marshal(d)
		return b
	}
	root := planTree(parse(classic))
	if root == nil || root.Label != "FETCH" || root.Rows != -1 || len(root.Children) != 1 || root.Children[0].Label != "IXSCAN" || root.Children[0].Detail != "a_1" {
		t.Fatalf("classic plan %+v", root)
	}
	if root.Detail == "" {
		t.Error("FETCH filter detail missing")
	}
	root = planTree(parse(sbe))
	if root == nil || root.Label != "SORT" || len(root.Children) != 2 || root.Children[1].Detail != "b_1" {
		t.Fatalf("sbe plan %+v", root)
	}
	if root = planTree(parse(agg)); root == nil || root.Label != "COLLSCAN" {
		t.Fatalf("aggregate plan %+v", root)
	}
	if planTree(parse(`{"ok":1}`)) != nil {
		t.Error("a reply without a plan has no tree")
	}
}
