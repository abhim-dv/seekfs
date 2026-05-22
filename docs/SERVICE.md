# Service Setup

## Install

Build the binary first:

```powershell
go build -o seekfs.exe ./cmd/seekfs
```

Install the service:

```powershell
.\seekfs.exe install-service
Start-Service seekfs
```

## Build Indexes

```powershell
.\seekfs.exe service-index-usn -volume C: -db F:\seekfs_c.gsi
.\seekfs.exe service-index-usn -volume F: -db F:\seekfs_f.gsi
```

## Configure Resident Search

Reinstall the service with the DB paths it should keep loaded:

```powershell
Stop-Service seekfs -ErrorAction SilentlyContinue
.\seekfs.exe uninstall-service
.\seekfs.exe install-service -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi
Start-Service seekfs
```

Or use:

```powershell
.\install_seekfs_service.ps1 -Db F:\seekfs_c.gsi,F:\seekfs_f.gsi
```

## Upgrade

1. Build or unpack the new `seekfs.exe`.
2. Record current DB paths.
3. Stop the service.
4. Reinstall with the same `-db` arguments.
5. Start the service.
6. Run `seekfs service-status` and a smoke search.

## Uninstall

```powershell
Stop-Service seekfs -ErrorAction SilentlyContinue
.\seekfs.exe uninstall-service
```

Remove index files manually only if you no longer need them.
