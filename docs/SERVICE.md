# Service Setup

## Install

Build the binary first:

```powershell
go build -o seekfs.exe ./cmd/seekfs
```

Install the service:

```powershell
.\seekfs.exe install
.\seekfs.exe start
```

Service install, launch, start, stop, and restart require an elevated shell
because the service runs as LocalSystem. Search commands do not require
elevation once the service is running.

## Build Indexes

```powershell
.\seekfs.exe index-volumes -volume C: -volume F:
.\seekfs.exe index-volumes --dry-run --json
```

Without `-volume`, `index-volumes` indexes fixed local drives by default and
stores generated indexes under:

```text
%ProgramData%\seekfs\indexes
```

## Configure Resident Search

Launch the service with the DB paths it should keep loaded:

```powershell
.\seekfs.exe launch -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi
```

`launch` installs or reinstalls the service, starts it, waits for the named pipe,
and runs the same health checks as `doctor`.

Check health:

```powershell
.\seekfs.exe status --json
.\seekfs.exe loaded --json
```

`status` verifies the Windows service and the pipe. `loaded` shows the process
serving the pipe and the loaded DB state. If a DB reports `state: "stale"`, the
service is answering from the index but journal replay is not active.

## Query Semantics

In path-scoped queries, short dotted tokens such as `.pdf`, `.raw`, and `.nrrd`
are treated as Everything-compatible extension shorthands. For example,
`path:Downloads .pdf` and `dir:Reports .pdf` match files whose extension is
exactly `pdf`; they do not match `manual.pdf.bak`. Outside path mode, the same
dotted token remains a normal name substring, so `.pdf` can match
`manual.pdf.bak`. Use `ext:pdf` when exact extension matching is desired in any
mode.

When `SEEKFS_ENGINE_V9=1` is set while building or upgrading an index, seekfs
writes the gated v9 container with mapped derived sections for rank, children,
subtree intervals, FRNs, lowercase names, and posting metadata. Convert an
existing index offline with:

```powershell
$env:SEEKFS_ENGINE_V9=1; .\seekfs.exe upgrade-index -db C:\ProgramData\seekfs\indexes\seekfs_c.gsi
```

`loaded --json` reports `derived_sections` and `derived_bytes` for mapped v9
indexes. Default v8 indexes remain readable and use the existing runtime builds.
When the v9 gate is enabled, live update WAL appends use CRC-protected binary
frames; the replay path still accepts the older JSON WAL format.

## Incremental Durability

The service keeps recent USN updates in memory for low search latency and
debounces full `.gsi` rewrites. Before applying each replay batch, it appends the
batch to a sidecar file beside the index:

```text
seekfs_c.gsi.wal
```

On service startup, seekfs replays the sidecar before reading newer NTFS journal
changes. A successful full index save removes the sidecar. Keep `.gsi.wal` files
with their matching `.gsi` files when copying or backing up a live index.

The legacy `service -skip-startup-sync` flag is a deprecated no-op. Startup WAL
replay and catch-up now always run; remove the flag after one compatibility
release once older UI/service launch scripts have aged out.

Use `info --json` to inspect index layout and size contributors:

```powershell
.\seekfs.exe info -db C:\ProgramData\seekfs\indexes\seekfs_c.gsi --json
```

## Upgrade

1. Build or unpack the new `seekfs.exe`.
2. Record current DB paths.
3. Run `seekfs launch` with the same `-db` arguments.

## Uninstall

```powershell
.\seekfs.exe stop
.\seekfs.exe uninstall
```

Remove index files manually only if you no longer need them.
