---
layout: ../../../layouts/DocsLayout.astro
title: "Security: network credentials"
---

# Security: network credentials

tobira follows two rules for every driver that speaks HTTP, so a credential never reaches a host
you did not name and never crosses the network in clear by accident: it never follows a redirect,
and it needs an explicit opt-in for plain text to another machine.

## Quick start

TLS is on by default for any host that is not this machine. To connect in clear to a remote test
server, you need both the driver's own switch and `allow_plaintext=1`:

```text
qdrant://qdrant.internal:6334?tls=false&allow_plaintext=1
```

## The rules

- **No redirect is followed.** An HTTP client can forward credential headers to whatever host a
  server redirects to, and replay a login on a 307 or 308. tobira's HTTP drivers refuse every
  redirect instead. gRPC has no redirects. If you see a 3xx status in an error, point the DSN or
  endpoint at the address the server redirects to.
- **Plain HTTP needs an explicit opt-in.** TLS is the default for any host that is not `localhost`
  or a loopback address. Plain HTTP to another host needs `allow_plaintext=1` on top of the driver's
  own switch (`tls=false`, `scheme=http`, `protocol=http`, an `http://` endpoint or URL).
  `allow_plaintext` alone never turns TLS off. Credentials sent in clear write a warning to the
  `--debug` log.
- **Managed clouds always use TLS**, even with `allow_plaintext`. The table below lists each
  driver's cloud endpoints.
- **Azure Cosmos DB** also sends signed requests only to the account endpoint (or, for an Azure
  account, its regional endpoints in the same zone), whatever the server advertises.

## Credentials in transit

What each driver that speaks HTTP or gRPC does with a credential on the network. "Off loopback"
means TLS is on unless the host is this machine. A host in the "Always TLS" column refuses plain
text even with `allow_plaintext=1`.

| Driver | Transport | TLS by default | Plain text to a remote host needs | Always TLS, even with `allow_plaintext=1` |
|---|---|---|---|---|
| BigQuery | HTTPS | always | not possible; only emulator mode is plain, and it sends no key | Google's API endpoints |
| Chroma | HTTP | off loopback | `tls=false` + `allow_plaintext=1` | `*.trychroma.com` |
| ClickHouse (HTTP) | HTTP | with `secure=true` | `allow_plaintext=1` (without it or `secure=true`, refused) | `*.clickhouse.cloud` |
| Cloudflare D1 | HTTPS | always | `endpoint=http://…` + `allow_plaintext=1` | `*.cloudflare.com` |
| Azure Cosmos DB | HTTPS | always | `tls=false` + `allow_plaintext=1` | `*.documents.azure.com`, `*.documents.azure.cn`, `*.documents.azure.us` |
| Amazon DynamoDB | HTTPS | always | `endpoint=http://…` + `allow_plaintext=1` | `*.amazonaws.com`, `*.amazonaws.com.cn` |
| Elasticsearch / OpenSearch | HTTPS | always | `tls=false` + `allow_plaintext=1` | `*.elastic-cloud.com`, `*.cloud.es.io`, `*.elastic.cloud`, `*.es.amazonaws.com`, `*.es.amazonaws.com.cn`, `*.aoss.amazonaws.com` |
| Cloud Firestore | gRPC | always | not possible; only emulator mode is plain, and it sends no key | Google's API endpoints |
| InfluxDB | HTTP + Flight (gRPC) | off loopback | `tls=false` + `allow_plaintext=1` | `*.influxdata.com`, `*.influxdb.io` |
| libSQL / Turso | HTTP | `libsql://`, `https://`, `wss://` | `http://`, `ws://` or `?tls=0`, + `allow_plaintext=1` | `*.turso.io` |
| Milvus | gRPC | off loopback | `tls=false` + `allow_plaintext=1` | `*.zillizcloud.com`, `*.zilliz.com`, `*.zillizcloud.com.cn` |
| Pinecone | HTTPS + gRPC | always | `tls=false` + `allow_plaintext=1` | `*.pinecone.io` |
| Qdrant | gRPC | off loopback | `tls=false` + `allow_plaintext=1` | `*.cloud.qdrant.io` |
| Snowflake | HTTPS | always | `host=…&protocol=http` + `allow_plaintext=1` | `*.snowflakecomputing.com`, `*.snowflakecomputing.cn`, and no `host` |
| Weaviate | HTTP | off loopback | `scheme=http` + `allow_plaintext=1` | `*.weaviate.cloud`, `*.weaviate.network` |

None of these drivers follows a redirect with your credentials. The one exception is BigQuery with
Application Default Credentials, whose tokens are fetched by Google's own auth library.

InfluxDB and Qdrant try TLS when the DSN has no `tls=`. Against a server that speaks plain text,
the error says to add `tls=false&allow_plaintext=1`.

## Drivers with their own wire protocol

| Driver | TLS by default | Credentials without TLS |
|---|---|---|
| ClickHouse (native) | with `secure=true` | a password to a host that is not loopback needs `allow_plaintext=1`; ClickHouse Cloud (`*.clickhouse.cloud`) is refused even then |
| Cassandra | off (`tls=true` turns it on) | a password to a host that is not loopback needs `allow_plaintext=1`; Amazon Keyspaces (`*.amazonaws.com`) is refused even then |
| Vertica | `tlsmode=server-strict` off loopback, `prefer` on loopback | off loopback `tlsmode=server` (unverified), `prefer` or `none` needs `allow_plaintext=1` |
| SQL Server | go-mssqldb's default: without `encrypt=`, only the login (and so the password) is encrypted, and the server certificate is not verified | `encrypt=disable` sends everything in clear; use `encrypt=true` (or `strict`) with `TrustServerCertificate=false` for verified TLS |
| Oracle | off: go-ora uses TLS only with `SSL=true` (and then verifies the certificate unless `SSL VERIFY=false`) | Oracle's logon exchange does not send the password itself, but queries and rows travel unencrypted unless the server enforces native network encryption |

PostgreSQL, MySQL, MongoDB and Redis take their client library's TLS settings from the DSN
(`sslmode`, `tls` …). Each [driver page](/docs/drivers/) has the details.
