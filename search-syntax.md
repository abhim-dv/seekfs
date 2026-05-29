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
- `dir:` is a path substring filter.
- `regex:` evaluates against the normalized full path.
- `--exists` calls `os.Stat` and is slower, but filters stale index entries.
- `size:` units are 1024-based (`kb`, `mb`, `gb`, `tb`; the trailing `b` is
  optional). `size:` and `dm:` require an index built with file metadata; NTFS
  service indexes capture this from the MFT. Querying them against an index that
  lacks size or modification times returns a clear error rather than no results.
- Unsupported `name:` style filters (for example `attrib:`, `parent:`) are
  rejected with an error instead of being treated as literal text.

## Not Implemented Yet

- Everything filters such as `attrib:` and `parent:`.
- Directory sizes (Everything reports folders at the recursive size of their
  contents; seekfs reports directory size as 0).
- Everything-compatible ranking.
