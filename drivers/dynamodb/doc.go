// Package dynamodb is the tobira driver for Amazon DynamoDB, built on
// aws-sdk-go-v2. Importing it registers the "dynamodb" driver in
// driver.Default for the dynamodb scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/dynamodb/
//
// A query is one PartiQL statement, run with ExecuteStatement; the driver
// never batches statements (BatchExecuteStatement) or runs them as a
// transaction (ExecuteTransaction). Read-only is a hard guard: a
// fail-closed lexer allows a single SELECT statement and nothing else
// (guard.go). Browse is a Scan paged by LastEvaluatedKey, with conditions
// as a FilterExpression. Edits are PutItem, UpdateItem and DeleteItem by
// primary key, with existence conditions. Watch reads DynamoDB Streams.
// Credentials come from the AWS default chain, a profile, or an access key
// id with its secret through a password reference; they are never logged
// and are masked in errors.
package dynamodb
