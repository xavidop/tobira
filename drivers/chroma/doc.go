// Package chroma is the tobira driver for Chroma, the open-source vector
// database, over its v2 REST API with a small typed HTTP client. Importing
// it registers the "chroma" driver in driver.Default for the chroma scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/chroma/
//
// Namespaces are the tenant's databases and collections are Chroma
// collections. Records are rows: id, document, uri, embedding, then one
// column per metadata key. Query takes a JSON request whose op is get,
// query or count; the op set is the read-only allow-list, so Query never
// writes. Query texts are embedded only by the host's embedder, never by
// Chroma or a collection's embedding function.
package chroma
