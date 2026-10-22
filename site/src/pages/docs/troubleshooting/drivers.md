---
layout: ../../../layouts/DocsLayout.astro
title: "Troubleshooting: driver connections"
---

# Troubleshooting: driver connections

The problems people hit first when they connect to each database, and how emulators differ from the
real services. Each [driver page](/docs/drivers/) has the full DSN reference.

## By driver

| Driver | Problem and fix |
|---|---|
| SQL Server | In a URL the path is the instance name, and the database is `?database=`. go-mssqldb's `encrypt` and `TrustServerCertificate` pass through, so a self-signed server certificate needs one of them. On a read-only connection with a database tobira asks for `ApplicationIntent=ReadOnly`. |
| Oracle | The path is the service name (`/FREEPDB1`). `TRACE FILE`, `TRACE DIR`, `LOB FETCH=STREAM` and a `PASSWORD` inside `connStr` are refused. ORA-01466 on a read-only call means the table was created a few seconds before; retry after a moment. |
| Cassandra | A password is sent only to the known password authenticators. Behind a proxy or NAT, set `discovery=false`. TLS needs `tls=true`, with `tls_ca` for a private CA; a password to a remote host without TLS also needs `allow_plaintext=1`. |
| Vertica | Off loopback the default is `tlsmode=server-strict`, so a server without a verifiable certificate fails the handshake; `tlsmode=server`, `prefer` or `none` to such a host also needs `allow_plaintext=1`, and tobira logs a warning for `prefer` and `none`. A DSN that carries a password is refused: put it in the connection's password. |
| Snowflake | The host is the account identifier (`orgname-accountname`), not the full `snowflakecomputing.com` name. `authenticator=` says what the password holds (password, a PEM key for `snowflake_jwt`, an OAuth or programmatic token); browser SSO, Okta and MFA prompts are refused. |
| BigQuery and Cloud Firestore | The password must be a service-account key JSON (`"type": "service_account"`); other credential files are refused. Without one, Application Default Credentials are used. Firestore refuses to open while `FIRESTORE_EMULATOR_HOST` is set unless the DSN names the same address with `emulator=`, so unset it for a connection meant for Google. A BigQuery query over `max_bytes_billed` (1 GiB by default) fails instead of running. |
| Amazon DynamoDB | Without a region in the DSN, `AWS_REGION` or the profile must give one. Open fails when the AWS default chain finds no credentials. |
| Azure Cosmos DB | The password is the account key; Microsoft Entra ID sign-in is not supported. `tls=false` needs `allow_plaintext=1` unless the host is loopback (as with the emulator), and is refused for Azure's own endpoints. |
| libSQL / Turso and Cloudflare D1 | A token in a query parameter is refused; put it in the password. Plain HTTP to a remote host (libSQL `http://`, `ws://` or `?tls=0`, a D1 `endpoint=http://…`) also needs `allow_plaintext=1`, and is refused for `*.turso.io` and `*.cloudflare.com`. |
| Pinecone, Weaviate, Chroma, Milvus, Qdrant and InfluxDB | The API key or token goes in the password. Remote hosts use TLS by default. Plain text to a remote host needs the driver's switch (`tls=false`, or Weaviate's `scheme=http`) **and** `allow_plaintext=1`, and is refused toward the vendor's cloud even then. Milvus keeps port 19530 with TLS, so name the Zilliz port. |
| A TLS handshake error (InfluxDB, Qdrant) | A DSN without `tls=` uses TLS to a remote server. If the server really speaks plain text, add `tls=false&allow_plaintext=1`, as the error says. |
| Elasticsearch and OpenSearch | TLS is on by default; `tls=false` to another machine needs `allow_plaintext=1`. Elasticsearch 8's own CA needs `ca_file=` (its `http_ca.crt`). |
| ClickHouse over HTTP, Snowflake and DynamoDB | Plain HTTP to a remote host (ClickHouse without `secure=true`, Snowflake `protocol=http`, a DynamoDB `endpoint=http://…`) needs `allow_plaintext=1`, and is refused for ClickHouse Cloud and the vendors' own endpoints. |
| ClickHouse over the native protocol | A password to a remote host without `secure=true` needs `allow_plaintext=1`, and is refused for ClickHouse Cloud. Add `secure=true` (port 9440). |
| A 3xx status in an HTTP error | No HTTP driver follows a redirect, so a credential cannot be forwarded to another host. Point the DSN or endpoint at the address the server redirects to. |

## Emulators and fakes

Several drivers were tested against an emulator or a local stand-in, not the real service; the
drivers page lists [what each was tested against](/docs/drivers/#what-each-driver-was-tested-against).
What works on an emulator can differ on the real service:

- Emulators accept any key (DynamoDB Local, pinecone-local, the Cosmos DB and BigQuery emulators,
  fakesnow), so an authentication error appears only against the real service.
- The Firestore emulator enforces no composite indexes and no security rules; Google may refuse a
  query that worked there and ask for an index.
- Pinecone's service is eventually consistent, so a record just written can take a moment to
  appear; pinecone-local shows it at once.
- The Cosmos DB emulator has one partition and never throttles, so request-unit and
  cross-partition limits show only on Azure.
