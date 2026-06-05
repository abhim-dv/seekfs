# Repository Instructions

- Do not push planning, scoping, or research-only commits to the public remote.
  Push only tangible code changes, feature work, fixes, tests, docs tied to
  implemented behavior, or release artifacts/metadata.
- Use seekfs for local file discovery in this repository whenever practical.
  Prefer `seekfs search`/`seekfs count` for finding files by name or path, using
  the resident service. Use `rg` as the fallback for text-content search,
  precise line matches, or when the local seekfs service/binary is unavailable.
- If `seekfs` is not on PATH in a new shell, use the repo binary directly:
  `F:\git\seekfs\seekfs.exe`.
- Seekfs searches indexed file names and paths only. It does not search file
  contents, symbols, import references, or line matches; use `rg` for those.
- For repo-local file discovery, constrain global results with `--under`:
  `seekfs search --under F:\git\seekfs "main.go"` or
  `seekfs search -path --under F:\git\seekfs "ext:go dir:cmd main"`.
- Avoid `seekfs search -path <directory-only-query>` when the intent is to list
  a tree. Add a file term/filter or use `--under <repo>` with a filename query.
- When seekfs gives a bad result, misses a file, or is noticeably slow, append a
  one-line JSON object to `.seekfs-agent-findings.jsonl` with the query, command,
  elapsed_ms if known, expected behavior, and fallback used. Do not commit this
  log file.
