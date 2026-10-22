---
layout: ../../../layouts/DocsLayout.astro
title: Prompts
---

# Prompts

The assistant uses four built-in prompts. You can replace any of them with your own file, and you
can run and inspect the assistant's flows in the Genkit Dev UI.

## Prompts and overrides

The four prompts are `ask` (propose a query), `answer` (answer by running tools),
`explain_results` and `fix_error`. To change one, put a file of the same name in
`$XDG_CONFIG_HOME/tobira/prompts/` (`~/.config/tobira/prompts/` when `XDG_CONFIG_HOME` is unset):

```text
~/.config/tobira/prompts/ask.prompt
~/.config/tobira/prompts/answer.prompt
~/.config/tobira/prompts/explain_results.prompt
~/.config/tobira/prompts/fix_error.prompt
```

- Files use the [Dotprompt](https://github.com/google/dotprompt) format, and receive the same
  inputs as the built-in ones (`dialect`, `kind`, `schema`, `nonce`, `question` or `query`, and so
  on).
- Start from a copy of the built-in file in the
  [source](https://github.com/xavidop/tobira/tree/main/ai/prompts), and keep the data fences and
  their rules. Removing them removes the [prompt-injection protection](/docs/ai/privacy/#prompt-injection).
- An override that cannot be read, is not a regular file or does not render stops the assistant
  from starting, with an error naming the file.
- A file whose name is not one of the four is ignored. Without the directory, the built-in prompts
  are used.

## Genkit Dev UI

The assistant is built on [Genkit](https://genkit.dev), and you can run and inspect its flows in
the Genkit Dev UI. This is a developer tool, so the command is hidden from `--help`.

```sh
genkit start -- tobira ai dev
```

While it runs, any process on your machine can call the flows through Genkit's local server on
127.0.0.1. The flows run read queries on any configured connection (the caller picks it) and, as
`send_rows` allows, send rows to the provider. Genkit may also store traces of each run, prompts and
rows included, in a `.genkit/` directory under the working directory.

- Use test connections, or `send_rows: never`.
- The Dev UI cannot answer the row prompt, so under `ask` rows are withheld there.
- `execute` is never offered.
