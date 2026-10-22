// Package firestore is the tobira driver for Cloud Firestore (native mode),
// built on cloud.google.com/go/firestore. Importing it registers the
// "firestore" driver in driver.Default for the firestore scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/firestore/
//
// Firestore has no query language, so a query is one small JSON request,
// such as {"collection":"orders","where":[["status","==","paid"]],"limit":10}
// or {"document":"orders/o1"}. The request format only reads: its keys are
// an allow-list, and anything else, every write verb included, is refused
// (ErrReadOnly on a read-only call). Edits go through the Mutator.
package firestore
