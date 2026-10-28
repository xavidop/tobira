---
layout: ../../../layouts/DocsLayout.astro
title: GitHub
---

# GitHub

The `github` driver connects tobira to GitHub through its REST API, with the GraphQL API as an
escape hatch. It is compiled into the `tobira` binary. A repository is a namespace whose issues,
pull requests, comments, commits, branches, tags, releases, labels, milestones and workflow runs
are collections; a user or organization is a namespace too, with its repositories and its issues,
pull requests and commits across all of them. Queries are one JSON object.

## Quick start

```sh
tobira query "github://xavidop/tobira" '{"collection":"issues","state":"open","limit":20}' --read-only
```

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
github://xavidop                                           # a user or organization and its repositories
github://                                                  # the token's own login and repositories
github://acme/app?endpoint=https://ghe.example.com/api/v3  # GitHub Enterprise Server
```

| Parameter | Default | Meaning |
| --- | --- | --- |
| `endpoint` | `https://api.github.com` | the REST API base; `https://host/api/v3` on an Enterprise Server |
| `allow_plaintext` | `false` | allow an `http://` endpoint on a host that is not this machine |
| `timeout` | `30s` | the time one request may take |

- **Token.** A personal access token or app token in the connection's `password:` reference or
  `TOBIRA_CONN_<NAME>_PASSWORD` ([Passwords and secrets](/docs/secrets/)); `github://:TOKEN@…`
  works too and is masked wherever a DSN is printed. A `token=` parameter is refused. Without a
  token only public data is visible and GitHub allows 60 requests an hour.
- **TLS.** GitHub's own hosts are always TLS; redirects are never followed, so the token cannot
  leave the endpoint.
- Private repositories and every edit need the token's access to the repository.

## Capabilities

| Capability | Support |
| --- | --- |
| Query, namespaces, collections, describe | yes; fixed columns per collection |
| Browse | yes; page cursors; the filters GitHub offers run on the server, the rest in tobira; sorting where GitHub sorts |
| Explain | yes; the requests a query would send and their rate-limit cost, never sent |
| Completion | yes |
| Editing | yes; issues, pull requests, comments, labels, milestones, releases |
| Transactions, vector search, time series, export, watching | no |

## Collections

A **repository** namespace (`owner/repo`) has ten collections. Key columns are in bold; logins
such as `user` are text, `labels` and `assignees` are lists, `milestone` is a title, times are UTC.

| Collection | Columns | Editable |
| --- | --- | --- |
| `issues` | **number**, title, state, state_reason, user, labels, assignees, milestone, comments, locked, author_association, created_at, updated_at, closed_at, body, html_url | create (title, body, labels, assignees, milestone); edit title, body, state, state_reason, labels, assignees, milestone; no delete: close it |
| `pull_requests` | **number**, title, state, draft, user, head, base, labels, assignees, requested_reviewers, milestone, merged_at, merge_commit_sha, created_at, updated_at, closed_at, body, html_url | create (title, head, base, body, draft); edit title, body, state, base |
| `issue_comments` | **id**, issue, user, body, author_association, created_at, updated_at, html_url | create (issue, body); edit body; delete |
| `commits` | **sha**, message, author, author_name, author_email, author_date, committer, committer_name, committer_date, parents, html_url | read-only |
| `branches` | **name**, sha, protected | read-only |
| `tags` | **name**, sha, zipball_url, tarball_url | read-only |
| `releases` | **id**, tag_name, target_commitish, name, draft, prerelease, author, assets, created_at, published_at, body, html_url | create, edit, delete |
| `labels` | **name**, color, description, default, id | create, edit (renaming), delete |
| `milestones` | **number**, title, state, description, creator, open_issues, closed_issues, due_on, created_at, updated_at, closed_at, html_url | create, edit, delete |
| `workflow_runs` | **id**, name, display_title, run_number, run_attempt, event, status, conclusion, workflow_id, head_branch, head_sha, actor, created_at, updated_at, run_started_at, html_url | read-only |

An **owner** namespace (`xavidop`, `acme`) has four, read through GitHub search except the first:

| Collection | Columns | Notes |
| --- | --- | --- |
| `repositories` | **full_name**, owner, name, private, fork, archived, description, language, default_branch, stars, forks, open_issues, topics, pushed_at, created_at, updated_at, html_url | read-only |
| `issues`, `pull_requests` | the issue columns plus **repository** | keyed by repository and number; at most 1000 results; edits go to the repository named in `repository` |
| `commits` | the commit columns plus **repository** | read-only; at most 1000 results; GitHub's commit search needs text, so browse with a `message contains` filter or list with words in `q` |

GitHub's issues endpoint lists pull requests too; tobira drops them from `issues`. `commits` lists
the default branch; `{"collection":"commits","sha":"release"}` lists another.

## Queries

One JSON object. `op` is `list` (the default), `get`, `search`, `repositories`, `rest`, `graphql`
or `rate_limit`. `repo` is `owner/name` or an owner; it defaults to the query's namespace, then the
DSN's.

```json
{"collection":"issues","state":"open","labels":"bug,ui","sort":"updated","direction":"desc","limit":50}
{"collection":"issues","repo":"acme","q":"label:bug","sort":"updated","order":"desc"}
{"op":"get","collection":"pull_requests","number":46}
{"op":"get","collection":"issues","repo":"acme","repository":"app","number":12}
{"op":"search","type":"issues","q":"repo:xavidop/tobira is:open label:bug","sort":"updated"}
{"op":"rest","method":"POST","path":"/repos/xavidop/tobira/issues/12/reactions","body":{"content":"+1"}}
{"op":"graphql","query":"{ repository(owner:\"xavidop\", name:\"tobira\") { stargazerCount } }"}
{"op":"rate_limit"}
```

| op | Members |
| --- | --- |
| `list` | `collection`, `repo`, `limit`, and the endpoint's own parameters: issues `state`, `labels`, `assignee`, `creator`, `mentioned`, `milestone`, `since`, `sort`, `direction`; pull requests `state`, `head`, `base`, `sort`, `direction`; commits `sha`, `path`, `author`, `committer`, `since`, `until`; branches `protected`; milestones `state`, `sort`, `direction`; workflow runs `actor`, `branch`, `event`, `status`, `created`, `exclude_pull_requests`, `check_suite_id`, `head_sha`; comments `sort`, `direction`, `since`. An owner's `issues`, `pull_requests` and `commits` take `q` (extra [search qualifiers](https://docs.github.com/search-github/searching-on-github)), `sort` and `order`; `repositories` take `type`, `sort`, `direction` |
| `get` | `collection`, `repo`, the key (`number`, `id`, `sha`, `name` or `full_name`); in an owner, `repository` too |
| `search` | `type` (`issues`, `pull_requests`, `commits`, `repositories`), `q`, `sort`, `order`, `limit` |
| `repositories` | `limit`: the connection's repositories with their details |
| `rest` | `method` (`GET` by default), `path` relative to the API base, `params`, `body`, `limit`; an array answer is one row per element over the union of their members, paginated; an object is one row |
| `graphql` | `query`, `variables`; one row with a column per top-level field of `data` |
| `rate_limit` | none; one row per resource, and the call is free |

`limit` is 100 unless given, at most 1000; a query sends at most 10 requests and stops with a
warning past that. `Stats.Truncated` says more existed. Queries take no positional parameters.

## Browsing

- **Paging** follows GitHub's pages (100 rows unless a page size is set, at most 1000, at most 10
  requests per page). A cursor belongs to its filters, sort and page size.
- **Filters GitHub applies** go to the server: in a repository, issues by `state`, `labels`,
  `assignees` (also is null and is not null), `user`, `milestone` and `updated_at ≥`; pull requests
  by `state`, `head`, `base`; commits by `author`, `committer`, `author_date ≥`, `committer_date ≤`;
  branches by `protected`; milestones by `state`; workflow runs by `actor`, `head_branch`, `event`,
  `status`, `conclusion`, `head_sha`, `created_at`. In an owner, issues and pull requests by
  `state`, `labels`, `user`, `assignees`, `milestone`, `repository`, `comments` and the dates;
  commits by `message` (the search text), `author`, `committer`, `author_name`, `author_email`,
  `author_date`, `committer_date`. A `repository` filter, or `repo:` in `q`, replaces the owner
  qualifier, since GitHub ORs `user:`, `org:` and `repo:` together.
- **Every other condition** runs in tobira over the pages read (equals, comparisons, contains,
  starts with, in, is null). A filtered page looks at up to 1000 items or 10 requests, then ends
  early with a warning and continues on the next page.
- **Sorting** is what GitHub sorts: issues by `created_at`, `updated_at`, `comments`; pull requests
  and comments by `created_at`, `updated_at`; milestones by `due_on`; an owner's repositories by
  `created_at`, `updated_at`, `pushed_at`, `full_name`; an owner's commits by `author_date`,
  `committer_date`. Anything else is refused with the list of what works.

## Editing

Rows are edited by key; GitHub applies a write as it is, so a concurrent change is overwritten.
`labels` and `assignees` take a list, a JSON array or comma-separated text and replace the whole
set; `milestone` takes a title or a number, NULL clears it; a new comment needs `issue`; renaming
a label is editing `name`. In an owner namespace, a new issue or pull request needs `repository`.
Commits, branches, tags, workflow runs and an owner's repositories are read-only.

## Read-only guard

On a read-only connection, and for a query sent read-only, tobira refuses before anything is sent:
a `rest` with a method other than `GET`; a `graphql` document that is not plainly a query (it must
start with `{` or `query` and name no `mutation` or `subscription`); duplicate keys, trailing text,
unknown ops or members; and every edit. The other ops only ever read, and browse filters become
query parameters. Where you can, also use a token that cannot write.

## Rate limits

GitHub allows 5,000 REST requests an hour per token (60 without one), 30 searches a minute and a
GraphQL point budget. tobira keeps the ETag of every page it reads, so a page unchanged since the
last read costs no request; a result carries a notice when under a tenth of the limit is left, a
call refused for the limit fails with the reset time, and `{"op":"rate_limit"}` shows the budgets.
Explain shows a query's requests and their cost without sending them.

## Compatibility

Tested against an in-process fake of the GitHub REST API, and read-only against `api.github.com`
for `xavidop/tobira` in CI. Not yet tested: editing against GitHub itself, GitHub Enterprise
Server, private repositories and GitHub Apps' installation tokens. What the collections leave
out is reachable with `rest` and `graphql`; a listing is eventually consistent with the web UI,
and a search lags longer.

## Troubleshooting

- **"Bad credentials"**: the token is wrong or expired.
- **A private repository is not found**: the token lacks access to it; GitHub answers 404.
- **"listing your repositories needs a token"**: name an owner or a repository in the DSN.
- **A size-limit error naming a reset time**: the rate limit is spent; wait, add a token, or
  narrow the query.
- **"a github query is one JSON object"**: queries are JSON, not SQL; see the examples above.
- **An issue is not found but exists**: it is a pull request; read it from `pull_requests`.
