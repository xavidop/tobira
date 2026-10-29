# tobira GitHub driver

The `github` driver lets [tobira](https://github.com/xavidop/tobira) browse, query and edit a
GitHub repository's issues, pull requests, comments, commits, branches, tags, releases, labels,
milestones and workflow runs as collections, through the GitHub REST API (and the GraphQL API as
an escape hatch). It is built into the `tobira` binary and has no SDK dependency.

## Connect

| Form | Meaning |
| --- | --- |
| `github://owner/repo` | one repository (the default namespace) |
| `github://owner` | a user or organization: its repositories, and its issues, pull requests and commits across all of them |
| `github://` | the token's own login and repositories, the ones it collaborates on and its organizations' |
| `github://owner/repo?endpoint=https://ghe.example.com/api/v3` | a GitHub Enterprise Server |

Put a token (a fine-grained or classic personal access token, or an app token) in the
connection's `password:` secret reference or `TOBIRA_CONN_<NAME>_PASSWORD`; `github://:TOKEN@owner/repo`
works too and is masked wherever tobira prints a DSN. A token in a query parameter is refused.
Without a token only public data is visible and GitHub allows 60 requests an hour. Parameters:
`endpoint`, `allow_plaintext` (an `http://` endpoint to a host other than this machine) and
`timeout` (per request, `30s`). GitHub's own hosts are always TLS.

## Quick example

Queries are one JSON object.

```sh
tobira query "github://xavidop/tobira" '{"collection":"issues","state":"open","limit":20}' --read-only
tobira query "github://xavidop/tobira" '{"op":"search","type":"pull_requests","q":"repo:xavidop/tobira is:open"}' --read-only
```

## Capabilities

| Capability | Support |
| --- | --- |
| Query, namespaces, collections, describe | yes; a repository has ten collections, an owner has repositories, issues, pull_requests and commits across its repositories (GitHub search) |
| Browse | yes; page cursors, the filters GitHub offers run on the server and the rest in tobira, sorting where GitHub sorts |
| Explain | yes; the HTTP requests a query would send and their rate-limit cost, never sent |
| Completion | yes; ops, members, collections, parameters, repositories |
| Editing | yes; issues (create, edit, close), pull requests (create, edit), comments, labels, milestones and releases (create, edit, delete) |
| Read-only guard | yes; `rest` runs GET only, `graphql` only plain queries, editing is refused |
| Transactions, vector search, time series, export, watch | no |

## Queries

| op | What it reads | Members |
| --- | --- | --- |
| `list` (default) | a collection, with the endpoint's own parameters | `collection`, `repo` (owner/name or an owner), `limit`, the parameters (`state`, `labels`, `sort`, `since`, `sha`, `branch`…; `q`, `sort`, `order` across an owner) |
| `get` | one item by key | `collection`, `repo`, the key (`number`, `id`, `sha`, `name` or `full_name`); `repository` across an owner |
| `search` | GitHub search | `type` (`issues`, `pull_requests`, `commits`, `repositories`), `q`, `sort`, `order`, `limit` |
| `repositories` | the connection's repositories with their details | `limit` |
| `rest` | any REST endpoint; arrays are paginated into rows | `method`, `path`, `params`, `body`, `limit` |
| `graphql` | a GraphQL document; one row with a column per top-level field | `query`, `variables` |
| `rate_limit` | the rate limits, one row per resource; costs nothing | |

`limit` defaults to 100 rows and is capped at 1000 (10 requests). `repo` defaults to the query's
namespace, then the DSN's repository.

## Tests

The conformance kit's dataset cannot be seeded into GitHub, whose collections have fixed
columns, so the driver has its own suite against an in-process fake of the REST API covering the
same ground (catalog, queries, browsing, editing, the read-only guard, errors, cancellation,
leaks and stderr). `go test -tags integration ./...` adds a read-only smoke test against
api.github.com for the public `xavidop/tobira` repository when `GITHUB_TOKEN` is set. The fake
encodes our reading of GitHub's documentation; the integration test is what has been checked
against GitHub itself.

The docs page is [GitHub](https://xavidop.github.io/tobira/docs/drivers/github/).
