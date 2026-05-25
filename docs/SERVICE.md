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
