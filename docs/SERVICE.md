# Service Setup

## Install

Build the binary first:

```powershell
go build -o seekfs.exe ./cmd/seekfs
```

Install the service:

```powershell
.\seekfs.exe install-service
.\seekfs.exe start-service
```

## Build Indexes

```powershell
.\seekfs.exe service-index-usn -volume C: -db F:\seekfs_c.gsi
.\seekfs.exe service-index-usn -volume F: -db F:\seekfs_f.gsi
```

## Configure Resident Search

Launch the service with the DB paths it should keep loaded:

```powershell
.\seekfs.exe launch -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi
```

`launch` installs or reinstalls the service, starts it, waits for the named pipe,
and runs the same health checks as `doctor`.

Or use:

```powershell
.\install_seekfs_service.ps1 -Db F:\seekfs_c.gsi,F:\seekfs_f.gsi
```

## Upgrade

1. Build or unpack the new `seekfs.exe`.
2. Record current DB paths.
3. Run `seekfs launch` with the same `-db` arguments.

## Uninstall

```powershell
.\seekfs.exe stop-service
.\seekfs.exe uninstall-service
```

Remove index files manually only if you no longer need them.
