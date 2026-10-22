package mssql

import (
	"strings"
	"testing"
)

const samplePlan = `<?xml version="1.0" encoding="utf-16"?>
<ShowPlanXML xmlns="http://schemas.microsoft.com/sqlserver/2004/07/showplan" Version="1.6">
 <BatchSequence><Batch><Statements>
  <StmtSimple StatementText="select *&#xD;&#xA;  from tobira_people where id &gt; 3" StatementSubTreeCost="0.0034" StatementEstRows="54" StatementType="SELECT">
   <QueryPlan>
    <RelOp NodeId="0" PhysicalOp="Nested Loops" LogicalOp="Inner Join" EstimateRows="54" EstimatedTotalSubtreeCost="0.0034">
     <NestedLoops>
      <RelOp NodeId="1" PhysicalOp="Clustered Index Seek" LogicalOp="Clustered Index Seek" EstimateRows="54.4" EstimatedTotalSubtreeCost="0.002">
       <IndexScan><Object Database="[c]" Schema="[dbo]" Table="[tobira_people]" Index="[PK_p]"/></IndexScan>
      </RelOp>
      <RelOp NodeId="2" PhysicalOp="Constant Scan" LogicalOp="Constant Scan" EstimateRows="1" EstimatedTotalSubtreeCost="0.0000011"><ConstantScan/></RelOp>
     </NestedLoops>
    </RelOp>
   </QueryPlan>
  </StmtSimple>
 </Statements></Batch></BatchSequence>
</ShowPlanXML>`

func TestBuildPlan(t *testing.T) {
	plan, err := buildPlan([]string{samplePlan})
	if err != nil {
		t.Fatal(err)
	}
	root := plan.Root
	if root.Label != "Statement" || root.Detail != "select * from tobira_people where id > 3" || root.Rows != 54 || root.Cost != 0.0034 {
		t.Errorf("root = %+v", root)
	}
	if len(root.Children) != 1 || root.Children[0].Label != "Nested Loops" || root.Children[0].Detail != "Inner Join" {
		t.Fatalf("root children = %+v", root.Children)
	}
	nl := root.Children[0]
	if len(nl.Children) != 2 {
		t.Fatalf("nested loops children = %+v", nl.Children)
	}
	if seek := nl.Children[0]; seek.Label != "Clustered Index Seek" || seek.Detail != "[c].[dbo].[tobira_people].[PK_p]" || seek.Rows != 54 {
		t.Errorf("seek = %+v", seek)
	}
	if plan.Format != "text" || !strings.Contains(plan.Text, "-> Clustered Index Seek") || !strings.Contains(plan.Text, "rows=54") {
		t.Errorf("text = %q", plan.Text)
	}
	two, err := buildPlan([]string{samplePlan, samplePlan})
	if err != nil || two.Root.Label != "plan" || len(two.Root.Children) != 2 {
		t.Errorf("two statements: %+v, %v", two, err)
	}
	if _, err := buildPlan([]string{"<not-xml"}); err == nil {
		t.Error("bad XML parsed")
	}
	if _, err := buildPlan([]string{"<ShowPlanXML/>"}); err == nil {
		t.Error("a plan without statements parsed")
	}
}
