// Package qdrant is the tobira driver for Qdrant, built on
// github.com/qdrant/go-client over gRPC. Importing it registers the "qdrant"
// driver in driver.Default for the qdrant scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/qdrant/
//
// Query takes a native JSON request whose op is scroll, search, query,
// count or retrieve; the op switch is the read-only allow-list, so Query
// never writes and every other op is refused before anything is sent.
// Points are rows: id, one column per vector, then the payload keys.
package qdrant
