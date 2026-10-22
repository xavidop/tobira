#!/usr/bin/env bash
# Proves check_root_deps.sh's denylist catches each driver's own SDK: run
# against a driver module, the check must fail and name that SDK. A driver
# whose SDK is missing from the denylist fails this test.
set -euo pipefail

cd "$(dirname "$0")/../.."
pairs=(
	"drivers/postgres:github.com/jackc/pg"
	"drivers/mysql:github.com/go-sql-driver/"
	"drivers/sqlite:modernc.org/sqlite"
	"drivers/mongodb:go.mongodb.org/"
	"drivers/redis:github.com/redis/"
	"drivers/clickhouse:github.com/ClickHouse/"
	"drivers/qdrant:github.com/qdrant/"
	"drivers/influxdb:github.com/influxdata/"
	"drivers/elasticsearch:github.com/elastic/"
	"drivers/dynamodb:github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"drivers/bigquery:cloud.google.com/go/bigquery"
	"drivers/cosmos:github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
	"drivers/pinecone:github.com/pinecone-io/"
	"drivers/weaviate:github.com/weaviate/"
	"drivers/milvus:github.com/milvus-io/"
	"drivers/mssql:github.com/microsoft/go-mssqldb"
	"drivers/oracle:github.com/sijms/go-ora"
	"drivers/cassandra:github.com/apache/cassandra-gocql-driver"
	"drivers/snowflake:github.com/snowflakedb/gosnowflake"
	"drivers/vertica:github.com/vertica/vertica-sql-go"
	"drivers/firestore:cloud.google.com/go/firestore"
	"drivers/duckdb:github.com/duckdb/duckdb-go"
)
failed=0
for p in "${pairs[@]}"; do
	mod="${p%%:*}" sdk="${p#*:}"
	if out="$(bash .github/scripts/check_root_deps.sh "$mod" 2>&1)"; then
		echo "check-deps selftest: $mod passed, but its SDK $sdk must be refused" >&2
		failed=1
	elif ! grep -qF "  $sdk" <<<"$out"; then
		echo "check-deps selftest: $mod was refused, but not for $sdk" >&2
		failed=1
	fi
done
[ "$failed" -eq 0 ] || exit 1
echo "check-deps selftest: the denylist catches every driver SDK (${#pairs[@]} drivers)"
