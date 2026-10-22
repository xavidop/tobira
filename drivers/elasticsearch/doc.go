// Package elasticsearch is the tobira driver for Elasticsearch and
// OpenSearch, built on github.com/elastic/go-elasticsearch/v8. Importing it
// registers two drivers in driver.Default: "elasticsearch" for the
// elasticsearch scheme and "opensearch" for the opensearch scheme.
//
// The full reference is https://xavidop.github.io/tobira/docs/drivers/elasticsearch/
//
// Query takes a JSON search request, {"index": "x", ...search body}, run
// against x/_search, or a raw request {"method", "path", "body"}. On a
// read-only path the raw form is limited to an allow-list of read
// endpoints and methods, and any key containing "script" is refused. Hits
// are rows: _id, _score, then one column per source field, typed from the
// index mapping.
package elasticsearch
