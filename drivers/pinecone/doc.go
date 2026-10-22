// Package pinecone is the tobira driver for Pinecone, built on
// github.com/pinecone-io/go-pinecone (the control plane over REST, records
// over gRPC). Importing it registers the "pinecone" driver in
// driver.Default for the pinecone scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/pinecone/
//
// Indexes are namespaces and Pinecone namespaces are collections. Query
// takes a JSON request whose op is query, fetch, list or stats; the op
// switch is the read-only allow-list, so Query never writes and every
// other op is refused before anything is sent. Records are rows: _id, the
// dense values, the sparse values when present, then one column per
// metadata key.
package pinecone
