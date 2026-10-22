// Package tools defines the Genkit tools the assistant calls: the schema
// tools (list_connections, list_namespaces, list_collections, describe),
// the read tools (sample, query, explain), similar, which is offered only
// where a reachable connection's driver declares driver.CapVectorSearcher
// (VectorSearchable), and execute, which is kept apart.
//
// A Toolset is built without a Genkit instance or the flows, so the MCP
// server can reuse it. Its tools are dynamic (ai.NewTool): Genkit registers
// them per Generate call in a child registry, which lets every panel session
// own a Toolset (and its remembered row consent) without clashing names in
// the shared registry. A turn's callbacks go on its context (WithEvents),
// so one Toolset serves every turn of its session; a consent prompt is
// shared only by the calls of the turn that opened it.
//
// What the tools guarantee:
//   - Read tools (similar included) always run with ReadOnly, the row limit (MaxRows, at most
//     RowCap: 200 unless raised, as the MCP server does to 1000) and a 30 s
//     timeout, and only on connections whose driver declares
//     driver.CapReadOnly. Other connections get the schema tools only.
//   - A turn reaches only the connections it is allowed: ToolDeps.Connections
//     (nil means all), narrowed by WithConnections, which the assistant sets
//     to the turn's scope. Any other name is refused exactly like a name that
//     is not configured, and list_connections leaves it out.
//   - Row data obeys the RowPolicy (ai.send_rows). similar's k defaults to
//     10 and is capped at MaxRows like a row limit. A vector value is sent
//     as its first 8 components and "…"; the query vector is reported
//     as "<vector dim=N>" and recorded in history as "vector dim=N".
//   - Errors obey it too. When rows may not be sent for a call (never, or
//     ask without a yes), a statement error is reduced to its kind, a fixed
//     text for that kind and its native code, for example "not found:
//     object not found; use describe/list tools to check names (SQLSTATE
//     42P01)". Driver messages can carry row values, quoted or not
//     ("operator does not exist: <value>"), so none of their text is kept,
//     with one exception: pure syntax errors (postgres 42601, mysql 1064
//     and 1149, sqlite "near ..."/"incomplete input"/"syntax error") keep
//     the text before their first quote character, which cites only the
//     model's own query. With rows allowed, the full redacted error is
//     returned.
//
// What send_rows: never does not do: it limits row data, but it is not a
// cryptographic boundary against an adversarial model that can run many
// queries. Some signals still reach the model, each worth about one bit per
// call (the count a little more): the row count of a withheld result
// (rows_withheld), the truncated flag, the timing of a call, and whether a
// statement failed or succeeded. A model that runs "select 1 from t where col like 'a%'"
// often enough can learn values that way. Choose never to keep row data out
// of normal answers, and keep secrets away from connections a model can
// query when the model itself is not trusted.
//
// The other guarantees:
//   - Every string that leaves a tool goes through the connection's
//     redactor, then ToolDeps.Redact.
//   - A failure is a tool result with an "error" field that the model can
//     react to; only a cancelled turn is returned as an error.
package tools
