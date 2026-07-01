# seekfs v0.9.0

This release ships the low-memory mmap-backed resident engine, the desktop UI
binary split, and the NTFS/component-aware query planner work from the
compressed trigram branch.

## Highlights

- `seekfs.exe` is now the desktop UI binary.
- `seekfs-service.exe` is the backend/CLI/service binary used for indexing,
  service launch, search, benchmarks, and diagnostics.
- Resident indexes can load compact records through mmap-backed storage,
  reducing Go heap pressure on very large C: and F: indexes.
- Name trigram postings are compressed and built with bounded segment workers.
- Extension-bounded path searches verify parent-chain path terms before
  reconstructing full paths, avoiding the slow path behind queries such as
  `path:C: pretraining DVT .nrrd`.
- Low-memory mode keeps selective trigram postings available for broad path
  query planning without returning to the earlier multi-GB heap footprint.
- The UI uses the supplied icon/logo assets for the window and taskbar icon.

## Query Coverage

- Added deterministic high-fanout fixtures with thousands of extension matches
  and only a few true multi-part path matches.
- Added generated multi-part path syntax matrices covering `path:` placement,
  drive tokens, dotted extension promotion, `ext:`, `glob:`, negatives, and
  multiple limits.
- Added service-volume parity tests for C:/F:/unscoped high-fanout path
  searches.
- Added low-memory deterministic performance coverage for broad extension and
  multi-part path searches.

## Validation

```text
go test -count=1 ./...
go build -trimpath -o seekfs-service.exe ./cmd/seekfs
go build -trimpath -tags "seekfs_ui production" -o seekfs.exe ./cmd/seekfs
```

End-to-end service checks on the development machine after resident startup:

```text
path:C: pretraining DVT .nrrd  ~45 ms
path:F: pretraining DVT .nrrd  ~43 ms
path:C: .pvsm                  ~42 ms
path:F: .nrrd                  ~42 ms
```

## Compatibility

- Existing v8 `.gsi` indexes remain compatible.
- No index rebuild is required, though rebuilding may improve stored metadata
  quality for size and modified-time display.
- The release artifact is unsigned.

## Upgrade

1. Stop any running seekfs processes.
2. Replace both `seekfs.exe` and `seekfs-service.exe` from the release zip.
3. Start the UI with `seekfs.exe`.
4. Use `seekfs-service.exe` for backend/service CLI commands when needed.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.
