# seekfs

`seekfs` is a Windows-first CLI and service for fast local file-name and
full-path search. It builds compact local indexes and can keep them resident in
a Windows service so command-line searches avoid loading large databases on
every invocation.

`seekfs` is independent software. It is not affiliated with, endorsed by, or
sponsored by voidtools or Everything.

## Who It Is For

`seekfs` is for local developer and agent workflows that need fast,
machine-readable file discovery from a CLI. It is currently focused on Windows
and NTFS/USN indexing.

## Quickstart

Build:

```powershell
go build -o seekfs.exe ./cmd/seekfs
```

Create a small fallback index:

```powershell
.\seekfs.exe index -root $env:USERPROFILE\Documents -db .\docs.gsi
.\seekfs.exe search -db .\docs.gsi -n 20 report
.\seekfs.exe search -db .\docs.gsi -path -n 20 "src main"
.\seekfs.exe count  -db .\docs.gsi report
```

## Service Setup

Use the service for full-volume NTFS/USN indexing and resident search.

```powershell
.\seekfs.exe index-volumes -volume C: -volume F: -launch
```

Preview defaults before indexing:

```powershell
.\seekfs.exe defaults --json
.\seekfs.exe index-volumes --dry-run --json
```

Reinstall the service with the indexes it should keep resident:

```powershell
.\seekfs.exe launch -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi
```

Query through the service:

```powershell
.\seekfs.exe config set output_format json
.\seekfs.exe config set default_limit 20
.\seekfs.exe search "gh.exe"
.\seekfs.exe search --under F:\git\seekfs "main.go"
.\seekfs.exe search -path "ext:go dir:cmd main"
.\seekfs.exe count  -path "ext:go dir:cmd main"
```

When no `-db` is supplied, `search` and `count` use the resident service by
default. Use `-local` to skip the service and read a local DB file instead.

## Examples

Search file names:

```powershell
.\seekfs.exe search main
.\seekfs.exe search "gh.exe"
```

Search full paths:

```powershell
.\seekfs.exe search -path "src cmd"
```

Agent-friendly filters:

```powershell
.\seekfs.exe search -path "ext:go dir:cmd main"
.\seekfs.exe search -path --under F:\git\seekfs "type:file glob:*.md"
.\seekfs.exe search -path --exists --recent 24h "ext:go"
.\seekfs.exe bench -service --json -iterations 100
```

Performance note for agents: prefer filename-only search when looking for a
known file or executable name. Use `-path` only when the query needs directory
terms, `dir:`, `--under`, regex over full paths, or path context. Broad
full-path searches can be much slower on very large indexes.

Agent usage note: `seekfs` searches indexed file names and paths, not file
contents or symbols. Use `rg` for text-content search, definitions, import
references, and line matches. For repo-local file discovery, use `--under` to
avoid unrelated machine-wide results. If `seekfs` is not on PATH in a fresh
agent shell, call the binary directly, for example `F:\git\seekfs\seekfs.exe`.

Inspect an index:

```powershell
.\seekfs.exe info -db F:\seekfs_c.gsi
```

## Current Benchmark Snapshot

On the development machine, packed v7 C: + F: indexes measured:

```text
C: index: 291,622,783 bytes
F: index: 460,212,392 bytes
combined: 751,835,175 bytes
entries: 22,479,440
```

Service startup loaded both DBs in `4.633s`.

For:

```powershell
.\seekfs.exe search -service -path -n 20 "ext:go dir:cmd main"
```

Measured over 30 runs:

```text
median: 100.498 ms
p90: 120.833 ms
max: 1,207.344 ms
failures: 0
```

## Limitations

- Windows and NTFS are the primary target.
- Result ranking is simple and not Everything-compatible.
- Some Everything-style filters are not implemented, including `dm:`, `size:`,
  `attrib:`, `parent:`, OR, and NOT.
- Index files contain local path names and should be treated as sensitive local
  metadata.

## Documentation

- [Service setup](docs/SERVICE.md)
- [Configuration](docs/CONFIG.md)
- [Service pipe protocol](docs/OPEN_PROTOCOL.md)
- [Benchmarks](docs/BENCHMARKS.md)
- [Incremental updates plan](docs/INCREMENTAL_UPDATES.md)
- [Security notes](SECURITY.md)

## Config Shortcuts

```powershell
.\seekfs.exe config path
.\seekfs.exe config set output_format json
.\seekfs.exe config set dbs = '["F:\\seekfs_c.gsi", "F:\\seekfs_f.gsi"]'
.\seekfs.exe defaults --json
```

## Development

```powershell
go test ./...
go vet ./...
go build -o seekfs.exe ./cmd/seekfs
powershell -ExecutionPolicy Bypass -File .\test_seekfs_cli.ps1
```

## Release Artifacts

The initial release artifact is an unsigned zip:

```text
seekfs-windows-amd64.zip
```

It contains `seekfs.exe`, README, license, notice, and docs. Windows may warn
about unsigned executables until code signing is added.
