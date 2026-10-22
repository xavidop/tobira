// Package weaviate is the tobira driver for Weaviate, built on the official
// Go client (github.com/weaviate/weaviate-go-client/v5) over REST and
// GraphQL. Importing it registers the "weaviate" driver in driver.Default
// for the weaviate scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/weaviate/
//
// Query takes a small JSON request ({"collection": …, "where": …,
// "nearVector": …}) or a raw GraphQL read: one { Get { … } } or
// { Aggregate { … } } operation, parsed fail-closed. Weaviate's GraphQL has
// no mutations, and Query never writes; edits go through the Mutator. On a
// read-only connection, operators that call a vectorizer, generative or
// reranker module (egress and cost) are refused. Objects are rows: id, the
// properties, then one column per vector.
package weaviate
