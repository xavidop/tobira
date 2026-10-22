---
layout: ../../../../layouts/DocsLayout.astro
title: "Firestore: browsing, editing and watching"
---

# Firestore: browsing, editing and watching

How tobira lists, browses, edits and watches Firestore collections. For connecting, see
[Cloud Firestore](/docs/drivers/firestore/); for the request format, see
[queries and types](/docs/drivers/firestore/queries/).

## Collections

- The database is the one namespace. tobira cannot list a project's databases; pick one with
  `database=` in the DSN.
- The root collections are listed, sorted, at most 10,000.
- Subcollections are not listed, since finding them means visiting every document. Open one by
  its path, such as `users/ada/orders`, in browse, describe, a query or an edit.

## Describe

Describe reads the collection's first 100 documents (in id order, for at most 5 seconds and
32 MiB) and infers the fields:

- nested fields for maps, and for maps inside arrays;
- mixed types shown as unions such as `integer|string`;
- nullable for a field some documents lack;
- a field holding both integers and doubles is a float (JavaScript clients store whole numbers as
  integers).

The key is `_id`. A collection without documents does not exist in Firestore, so describing it
reports "not found".

## Browsing

A page is a query: filters become `where` clauses, and the sort becomes `orderBy`, with ties broken
by document id. Unsorted pages come in document-id order. A page is 100 documents by default, at
most 1000.

- **Filters**: equals, not equals, less or greater than, in a list, is null, is not null, and
  starts with on a string. Contains is not supported: Firestore has no substring match.
- A filter column is one top-level field; a name with dots is one field. `_id` is the document id
  and `_id_field` the stored field `_id`. A name holding a backquote is a field path as in a
  request, so ``"`address`.city"`` filters a nested field.
- A **raw filter** is a JSON list of `[field, op, value]`, as in a request's `where`. It is
  combined with the other filters.
- **Firestore's rules apply.** A sorted page leaves out documents that lack the sort field, and
  `!=`, `not-in` and ranges leave out documents that lack the field. A sort on one field with a
  filter on another may need a composite index.
- **Paging** continues after the page's last document, read again. If that document was deleted
  meanwhile, the next page reports "not found": start again from the first page.

## Editing

An edit names a document by `{"_id": id}`. Any other key is matched with equality filters: exactly
one document must match, and the write then requires that document to be unchanged since it was
read. Otherwise the edit fails with "not found" or a conflict.

- **Insert** creates the document. A string `_id` names it; without one, Firestore picks a random
  id. It never replaces a document: an id in use is a conflict.
- **Update** sets the changed fields. Each change is one top-level field, so a name with dots is
  one field, not a path. Setting a field to NULL stores null. `_id` cannot change. Updating a
  missing document reports "not found".
- **Delete** requires the document to exist.

Edits are refused on a read-only connection.

## Watching

tobira can follow a collection or a single document through a Firestore snapshot listener.

- In the TUI, `W` (or `:watch`) opens a [live tail](/docs/tui/#live-tail) of the open collection.
- `tobira watch <connection> <collection-path>` streams the events as JSON lines. A document path,
  such as `users/ada`, watches one document.
- Each change after the first snapshot is an event: `insert`, `update` or `delete`, with the
  collection path, the `_id` and the document's fields (its last fields for a delete).
- If the listener fails, the error is the last event.
