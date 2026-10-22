---
layout: ../../../../layouts/DocsLayout.astro
title: Cloud Firestore
---

# Cloud Firestore

The `firestore` driver connects tobira to Cloud Firestore (Firebase, native mode) through the
official Go client. It is compiled into the `tobira` binary and talks gRPC to Google or to the
Firestore emulator. Firestore has no query language, so you query with a small JSON request
format; you can also browse and edit collections and watch them change.

This page covers connecting and the read-only guard. Two more pages cover the rest:

- [Queries and types](/docs/drivers/firestore/queries/): the JSON request format, value literals
  and how Firestore values show in tobira.
- [Browsing, editing and watching](/docs/drivers/firestore/browsing/): collections, describe,
  filters, paging, edits and snapshot listeners.

## Quick start

```sh
tobira query "firestore://my-project" \
  '{"collection":"orders","where":[["status","==","paid"]],"limit":10}' --read-only
```

Or add it to the [config file](/docs/configuration/), with the service-account key as the
password:

```yaml
connections:
  - name: app
    driver: firestore
    dsn: firestore://my-project
    password: file:///home/me/.config/gcp/viewer-key.json
    read_only: true
```

## Connecting

```text
firestore://my-project
firestore://my-project?database=orders
firestore://demo-project?emulator=localhost:8080
```

| Part | Meaning |
| --- | --- |
| host | the Google Cloud project id |
| `database` | the database id; `(default)` when left out |
| `emulator=host:port` | talk plain gRPC to a Firestore emulator; no credentials are sent |

Any other parameter is refused, and so is a DSN carrying a user, a password or a credential-like
parameter (`credentials`, `key`, `token` …). Those errors never quote the DSN.

### Credentials

Put the service-account key (its JSON) in the connection's `password:` reference, never in the DSN
(see [Passwords and secrets](/docs/secrets/)). Only a key of `"type": "service_account"` whose
private key parses is accepted. Other credential files are refused, because some of them make the
auth library run a program or call arbitrary URLs. Errors never contain the key.

Without a password, the client library's Application Default Credentials are used:
`GOOGLE_APPLICATION_CREDENTIALS`, `gcloud auth application-default login` or the metadata server.
ADC accepts any credential type, including an `external_account` file that runs a program or
calls URLs. That file is your own environment's choice; use a service-account key as the password
where you want the stricter check.

### TLS and the emulator

- Connections to Google always use TLS. There is no switch to turn it off.
- `emulator=` is the only plain-text mode. It sends the emulator's admin token, which bypasses
  security rules, and never sends your key.
- The client library also reads `FIRESTORE_EMULATOR_HOST` and, when it is set, sends every call to
  that host in plain text. So tobira refuses to open a DSN without `emulator=` while the variable
  is set, and a DSN whose `emulator=` names another address. When the two agree, the connection
  goes to that emulator.
- tobira waits at most 10 seconds for Firestore's first answer, which needs only list access.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, database, collections, describe | yes; a query is one JSON request; describe samples 100 documents |
| Browse | yes; `where` filters and `orderBy`, paged after the last document |
| Editing | yes; create, update and delete of one document, by `_id` (or by field values) |
| Completion | yes; request keys, operators and root collections; cached 60 s |
| Watching | yes; snapshot listeners on a collection or a document |
| Explain, transactions, native export | no |

A Firestore transaction is a callback the client retries for at most 270 seconds while it locks
what it reads, so tobira offers no `BEGIN`/`COMMIT` for it.

## Read-only guard

A request can only read: its keys are an allow-list, each maps to a read, and the whole text is
parsed before anything is sent. On a read-only connection (`read_only: true`, or `--read-only` on
one command) these are refused, failing closed:

- any key outside the [request keys](/docs/drivers/firestore/queries/#request-keys), including
  every write verb (`set`, `update`, `delete`, `create`, `add`, `batch`, `commit`, `transaction`,
  `data` …) and any other spelling (`Collection`);
- a repeated key, trailing data, or any text that is not exactly one JSON object.

On a writable connection a write verb is refused too, with a hint that edits go through the
editor. Edits are refused on a read-only connection before any call. A browse raw filter is a
`where` list, which cannot write.

Firestore has no read-only protocol mode. Give read-only connections a key whose role can only
read (`roles/datastore.viewer`), so Google refuses writes as well.

## Compatibility

Tested with the Firestore emulator. Not yet tested against Google Cloud. Known differences from
Google:

- the emulator takes no credentials, so authentication errors have not been tested;
- it enforces no composite indexes and no security rules;
- it cannot order by document id descending, which Google can.

## Troubleshooting

- **"FIRESTORE_EMULATOR_HOST is set, so the client library would send this connection to that
  emulator instead of Google"**: unset the variable, or add `emulator=host:port` to the DSN.
- **"credentials must not be in the DSN"**: move the key into the connection's `password:`
  reference.
- **"the password must be a service-account key"**: the password holds another credential type.
  Create a service-account key in the Google Cloud console and use its JSON file.
- **"the query needs a composite index"**: Firestore's message includes a link that creates the
  index. Open it, wait for the index to build, then run the query again.
- **"no answer from Firestore within 10s"**: check the project id and network access to Google,
  or that the emulator is running at the `emulator=` address.
- **A subcollection is not listed**: only root collections are. Open it by its path, such as
  `users/ada/orders`.
