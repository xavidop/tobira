#!/usr/bin/env bash
# Fails if the module in <moddir> (default: .) depends, directly or
# transitively, on a database SDK or on Genkit. The root module must stay
# driver-free, and Genkit (whose go.mod requires database SDKs) lives only in
# the ai/ module.
set -euo pipefail

moddir="${1:-.}"
denylist='^(modernc\.org/sqlite|github\.com/mattn/go-sqlite3|github\.com/jackc/pg|github\.com/lib/pq|github\.com/go-sql-driver/|go\.mongodb\.org/|github\.com/redis/|github\.com/ClickHouse/|github\.com/gocql/|github\.com/qdrant/|github\.com/influxdata/|github\.com/elastic/|github\.com/neo4j/|github\.com/aws/aws-sdk-go-v2/service/dynamodb|cloud\.google\.com/go/bigquery|github\.com/Azure/azure-sdk-for-go/sdk/data/azcosmos|github\.com/pinecone-io/|github\.com/weaviate/|github\.com/milvus-io/|github\.com/marcboeker/go-duckdb|github\.com/duckdb/duckdb-go|github\.com/microsoft/go-mssqldb|github\.com/sijms/go-ora|github\.com/apache/cassandra-gocql-driver|github\.com/snowflakedb/gosnowflake|github\.com/vertica/vertica-sql-go|cloud\.google\.com/go/firestore|github\.com/firebase/genkit)'

mods="$(cd "$moddir" && GOWORK=off go list -m all)"
offenders="$(printf '%s\n' "$mods" | awk '{print $1}' | grep -E "$denylist" || true)"

if [ -n "$offenders" ]; then
	echo "root module must not depend on database SDKs or Genkit; found:" >&2
	printf '  %s\n' $offenders >&2
	exit 1
fi
echo "check-deps: $moddir is free of database SDKs and Genkit"
