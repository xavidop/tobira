// Package milvus is the tobira driver for Milvus, built on
// github.com/milvus-io/milvus/client/v2 over gRPC. Importing it registers
// the "milvus" driver in driver.Default for the milvus scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/milvus/
//
// Query takes a JSON request whose op is query, get, count or search; the
// op switch is the read-only allow-list, so Query never writes and every
// other op is refused before anything is sent. Boolean filters pass a
// fail-closed lexer, and filters the driver builds carry every value as a
// template parameter. A read-only connection never loads a collection and
// never searches through a TEXTEMBEDDING function or a model reranker
// (egress).
package milvus
