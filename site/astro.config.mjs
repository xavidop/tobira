import { defineConfig } from "astro/config";
import sitemap from "@astrojs/sitemap";
import { visit } from "unist-util-visit";

// GitHub Pages project site. This is the ONE place the site's domain is
// declared: everything else (llms.txt/llms-full.txt, the sitemap, canonical
// links) derives it from Astro's `site`/`base` context rather than
// hardcoding it a second time. Change it here and it changes everywhere.
//
// The Pages workflow passes BASE_PATH (usually "/tobira"); locally it's "/".
// Normalized to always end in a trailing slash: import.meta.env.BASE_URL
// mirrors this value verbatim (Astro does not add the slash back), and every
// page/component in this site builds hrefs as `${base}docs/...`. Without a
// guaranteed trailing slash here, that concatenation silently produces
// "/tobiradocs" instead of "/tobira/docs". actions/configure-pages's own
// base_path output has no trailing slash, so this was reproduced with a real
// `BASE_PATH=/tobira build`, not a hypothetical.
const rawBase = process.env.BASE_PATH || "/";
const base = rawBase.endsWith("/") ? rawBase : `${rawBase}/`;

// remarkTableCode marks short inline code inside a table so CSS can keep it on
// one line. A table cell is a narrow column, and the browser treats every
// hyphen and dot in `--filter` or `driver.ErrReadOnly` as a place it may break:
// the identifier then arrives split across two lines inside a pill that looks
// torn in half. Anything short enough to fit a column is flagged here and set
// nowrap in DocsLayout; anything longer (import paths, endpoint URLs) is left
// wrappable, because forcing those onto one line is what pushed whole tables
// wider than the content column.
// How long "short" is depends on how many columns share the row: a two-column
// table can give an identifier most of a column, a five-column comparison
// table cannot.
const TABLE_CODE_ATOM_MAX = { 2: 34, 3: 30, 4: 22 };
const TABLE_CODE_ATOM_MAX_DENSE = 16;

function remarkTableCode() {
  return (tree) => {
    visit(tree, "table", (table) => {
      const columns = Math.max(
        ...table.children.map((row) => (row.children ? row.children.length : 0)),
      );
      const max = TABLE_CODE_ATOM_MAX[columns] ?? TABLE_CODE_ATOM_MAX_DENSE;
      visit(table, "inlineCode", (node) => {
        const value = String(node.value);
        if (value.length > max || /\s/.test(value)) return;
        node.data = node.data || {};
        node.data.hProperties = {
          ...(node.data.hProperties || {}),
          className: ["code-atom"],
        };
      });
    });
  };
}

// remarkBaseLinks prefixes root-relative Markdown links and images with the configured
// base. Astro rewrites `base` into component hrefs but NOT into links written
// inside Markdown, so on a project Pages site (base "/tobira/") a page linking
// "/docs/steps/" resolves to <origin>/docs/steps/ and 404s: the docs are at
// <origin>/tobira/docs/steps/. Every cross-link in the docs tree is written
// root-relative, so without this the whole tree is broken the moment it is
// served under a base, and only there, which is why a base-less build looks
// fine.
//
// Anchors, external URLs and protocol-relative URLs are left alone, as is
// anything already carrying the base, so the transform is idempotent.
function remarkBaseLinks() {
  const prefix = base.replace(/\/$/, "");
  return (tree) => {
    if (!prefix) return;
    const fix = (node) => {
      const url = node.url;
      if (typeof url !== "string") return;
      if (!url.startsWith("/") || url.startsWith("//")) return;
      if (url === prefix || url.startsWith(prefix + "/")) return;
      node.url = prefix + url;
    };
    visit(tree, "link", fix);
    visit(tree, "image", fix);
    visit(tree, "definition", fix);
  };
}

export default defineConfig({
  // The origin the site is actually served from. Canonical links, the
  // sitemap and robots.txt's Sitemap line are all derived from it, so a
  // value that is not where the site lives points crawlers and readers at
  // a host that does not answer.
  //
  // This is the GitHub Pages project URL, which is what
  // `gh api repos/xavidop/tobira/pages` reports and where the Docs workflow
  // publishes. To move to a custom domain, do BOTH: add site/public/CNAME
  // containing the bare domain, and change this line to match. Doing only
  // one leaves the built HTML disagreeing with where it is served.
  site: "https://xavidop.github.io",
  base,
  // Internal doc links are root-relative (/docs/...), so they resolve the same
  // with or without a trailing slash; "ignore" lets the dev server serve both
  // forms rather than 404-ing bare URLs.
  trailingSlash: "ignore",
  devToolbar: { enabled: false },
  build: { format: "directory" },
  // /llms.txt (index) and /llms-full.txt (all docs as one Markdown file) are
  // native static endpoints under src/pages, so they render in dev and build
  // alike. See src/lib/docs-content.ts.
  integrations: [sitemap()],
  markdown: {
    remarkPlugins: [remarkBaseLinks, remarkTableCode],
    shikiConfig: {
      // Two themes, not one. A single light theme was pinned here while the
      // site itself has a full dark palette, so in dark mode every code block
      // kept its light-theme colours: punctuation is near-black in
      // github-light, which on a dark background is invisible. `type Sink
      // interface {` read as `type Sink interface`, and `Emit(api.Event)` lost
      // both parens and the dot.
      //
      // Shiki emits the second theme as a --shiki-dark custom property per
      // token; DocsLayout switches to it under the same three conditions the
      // rest of the palette uses.
      themes: {
        light: "github-light",
        dark: "github-dark",
      },
      wrap: false,
    },
  },
});
