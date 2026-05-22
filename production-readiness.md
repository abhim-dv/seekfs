# seekfs production readiness

## Current Status

The CLI and Windows service are ready for local production-style testing on this
machine. The current production architecture is service-backed: the Windows
service keeps one or more packed `.gsi` indexes resident, and the CLI queries the
service with `search -service` or `count -service`.

Implemented:

- Directory-walk indexer for portable fallback indexing.
- Windows NTFS/USN initial indexer through elevated CLI or service helper.
- Packed v7 compact binary index format.
- Repeated-name interning and 24-bit packed parent/name records.
- Multi-volume querying with repeated `-db` flags.
- Windows service install/uninstall commands.
- Windows service resident search mode.
- Named pipe IPC for service queries and privileged commands.
- Service-backed name/path/count search.
- Optional path-token sidecar generation for targeted full-path acceleration.
- Service status and USN monitor start/stop commands.
- Everything comparison command.
- CLI integration test and benchmark harness.

## Known Scope

This is a CLI/service package, not a UI app. It is not an exact Everything clone,
and it does not attempt to recover or reuse Everything's implementation.

Indexes are not committed to the repository. Build them locally with
`service-index-usn` or `index`.

## Production Setup

Build:

```powershell
go build -o seekfs.exe ./cmd/seekfs
```

Build indexes:

```powershell
.\seekfs.exe install-service
Start-Service seekfs
.\seekfs.exe service-index-usn -volume C: -db F:\seekfs_c.gsi
.\seekfs.exe service-index-usn -volume F: -db F:\seekfs_f.gsi
```

Install service-backed search:

```powershell
.\install_seekfs_service.ps1 -Db F:\seekfs_c.gsi,F:\seekfs_f.gsi
```

Query:

```powershell
.\seekfs.exe search -service -path -n 20 "linkmerge w6 full e10"
.\seekfs.exe count  -service -path "linkmerge w6 full e10"
```

## Benchmark Snapshot

Against packed C: + F: indexes on this machine:

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

## Remaining Before Wider Release

- Add a versioned release build script and signed binary packaging.
- Add CI on Windows for `go test`, `go vet`, and the CLI integration test.
- Add service upgrade flow that preserves configured DB paths.
- Add a command to inspect the service's loaded DB paths and entry counts.
- Apply USN monitor events to the live and persisted index.
- Persist monitor checkpoints after journal reads.
- Detect journal ID changes, journal rollover, and stale checkpoints.
- Add a single-writer lock around service index writes.
- Add result ranking rules closer to Everything.
- Add robust hard-link, reparse-point, deleted-parent, and rename-pair handling.
- Add structured service logs.
