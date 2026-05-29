# Release Plan

## Artifact

GitHub release artifact:

```text
seekfs-windows-amd64.zip
```

Contents:

- `seekfs.exe`
- `README.md`
- `LICENSE`
- `NOTICE.md`
- `docs/`

Indexes and benchmark output are not included.

## Current Release

Current release: `v0.8.0`.

Release notes:

```text
docs/RELEASE_NOTES_0.8.0.md
```

## Signing

The release artifact is unsigned. Document this in release notes and expect
normal Windows warnings for unsigned binaries. Add Authenticode signing before
wider distribution beyond trusted local users.
