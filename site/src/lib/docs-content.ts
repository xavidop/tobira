// Shared source for the /llms.txt and /llms-full.txt endpoints.
//
// These are rendered as ordinary Astro static endpoints (src/pages/*.txt.ts),
// so the files exist under `astro dev` AND `astro build` alike - unlike a
// build-only integration whose astro:build:done hook never runs in the dev
// server and 404s there. Every doc page's raw Markdown is read at build time
// via import.meta.glob so both endpoints render from one place.

export const LLMS_TITLE = "tobira";
export const LLMS_DESCRIPTION =
  "tobira is a terminal app, written in Go, to browse, query and edit many databases with the same keys " +
  "and commands. " +
  "SQLite, PostgreSQL with pgvector, MySQL, MariaDB, SingleStore, Amazon Redshift, " +
  "CockroachDB, Greenplum, SQL Server, Oracle, Snowflake, BigQuery, Vertica, libSQL / Turso, Cloudflare D1, MongoDB, " +
  "Cloud Firestore, Azure Cosmos DB, Amazon DynamoDB, Redis, Cassandra, Neo4j, ClickHouse, InfluxDB, Elasticsearch, OpenSearch, " +
  "Qdrant, Pinecone, Weaviate, Chroma, Milvus and GitHub work in the terminal UI and `tobira query` (DuckDB as an external plugin), " +
  "an AI assistant answers questions about them, `tobira mcp` serves them to agents, " +
  "and drivers can also be external plugins.";
// No absolute URL here: every link in these files is built with absoluteUrl
// from Astro's own `site` and `base`, and a domain written a second time here
// is one that goes stale the day the site moves.
export const LLMS_NOTES = "Auto-generated from the tobira documentation.";

// Every doc page, raw (frontmatter included), keyed by project-absolute path.
const rawPages = import.meta.glob("/src/pages/docs/**/*.md", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

export interface DocPage {
  /** Route-relative path, e.g. "/docs/attach". */
  path: string;
  title: string;
  /** Markdown body with the frontmatter block stripped. */
  body: string;
}

const FRONTMATTER = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?/;

function routeFor(fileKey: string): string {
  let p = fileKey.replace(/^\/src\/pages/, "").replace(/\.md$/, "");
  if (p.endsWith("/index")) p = p.slice(0, -"/index".length);
  return p || "/";
}

function titleFrom(frontmatter: string, body: string, route: string): string {
  const fm = frontmatter.match(/^title:\s*(.+?)\s*$/m);
  if (fm) return fm[1].replace(/^["']|["']$/g, "");
  const h1 = body.match(/^#\s+(.+?)\s*$/m);
  if (h1) return h1[1];
  return route.split("/").pop() || route;
}

// Pages that lead the index; everything else follows, ordered by title.
const ORDER = [
  "/docs",
  "/docs/install",
  "/docs/quickstart",
  "/docs/concepts",
  "/docs/tui",
  "/docs/tui/browsing",
  "/docs/tui/kind-views",
  "/docs/tui/commands",
  "/docs/editor",
  "/docs/editing",
  "/docs/export",
  "/docs/keymaps",
  "/docs/history",
  "/docs/connections",
  "/docs/connections/import",
  "/docs/connections/ssh",
  "/docs/secrets",
  "/docs/configuration",
  "/docs/cli",
  "/docs/cli/query",
  "/docs/cli/connect",
  "/docs/cli/connections",
  "/docs/cli/ask",
  "/docs/cli/mcp",
  "/docs/cli/history",
  "/docs/cli/driver",
  "/docs/cli/doctor",
  "/docs/cli/watch",
  "/docs/ai",
  "/docs/ai/configuration",
  "/docs/ai/using",
  "/docs/ai/privacy",
  "/docs/ai/prompts",
  "/docs/mcp",
  "/docs/mcp/clients",
  "/docs/mcp/tools",
  "/docs/mcp/http",
  "/docs/drivers",
  "/docs/drivers/redshift",
  "/docs/drivers/d1",
  "/docs/drivers/cockroachdb",
  "/docs/drivers/greenplum",
  "/docs/drivers/libsql",
  "/docs/drivers/mariadb",
  "/docs/drivers/mysql",
  "/docs/drivers/oracle",
  "/docs/drivers/postgres",
  "/docs/drivers/postgres/queries",
  "/docs/drivers/postgres/read-only",
  "/docs/drivers/postgres/listen-and-export",
  "/docs/drivers/singlestore",
  "/docs/drivers/mssql",
  "/docs/drivers/sqlite",
  "/docs/drivers/bigquery",
  "/docs/drivers/clickhouse",
  "/docs/drivers/snowflake",
  "/docs/drivers/vertica",
  "/docs/drivers/dynamodb",
  "/docs/drivers/cosmos",
  "/docs/drivers/cassandra",
  "/docs/drivers/firestore",
  "/docs/drivers/firestore/queries",
  "/docs/drivers/firestore/browsing",
  "/docs/drivers/mongodb",
  "/docs/drivers/redis",
  "/docs/drivers/elasticsearch",
  "/docs/drivers/influxdb",
  "/docs/drivers/github",
  "/docs/drivers/chroma",
  "/docs/drivers/milvus",
  "/docs/drivers/pgvector",
  "/docs/drivers/pinecone",
  "/docs/drivers/qdrant",
  "/docs/drivers/weaviate",
  "/docs/drivers/duckdb",
  "/docs/write-a-driver",
  "/docs/write-a-driver/module",
  "/docs/write-a-driver/conformance",
  "/docs/write-a-driver/plugin",
  "/docs/plugin-protocol",
  "/docs/plugin-protocol/host",
  "/docs/plugin-protocol/wire",
  "/docs/security",
  "/docs/security/read-only",
  "/docs/security/network",
  "/docs/security/ai-and-mcp",
  "/docs/security/plugins",
  "/docs/troubleshooting",
  "/docs/troubleshooting/connections",
  "/docs/troubleshooting/read-only",
  "/docs/troubleshooting/drivers",
  "/docs/troubleshooting/ai-and-mcp",
  "/docs/troubleshooting/plugins",
  "/docs/contributing",
  "/docs/roadmap",
];

export function collectDocs(): DocPage[] {
  const pages: DocPage[] = Object.entries(rawPages).map(([key, raw]) => {
    const route = routeFor(key);
    const m = raw.match(FRONTMATTER);
    const frontmatter = m ? m[1] : "";
    const body = (m ? raw.slice(m[0].length) : raw).trim();
    return { path: route, title: titleFrom(frontmatter, body, route), body };
  });
  pages.sort((a, b) => {
    const ia = ORDER.indexOf(a.path);
    const ib = ORDER.indexOf(b.path);
    if (ia !== -1 || ib !== -1) {
      return (ia === -1 ? Infinity : ia) - (ib === -1 ? Infinity : ib);
    }
    return a.title.localeCompare(b.title);
  });
  return pages;
}

// Absolute URL honouring the configured base and site; falls back to a
// root-relative path when the site is not configured.
export function absoluteUrl(route: string, site: URL | undefined): string {
  const base = import.meta.env.BASE_URL || "/";
  const joined = (base.replace(/\/$/, "") + route).replace(/\/{2,}/g, "/");
  return site ? new URL(joined, site).href : joined;
}
