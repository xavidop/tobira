// Package cosmos is the tobira driver for Azure Cosmos DB for NoSQL, built
// on github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos. Importing it
// registers the "cosmos" driver in driver.Default for the cosmos scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/cosmos/
//
// A query is one Cosmos SQL SELECT. Its container is the name in the FROM
// clause ("SELECT * FROM orders o"), or the container of a JSON envelope
// {"container":…,"query":…,"partitionKey":…,"parameters":…} that also
// scopes the query to one logical partition. Cosmos SQL has no writes;
// edits go through the Mutator (id plus partition key, replace with an
// ETag match). Read-only is a hard guard: a fail-closed lexer that admits
// one SELECT and refuses UDF calls (guard.go). Stored procedures and
// triggers are never exposed.
package cosmos
