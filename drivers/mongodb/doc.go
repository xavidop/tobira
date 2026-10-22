// Package mongodb is the tobira driver for MongoDB, built on
// go.mongodb.org/mongo-driver/v2. Importing it registers the "mongodb"
// driver in driver.Default for the mongodb and mongodb+srv schemes.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/mongodb/
//
// A query is one database command document in JSON or extended JSON, such
// as {"find":"orders","filter":{"status":"paid"},"limit":10}, run with
// runCommand. Read-only is a hard guard: a fail-closed allow-list of
// commands (guard.go) checked against the parsed document, which is exactly
// what is sent. An aggregate with $out or $merge anywhere in it is refused.
package mongodb
