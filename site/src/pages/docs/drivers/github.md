---
layout: ../../../layouts/DocsLayout.astro
title: GitHub
---

# GitHub

The `github` driver connects tobira to GitHub through its REST API, with the GraphQL API as an
escape hatch. It is compiled into the `tobira` binary. A repository is a namespace, and its issues,
pull requests, comments, commits, branches, tags, releases, labels, milestones and workflow runs are
collections with fixed columns. You browse and filter them, query with GitHub's own parameters or
its search syntax, explain what a query would cost, and edit issues, pull requests, comments,
labels, milestones and releases.

## Quick start

```sh
tobira query "github://xavidop/tobira" '{"collection":"issues","state":"open","limit":20}' --read-only
```

Or add it to the [config file](/docs/configuration/):

```yaml
connections:
  - name: tobira-repo
    driver: github
    dsn: github://xavidop/tobira
    password: env:GITHUB_TOKEN
    read_only: true
```

## Connecting

```text
github://xavidop/tobira                                    # one repository
github://xavidop                                           # every repository of a user or organization
github://                                                  # the token's own repositories
github://acme/app?endpoint=https://ghe.example.com/api/v3  # GitHub Enterprise Server
```

- **One repository** (`owner/repo`) is the default namespace, and Open checks that it exists.
- **An owner** lists that user's or organization's repositories as namespaces, up to 1000.
- **No owner** lists the repositories the token owns, collaborates on or reaches through its
  organizations; it needs a token.
- Unknown parameters, a user name, a port or a longer path are refused. The connection's
  `options:` may set `endpoint`, `allow_plaintext` and `timeout`.

| Parameter | Default | Meaning |
| --- | --- | --- |
| `endpoint` | `https://api.github.com` | the REST API base; `https://host/api/v3` on a GitHub Enterprise Server (GraphQL is found next to it) |
| `allow_plaintext` | `false` | allow an `http://` endpoint on a host that is not this machine |
| `timeout` | `30s` | the time one request may take |

- **Token.** Put a personal access token (fine-grained or classic) or an app token in the
  connection's `password:` reference or `TOBIRA_CONN_<NAME>_PASSWORD` (see
  [Passwords and secrets](/docs/secrets/)). It may also sit in the DSN as `github://:TOKEN@owner/repo`;
  tobira masks it wherever it prints a DSN, and it never appears in errors. A `token=` parameter is
  refused because it would be printed. No environment variable is read.
- **Without a token** only public data is visible and GitHub allows 60 requests an hour per
  address; tobira notes this in the `--debug` log.
- **TLS.** GitHub's own hosts (`github.com`, `githubusercontent.com`) are always TLS. An `http://`
  endpoint on a host that is not this machine needs `allow_plaintext=1`, and sending a token there
  writes a warning to the `--debug` log. tobira never follows an HTTP redirect, so the token cannot
  be forwarded to another host.
- **Permissions.** Reading public repositories needs no scope. Private repositories, and every
  edit, need the token's access to the repository (for a fine-grained token: Issues, Pull requests,
  Contents and Actions as read or read-and-write).

## Capabilities

| Capability | Support |
| --- | --- |
| Query, repositories, collections, describe | yes; repositories are namespaces, the ten resource kinds are collections, columns are fixed |
| Browse | yes; page cursors; the filters GitHub offers run on the server, the rest in tobira; sorting where GitHub sorts |
| Explain | yes; the HTTP requests a query would send and their rate-limit cost, without sending them |
| Completion | yes; ops, members, collections, parameters and repository names |
| Editing | yes; issues (create, edit, close), pull requests (create, edit), comments, labels, milestones and releases (create, edit, delete) |
| Transactions, vector search, time series, native export, watching | no |

## Collections

Every repository has the same ten collections. Each row is one item with the columns below; the
rest of GitHub's JSON is reachable with a `rest` query. Times are UTC. Key columns are in bold.

| Collection | Columns | Editable |
| --- | --- | --- |
| `issues` | **number**, title, state, state_reason, user, labels, assignees, milestone, comments, locked, author_association, created_at, updated_at, closed_at, body, html_url | title, body, state, state_reason, labels, assignees, milestone |
| `pull_requests` | **number**, title, state, draft, user, head, base, labels, assignees, requested_reviewers, milestone, merged_at, merge_commit_sha, created_at, updated_at, closed_at, body, html_url | title, body, state, base (create: title, head, base, body, draft) |
| `issue_comments` | **id**, issue, user, body, author_association, created_at, updated_at, html_url | body (create: issue, body); delete |
| `commits` | **sha**, message, author, author_name, author_email, author_date, committer, committer_name, committer_date, parents, html_url | read-only |
| `branches` | **name**, sha, protected | read-only |
| `tags` | **name**, sha, zipball_url, tarball_url | read-only |
| `releases` | **id**, tag_name, target_commitish, name, draft, prerelease, author, assets, created_at, published_at, body, html_url | tag_name, target_commitish, name, body, draft, prerelease; delete |
| `labels` | **name**, color, description, default, id | name, color, description; delete |
| `milestones` | **number**, title, state, description, creator, open_issues, closed_issues, due_on, created_at, updated_at, closed_at, html_url | title, state, description, due_on; delete |
| `workflow_runs` | **id**, name, display_title, run_number, run_attempt, event, status, conclusion, workflow_id, head_branch, head_sha, actor, created_at, updated_at, run_started_at, html_url | read-only |

- `user`, `author`, `actor` and the like are logins; `labels`, `assignees`, `requested_reviewers`
  and `parents` are lists; `milestone` is the milestone's title.
- GitHub's issues endpoint lists pull requests too; tobira drops them from `issues`, so a page may
  hold fewer rows than asked while the next page continues.
- `commits` lists the default branch, newest first. List another branch with
  `{"collection":"commits","sha":"release"}`.
- Describe marks the computed columns as generated and the key GitHub assigns (an issue number, a
  release id) as auto-assigned, so the inline insert asks only for what you can set.

## Queries

A query is one JSON object. `op` is `list` (the default), `get`, `search`, `repositories`, `rest`,
`graphql` or `rate_limit`. `repo` names the repository as `owner/name`; it defaults to the
query's namespace, then to the DSN's repository.

| op | What it runs | Members |
| --- | --- | --- |
| `list` | a collection's list endpoint, with its own parameters | `collection`, `repo`, `limit`, and the endpoint's parameters |
| `get` | one item by key | `collection`, `repo`, the key: `number`, `id`, `sha` or `name` (or `key`) |
| `search` | GitHub search, in its [search syntax](https://docs.github.com/search-github/searching-on-github) | `type` (`issues`, `pull_requests`, `commits`, `repositories`), `q`, `sort`, `order`, `limit` |
| `repositories` | the connection's repositories with stars, forks, language, dates | `limit` |
| `rest` | any REST endpoint under the API base | `method` (`GET` by default), `path`, `params`, `body`, `limit` |
| `graphql` | a GraphQL document | `query`, `variables` |
| `rate_limit` | the rate limits, one row per resource; the call is free | |

```json
{"collection":"issues","state":"open","labels":"bug,ui","sort":"updated","direction":"desc","limit":50}
{"collection":"commits","repo":"xavidop/tobira","sha":"main","since":"2026-10-01T00:00:00Z"}
{"collection":"workflow_runs","branch":"main","status":"failure"}
{"op":"get","collection":"pull_requests","number":46}
{"op":"search","type":"issues","q":"repo:xavidop/tobira is:open label:bug","sort":"updated"}
{"op":"search","type":"repositories","q":"terminal database language:go stars:>100"}
{"op":"rest","path":"/repos/xavidop/tobira/topics"}
{"op":"rest","method":"POST","path":"/repos/xavidop/tobira/issues/12/reactions","body":{"content":"+1"}}
{"op":"graphql","query":"{ repository(owner:\"xavidop\", name:\"tobira\") { stargazerCount } }"}
{"op":"rate_limit"}
```

- **`list` parameters** are the endpoint's own, passed as given: `state`, `labels`, `assignee`,
  `creator`, `mentioned`, `milestone`, `since`, `sort`, `direction` for issues; `state`, `head`,
  `base`, `sort`, `direction` for pull requests; `sha`, `path`, `author`, `committer`, `since`,
  `until` for commits; `protected` for branches; `state`, `sort`, `direction` for milestones;
  `actor`, `branch`, `event`, `status`, `created`, `exclude_pull_requests`, `check_suite_id`,
  `head_sha` for workflow runs; `sort`, `direction`, `since` for comments. Issues, pull requests
  and milestones list every state unless `state` says otherwise.
- **`limit`** is the rows a `list`, `search`, `repositories` or `rest` query returns: 100 unless
  given, at most 1000 (10 requests of 100). A query sends at most 10 requests and stops with a
  warning past that (an `issues` listing of a repository that is mostly pull requests can reach
  it, since pull requests are dropped after the read). `Stats.Truncated` says more existed.
  GitHub itself returns at most 1000 search results.
- **`rest`** paths are relative to the API base and may not hold a scheme, host, query or `..`;
  parameters go in `params`. An array answer is one row per element over the union of their
  members, following GitHub's pagination up to `limit`; an object is one row; a scalar is one
  `value` cell; `204 No Content` is no rows with a notice. Nested values show as JSON.
- **`graphql`** answers one row with a column per top-level field of `data`, nested values as
  JSON. Errors beside data are warnings; errors without data fail the query.
- Queries take no positional parameters.
- A result carries a notice when under a tenth of the rate limit is left.

## Browsing

- **Repositories** are the namespaces, with their descriptions. **Collections** are the ten kinds
  above; `issues` shows the repository's open issue count as its row estimate.
- **Paging** follows GitHub's pages: 100 rows unless a page size is set, at most 1000. A page
  sends at most 10 requests; `issues` always reads GitHub's pages of 100, since pull requests are
  dropped after the read. The cursor remembers the page and the position in it, and belongs to
  its filters, sorting and page size: another browse refuses it.
- **Filters GitHub applies** are sent as the endpoint's parameters: issues by `state`, `labels`
  (contains), `assignees` (equals, is null, is not null), `user` (the creator), `milestone`
  (equals, is null, is not null) and `updated_at` (greater or equal, as `since`); pull requests by
  `state`, `head` and `base`; commits by `author`, `committer`, `author_date` (greater or equal)
  and `committer_date` (less or equal); branches by `protected`; milestones by `state`; workflow
  runs by `actor`, `head_branch`, `event`, `status`, `conclusion`, `head_sha` and `created_at`;
  comments by `updated_at`.
- **Every other condition** runs in tobira over the pages it reads: equals, not equals,
  comparisons, contains (a substring of text, or a member of a list), starts with, in, is null
  and is not null, on any column. A filtered page looks at up to 1000 items or 10 requests, then
  ends early with a warning and continues on the next page. A raw filter is not supported: run a
  `list` query.
- **Sorting** is what GitHub sorts: issues by `created_at`, `updated_at` or `comments`; pull
  requests and comments by `created_at` or `updated_at`; milestones by `due_on`. Other collections
  come in GitHub's own order (newest first), and another sort is refused with the list of what
  works.

## Editing

Rows are edited one at a time, by their key. GitHub applies a write as it is: a change another
client made meanwhile is overwritten, and an item deleted meanwhile is reported as not found.

- **Issues.** Insert needs `title`; `body`, `labels`, `assignees` and `milestone` may be set.
  Edit `title`, `body`, `state` (`open`, `closed`), `state_reason`, `labels`, `assignees` and
  `milestone`. `labels` and `assignees` take a list, a JSON array or comma-separated text and
  replace the whole set; `milestone` takes a title or a number, and NULL clears it. GitHub cannot
  delete an issue through its REST API: close it instead.
- **Pull requests.** Insert needs `title`, `head` (the branch with the changes, `user:branch`
  from a fork) and `base`; `body` and `draft` may be set. Edit `title`, `body`, `state` and
  `base`. No delete.
- **Comments.** Insert needs `issue` (the issue or pull request number) and `body`. Edit `body`.
  Delete removes the comment.
- **Labels.** Insert needs `name`; `color` (hex without `#`) and `description` may be set. Editing
  `name` renames the label. Delete removes it from the repository and from every issue.
- **Milestones.** Insert needs `title`; `state`, `description` and `due_on` may be set. Delete
  removes it.
- **Releases.** Insert needs `tag_name`; `target_commitish`, `name`, `body`, `draft` and
  `prerelease` may be set. Delete removes the release, not the tag.
- Commits, branches, tags and workflow runs are read-only.
- A computed column (`user`, `comments`, `created_at`…) cannot be edited; the error names what can.

## Explain

Explain lists the HTTP requests a query would send, with the method and the full URL of each
page, and what they cost: a `list` reads up to `limit / 100` pages against the core rate limit
(5,000 requests an hour with a token, 60 without), a `search` counts against the search limit
(30 a minute), a `graphql` query against the GraphQL point budget, and `rate_limit` is free. Nothing
is sent, so analyze is not supported, and a write refused read-only is refused here too.

## Read-only guard

On a read-only connection (`read_only: true`, or `--read-only` on one command), and for a query
sent read-only, tobira refuses before anything is sent:

- a `rest` query with a method other than `GET`;
- a `graphql` document that is not plainly a query: it must start with `{` or `query` and name no
  `mutation` or `subscription` (outside comments and strings), so a document the guard cannot
  read never runs;
- a repeated key such as two `op`s, trailing text, any text that is not exactly one JSON object,
  an unknown op or member;
- every edit.

`list`, `get`, `search`, `repositories` and `rate_limit` only ever read. Browse filters become
query parameters, so they cannot write. Where you can, also use a token that cannot write
(a fine-grained token with read-only permissions), so GitHub refuses writes too.

## Rate limits and caching

GitHub allows 5,000 REST requests an hour per token (60 without one), 30 searches a minute and a
GraphQL point budget. tobira reads each page once per call, caps a query at 10 pages, and keeps the
ETag of every page it reads: a page unchanged since the last read answers `304 Not Modified`, which
costs no request. A result carries a notice when under a tenth of the limit is left; a call refused
for the limit fails with a size-limit error that says when the limit resets. `{"op":"rate_limit"}`
shows the budgets and costs nothing.

## Limits

| Limit | Value |
| --- | --- |
| Repositories listed | 1000 |
| Rows per query (`limit`) | 100 by default, at most 1000 |
| Browse page | 100 rows by default, at most 1000 |
| Items read for a filtered page | 1000 |
| Requests per query or browse page | 10 |
| Search results | 1000 (GitHub's cap) |
| Response body | 8 MiB |
| Cached pages | 128 (16 MiB) |

## Compatibility

Tested against an in-process fake of the GitHub REST API that encodes our reading of GitHub's
documentation, and, read-only, against `api.github.com` for the public `xavidop/tobira` repository
in CI. Not yet tested: editing against GitHub itself, GitHub Enterprise Server, private
repositories, organization listings of more than one page, and GitHub Apps' installation tokens.
Known differences from a database:

- the collections are fixed; what they leave out is reachable with `rest` and `graphql`;
- a list is eventually consistent with the web UI by a few seconds; a search index lags longer;
- GitHub counts the issues endpoint's open items, pull requests included, as `open_issues`.

## Troubleshooting

- **"Bad credentials"**: the token is wrong or expired; check the `password:` reference.
- **A private repository is not found**: the token lacks access to it (a fine-grained token must
  list the repository); GitHub answers 404, not 403.
- **"listing your repositories needs a token"**: without a token, name an owner or a repository in
  the DSN.
- **A size-limit error naming a reset time**: the rate limit is spent. Wait for the reset, add a
  token, or narrow the query with filters and a smaller `limit`.
- **"GitHub cannot sort issues by …"**: sort by one of the fields named in the error, or run a
  `list` query with `sort` and `direction`.
- **An issue is not found but exists on GitHub**: it is a pull request; read it from
  `pull_requests`.
- **A `rest` path is refused**: give it relative to the API base (`/repos/owner/repo/…`) with the
  parameters in `params`.
