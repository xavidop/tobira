---
layout: ../../../layouts/DocsLayout.astro
title: Importing connections
---

# Importing connections

`tobira connections export` prints connections in the config's `connections:` shape, and
`tobira connections import` reads them back, or reads another tool's list: DBeaver, a libpq
pgpass file or TablePlus. Import is a dry run until you add `--apply`.

## Quick start

```sh
tobira connections export > conns.yaml                 # all of them; --format json, or name some
tobira connections import --from file conns.yaml       # prints the plan, writes nothing
tobira connections import --from dbeaver ~/Library/DBeaverData/workspace6/General/.dbeaver/data-sources.json --apply
tobira connections import --from pgpass --apply --keyring-yes   # $PGPASSFILE or ~/.pgpass
tobira connections import --from tableplus             # its macOS Connections.plist
```

## Export

Export never writes a password. References (`env:`, `file://`, `keyring:` and so on) are kept,
since they are not secrets. A literal password, a password inside a DSN and an option that looks
like a secret are left out, each with a note on stderr.

## Import

- **A dry run by default.** It prints one line per entry: `+ add`, `~ rename` or `- skip` with the
  reason. A taken name gets `--rename-suffix` (default `-imported`), then `-2`, `-3` and so on. A
  name changed to fit the name rule (letters, digits, `.`, `_`, `-`) shows `(from "original")`.
- **`--apply` writes once.** It adds everything in one write that keeps the file's comments, and
  fails without writing if the file changed on disk meanwhile.
- **Passwords go only to the OS keyring.** A password the source holds is never written to the
  config. With `--apply`, `--keyring-yes` stores them without asking. Otherwise a terminal asks per
  connection, and without one they are skipped. A stored password becomes
  `password: keyring:<name>`. `--no-passwords` skips them all. A dry run never touches the keyring.
- **Missing drivers.** A DBeaver, pgpass or TablePlus connection whose driver this build does not
  have is skipped with `driver not available in this build`. One from an export file is added with
  a note to install the plugin, since it may use a plugin driver.
- Each source must be a regular file of at most 16 MiB.

## Sources

| Source | What is read |
|---|---|
| `dbeaver` | `data-sources.json`. PostgreSQL, MySQL, MariaDB, SingleStore, SQLite, MongoDB, ClickHouse, Redis, SQL Server, Oracle, Cassandra, BigQuery, DynamoDB, Elasticsearch, OpenSearch, libSQL, Snowflake, DuckDB, Vertica, Redshift, CockroachDB and Greenplum connections; others are skipped with a note. A password is read from a plain `configuration.password` field, or else from the JDBC URL. DBeaver's encrypted `credentials-config.json` is never read: set those passwords with `tobira connections set-password`. |
| `pgpass` | Every `hostname:port:database:username:password` line becomes a `postgres` connection named `<hostname>-<database>`. A line with a `*` wildcard is skipped. A file the group or others can read is refused, as libpq ignores it: `chmod 0600` it first. Default: `$PGPASSFILE` or `~/.pgpass`. |
| `tableplus` | macOS, best effort, since the format is undocumented. Reads `~/Library/Application Support/com.tinyapp.TablePlus/Data/Connections.plist`, or the path you give. The same databases as DBeaver are mapped. TablePlus keeps passwords in the macOS keychain, which tobira does not read. A Cloudflare D1 connection is skipped, as its account and database ids are not in the file. |
| `file` | The YAML or JSON that `export` wrote. References are kept as they are. |

[`tobira connections import`](/docs/cli/connections/#import) lists every flag.
