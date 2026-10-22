// Package drivertest is the conformance kit every tobira driver must pass.
//
// Run executes the core checks (info, ping, schema calls, queries, limits,
// parameters, cancellation, read-only, close, concurrency) and a subtest for
// each capability the driver lists in its Info. With Config.Strict a claimed
// capability must be exercised instead of skipped; the sqlite and postgres
// drivers in this repository run strict, and so does examples/custom-driver-module.
//
// The kit also provides the standard People dataset (People, PeopleDataset),
// a SQL seed renderer for SQL dialects (SQLSeed), a meaning-based value
// comparison (EqualValue) and Fake, an in-memory driver for testing hosts.
//
// For the non-SQL kinds it provides one fixture each: Documents (schemaless
// documents), KeyValueDataset, VectorDataset and TimeSeriesDataset, which the
// Fake seeds with WithDocuments, WithKeyValues, WithVectors and
// WithTimeSeries. KindVectorFixture and KindTimeSeriesFixture turn the vector
// and time-series datasets into Strict fixtures with exact expectations: the
// nearest neighbour under each metric, k honoured, and the window aggregates.
// WideColumnDataset (partition and clustering keys) and SearchDocuments (an
// index with a mapping), seeded by WithWideColumns and WithSearch, back the
// kind subtests WideColumn and Search through KindWideColumnFixture and
// KindSearchFixture: the keys Describe reports, a partition read in
// clustering order, the mapping, and full-text hits ranked by _score.
// Under Strict a widecolumn or search driver must set its kind fixture.
// Fixture.SelectAll is compared with People in id order; a store that
// cannot read a whole collection in key order (DynamoDB) sets
// Fixture.UnorderedSelectAll, and the rows are then matched by id, each id
// exactly once, with columns, count and values still checked.
// A store that cannot hold the Mutator suite's People-shaped probe row
// without an extra member (a vector store whose records all need a vector)
// sets Fixture.InsertDefaults; the kit appends those members to the probe
// row and refuses any that would replace a People value.
// HashEmbedder is a deterministic driver.Embedder for tests.
// Every capability subtest is checked against Fake, and against each real
// driver whose conformance test runs the kit and claims that capability.
//
// It imports only the standard library, package driver and goleak.
package drivertest
