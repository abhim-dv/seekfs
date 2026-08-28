# Repository Instructions

This file is public. Keep it generic: it may contain no private or
identifying terms, no local machine paths, and no project-internal
conventions. It exists only to give third-party agents guidance that is
not already in the README. If a change to this file is needed, ask the
repository owner first.

## Using seekfs in this repository

- seekfs searches indexed file names and paths, not file contents. For
  text-content search, definitions, import references, or exact line
  matches, use `rg`.
- Prefer `seekfs search`/`seekfs count` against the resident service for
  file discovery by name or path. Put flags before the query, and quote
  multi-term queries.
- Use a plain indexed filename/path term for discovery (do not add shell
  wildcards such as `*`); constrain results with `--under`, and prefer
  adding a file term/filter over a directory-only `-path` query when the
  intent is to list a tree.
- For repository-local file discovery, scope searches with `--under` to
  the repository so results stay relevant and fast.
- If `seekfs` is not on PATH in a fresh shell, call the repository binary
  directly.
