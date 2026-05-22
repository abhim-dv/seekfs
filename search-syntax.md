# seekfs search syntax

## Supported

`seekfs` supports a small agent-friendly query language:

- Case-insensitive substring matching by default.
- Whitespace-separated terms are ANDed.
- Name search by default.
- Full-path search with `-path`.
- Result limit with `-n`.
- Count-only mode with `count`.
- Extension filters with `ext:go`.
- Directory/path segment filters with `dir:src`.
- Glob filters with `glob:*.py`.
- Regular expressions with `regex:<pattern>`.
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

## Not Implemented Yet

- Everything filters such as `dm:`, `size:`, `attrib:`, `parent:`.
- OR / NOT operators.
- Date macros such as `today` or `lastweek`.
- Everything-compatible ranking.
