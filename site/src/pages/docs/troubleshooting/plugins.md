---
layout: ../../../layouts/DocsLayout.astro
title: "Troubleshooting: plugins"
---

# Troubleshooting: plugins

Fixes for external driver plugins that are not found, refused or failing.

## Common problems

`tobira driver list` shows every discovered plugin and why one is not used; doctor's `plugins`
section reports the same without starting any (`--online` starts them).

- **Refused as `world-writable` or `in a world-writable directory`.** Anyone on the machine could
  replace such a binary, so tobira will not run it. `chmod 755` the binary and `chmod 700` (or
  `755`) its directory.
- **`not executable`.** `chmod +x` the binary.
- **The plugins directory is group- or world-writable.** doctor warns (group) or fails (world);
  tighten it to `0700`. `tobira driver install` creates it that way.
- **Shadowed.** A built-in driver of the same name wins, and so does a plugin found earlier
  (the plugins directory before `PATH`).
- **A plugin starts but fails.** Its stderr goes to the `--debug` log, redacted. A plugin gets only an
  environment allow-list, so a variable it needs must be named `TOBIRA_PLUGIN_*` (and not end in
  `_PASSWORD`, `_TOKEN`, `_KEY` or `_SECRET`).
- **"the binary changed since discovery; restart tobira to use the new one".** The plugin file was
  replaced while tobira ran; start tobira again.
