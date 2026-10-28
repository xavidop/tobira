package github

import (
	"strings"
	"testing"
)

func TestClassifyGraphQL(t *testing.T) {
	t.Parallel()
	accepted := []string{
		"{ viewer { login } }",
		"  \n{ viewer { login } }",
		"query { viewer { login } }",
		"query{viewer{login}}",
		"query Name($x: Int) { node(id: $x) { id } }",
		"query Name($x: Int) {\n  node(id: $x) { id }\n}",
		"# mutation here\n{ viewer { login } }",
		"{ viewer { login } } # a mutation would be refused",
		`{ search(query: "mutation", type: ISSUE) { issueCount } }`,
		`{ search(query: "is:open \" mutation", type: ISSUE) { issueCount } }`,
		"query { a(doc: \"\"\"\n  a mutation and a subscription\n\"\"\") { b } }",
		"{ mutationCount }",
		"{ subscriptionId }",
		"{ a(mutation_kind: 1) }",
		"{ my_mutation { x } }",
		"query mutationLike { a }",
	}
	for _, doc := range accepted {
		if err := classifyGraphQL(doc); err != nil {
			t.Errorf("classifyGraphQL(%q) = %v, want accepted", doc, err)
		}
	}
	refused := []struct {
		doc  string
		hint string
	}{
		{"mutation { addStar(input: {starrableId: \"x\"}) { clientMutationId } }", `must start with "{" or "query"`},
		{"mutation M { a }", `must start with "{" or "query"`},
		{"subscription { a }", `must start with "{" or "query"`},
		{"query { a } mutation { b }", "mutation is refused"},
		{"query { a } subscription { b }", "subscription is refused"},
		{"{ a } mutation{b}", "mutation"},
		{"{ a }\nmutation {b}", "mutation is refused"},
		{"fragment F on Repository { name } query { ...F }", `must start with "{" or "query"`},
		{"", "empty"},
		{"   \n\t", "empty"},
		{"# only a comment", "empty"},
		{`"only a string"`, "empty"},
		{"queryFoo { x }", `must start with "{" or "query"`},
		{"querymutation { x }", `must start with "{" or "query"`},
		{"{ a } # fine\nmutation { b }", "mutation is refused"},
	}
	for _, tc := range refused {
		err := classifyGraphQL(tc.doc)
		if err == nil {
			t.Errorf("classifyGraphQL(%q) accepted, want refused", tc.doc)
			continue
		}
		if !strings.Contains(err.Error(), tc.hint) {
			t.Errorf("classifyGraphQL(%q) = %v, want %q", tc.doc, err, tc.hint)
		}
	}
}

func TestStripGraphQL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		doc, want string
	}{
		{"{ a }", "{ a }"},
		{"{ a } # comment\n{ b }", "{ a } \n{ b }"},
		{"# comment only", ""},
		{`{ a(x: "mutation") }`, "{ a(x:  ) }"},
		{`{ a(x: "esc \" mutation") }`, "{ a(x:  ) }"},
		{`{ a(x: """block mutation""") }`, "{ a(x:  ) }"},
		{"{ a(x: \"\"\"\nmulti\nline \"quoted\" mutation\n\"\"\") }", "{ a(x:  ) }"},
		{`{ a(x: "one", y: "two") }`, "{ a(x:  , y:  ) }"},
		// An unterminated block string swallows the rest of the document.
		{`{ a(x: """ mutation`, "{ a(x:  "},
		// An unterminated string swallows the rest too.
		{`{ a(x: " mutation`, "{ a(x:  "},
		// A # inside a string is not a comment.
		{`{ a(x: "#not a comment") { b } }`, "{ a(x:  ) { b } }"},
	}
	for _, tc := range tests {
		if got := stripGraphQL(tc.doc); got != tc.want {
			t.Errorf("stripGraphQL(%q) = %q, want %q", tc.doc, got, tc.want)
		}
	}
	if got := stripGraphQL(`{ a(x: """ mutation`); strings.Contains(got, "mutation") {
		t.Errorf("an unterminated block string leaked its contents: %q", got)
	}
}
