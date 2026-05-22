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

The 0.1.0 release is unsigned. Document this in release notes and expect normal
Windows warnings for unsigned binaries. Add Authenticode signing before wider
distribution beyond trusted local users.
