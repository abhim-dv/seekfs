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
.\seekfs.exe install-service
Start-Service seekfs
.\seekfs.exe service-index-usn -volume C: -db F:\seekfs_c.gsi
.\seekfs.exe service-index-usn -volume F: -db F:\seekfs_f.gsi
```

Reinstall the service with the indexes it should keep resident:

```powershell
.\install_seekfs_service.ps1 -Db F:\seekfs_c.gsi,F:\seekfs_f.gsi
```

Query through the service:

```powershell
.\seekfs.exe search -service -path -n 20 "linkmerge w6 full e10"
.\seekfs.exe count  -service -path "linkmerge w6 full e10"
```

## Examples

Search file names:

```powershell
.\seekfs.exe search -service -n 50 main
```

Search full paths:

```powershell
.\seekfs.exe search -service -path -n 50 "src cmd"
```

Inspect an index:

```powershell
.\seekfs.exe info -db F:\seekfs_c.gsi
```

Compare with Everything through `es.exe`:

```powershell
.\seekfs.exe compare-es -db F:\seekfs_c.gsi -es .\extracted\es.exe -instance 1.5a -path -n 20 "src cmd"
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
.\seekfs.exe search -service -path -n 20 "linkmerge w6 full e10"
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
- Live USN monitor events are not yet applied to the resident index.
- Result ranking is simple and not Everything-compatible.
- Advanced query syntax such as `ext:`, `glob:`, and `regex:` is planned but not
  complete.
- Index files contain local path names and should be treated as sensitive local
  metadata.

## Documentation

- [Service setup](docs/SERVICE.md)
- [Service pipe protocol](docs/OPEN_PROTOCOL.md)
- [Benchmarks](docs/BENCHMARKS.md)
- [Security notes](SECURITY.md)
- [Production readiness](production-readiness.md)
- [Release TODO](RELEASE_TODO.md)

## Development

```powershell
go test ./...
go vet ./...
go build -o seekfs.exe ./cmd/seekfs
powershell -ExecutionPolicy Bypass -File .\test_seekfs_cli.ps1
```
