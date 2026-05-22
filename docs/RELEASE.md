# Release Plan

## Artifact

Initial GitHub release artifact:

```text
seekfs-windows-amd64.zip
```

Contents:

- `seekfs.exe`
- `README.md`
- `LICENSE`
- `NOTICE.md`
- `install_seekfs_service.ps1`
- `restart_seekfs_service.ps1`
- `docs/`

Indexes and benchmark output are not included.

## Signing

Code signing is deferred unless the first release is distributed beyond local
trusted users. Unsigned binaries should be documented as such in release notes.
