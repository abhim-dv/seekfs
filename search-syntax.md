# seekfs search syntax

## Supported

`seekfs` supports a small agent-friendly query language:

- Case-insensitive substring matching by default.
- Whitespace-separated terms are ANDed.
- OR alternatives within a term with `a|b` (for example `ext:png|jpg`).
- Negation with `!term` or `-term` (for example `main !test`).
- Name search by default.
- Full-path search with `-path`.
- Result limit with `-n`.
- Count-only mode with `count`.
- Extension filters with `ext:go`.
- Directory/path segment filters with `dir:src`.
- Glob filters with `glob:*.py`.
- Regular expressions with `regex:<pattern>`.
- Size filters with `size:>100mb`, `size:>=1gb`, `size:<4k`, or `size:1024`.
- Modified-date filters with `dm:today`, `dm:yesterday`, `dm:thisweek`,
  `dm:lastweek`, a duration such as `dm:24h` / `dm:7d`, or a date `dm:2026-05-01`.
- Case-sensitive matching with `case:` or `--case`.
- Type filters with `type:file` and `type:dir`.
- Workspace scoping with `--under <path>`.
- Stale-result verification with `--exists`.
- Recency filters with `--recent 24h` or `--modified-after 2026-05-22`.
- Ranking bias with `--cwd-bias` or `--root-bias <path>`.
- Fuzzy matching: when a single-term query returns fewer than 10 results,
  close matches (Damerau-Levenshtein distance, accent/fullwidth-folded) are
  appended below the exact results and ranked by edit distance, then prefix
  alignment, then normal rank. For multi-term queries that return no results
  at all, terms that match nothing (or almost nothing) on their own are
  replaced by their best close-match variants and the query is re-run once
  (`reprot pdf` finds `report.pdf`, `discovery llog` finds `discovery log`);
  common terms stay as hard constraints. `term~` or `--fuzzy` opts in
  explicitly, regardless of how many exact results were found. Terms shorter
  than 3 characters are not fuzzed.

## Examples

```powershell
.\seekfs.exe search -service -path -n 20 "ext:go dir:cmd main"
.\seekfs.exe search -service -path -n 20 "glob:*.py"
.\seekfs.exe search -service -path -n 20 "regex:README\\.(md|txt)"
.\seekfs.exe search -service -path --under F:\git\seekfs "type:file ext:go"
.\seekfs.exe search -service -path --exists --recent 24h "ext:md"
.\seekfs.exe search -service -path --cwd-bias "main"
.\seekfs.exe count  -service -path "type:dir docs"
```

## Notes

- `ext:` matches exact file extensions without the leading dot.
- `glob:` currently matches the file name, not the full path.
- Bare wildcard filename tokens such as `*_test.go` are treated as filename
  globs too; use `glob:` when you want that behavior to be explicit.
- `dir:` is a path substring filter.
- `parent:` matches entries whose immediate parent directory name equals the
  supplied value; it accepts one directory name, not a path or glob.
- `attrib:` matches file attributes. Supported flags are `R`, `H`, `S`, `D`,
  and `A`; combined flags such as `attrib:HS` require all listed bits.
- `regex:` evaluates against the normalized full path.
- Omitting the `search` subcommand is accepted for search-like invocations, for
  example `seekfs --under F:\git\seekfs "main.go"`.
- `--exists` calls `os.Stat` and is slower, but filters stale index entries.
  Rootless service `--exists` uses a complete global verification fallback and
  is intentionally outside the bounded R5 planner performance claim.
- `size:` units are 1024-based (`kb`, `mb`, `gb`, `tb`; the trailing `b` is
  optional). `size:`, `dm:`, and `attrib:` require an index built with file
  metadata; NTFS service indexes capture this from the MFT. Querying them
  against an index that lacks required metadata returns a clear error rather
  than no results.
- Unsupported `name:` style filters are rejected with an error instead of being
  treated as literal text.

## Not Implemented Yet

- Directory sizes (Everything reports folders at the recursive size of their
  contents; seekfs reports directory size as 0).
- Everything-compatible ranking.
