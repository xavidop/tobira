---
layout: ../../../layouts/DocsLayout.astro
title: tobira connect
---

# Open the terminal UI on one connection

`tobira connect` opens the [terminal UI](/docs/tui/) with one connection expanded. Give it a
connection name from the config file, or a DSN.

```sh
tobira connect local-pg
tobira connect sqlite:./a.db --read-only
```

## tobira connect

```text
tobira connect <connection|dsn> [--read-only]
```

Opens the terminal UI with that connection expanded. The argument is a name from the config file
or a DSN (detected as for `tobira query`). A DSN is added for this session only, shown with its
credentials redacted. An unknown name exits 1 with a "did you mean" suggestion. `--read-only` makes
every connection of the session read-only. It needs a terminal on stdin and stdout, and exits 1
without one.

A DSN does not need a config file. If the file is missing, `tobira connect <dsn>` uses an empty
config. If the file exists but cannot be read (bad YAML, a bad `tui:` section), tobira prints
`warning: ...; continuing without the config file` to stderr, shows the same text in the status bar,
and opens the DSN with no configured connections and connection management off. A connection
*name* still needs a readable config, and exits 1 without one.

