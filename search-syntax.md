# seekfs search syntax

## Supported

`seekfs` currently supports a deliberately small query language:

- Case-insensitive substring matching.
- Whitespace-separated terms are ANDed.
- Name search by default.
- Full-path search with `-path`.
- Result limit with `-n`.
- Count-only mode with `count`.

Examples:

```powershell
.\seekfs.exe search -db .\seekfs_docs_downloads_v3.gsi bench
.\seekfs.exe search -db .\seekfs_docs_downloads_v3.gsi "bench py"
.\seekfs.exe search -db .\seekfs_docs_downloads_v3.gsi -path "Codex 2026"
.\seekfs.exe count  -db .\seekfs_docs_downloads_v3.gsi needle
```

## Current Behavior From Manual Checks

Plain substring:

```powershell
.\seekfs.exe search -db .\seekfs_docs_downloads_v3.gsi -n 5 bench
```

Works.

Multi-term AND:

```powershell
.\seekfs.exe search -db .\seekfs_docs_downloads_v3.gsi -n 5 "bench py"
```

Works; both terms must appear in the selected search field.

Path matching:

```powershell
.\seekfs.exe search -db .\seekfs_docs_downloads_v3.gsi -path -n 5 "Codex 2026"
```

Works against the full path.

Extension approximation:

```powershell
.\seekfs.exe search -db .\seekfs_docs_downloads_v3.gsi -n 5 .py
```

Works as a substring search. This is not the same as Everything's `ext:py`.

## Not Implemented Yet

These Everything-style features are currently treated literally and generally
return no results unless the filename literally contains the text:

- `ext:py`
- `dm:today`
- `size:>1mb`
- `attrib:h`
- `parent:"C:\path"`
- wildcards such as `*.py`
- regex mode
- OR / NOT operators
- date macros
- Everything-compatible ranking

## Recommended Next Search Features

1. `ext:<list>` extension filter.
2. Wildcard expansion for `*` and `?`.
3. Negation with `!term`.
4. OR groups with `a|b`.
5. Quoted phrase parsing inside the Go CLI.
6. Size/date/attribute predicates.
7. Regex mode.

