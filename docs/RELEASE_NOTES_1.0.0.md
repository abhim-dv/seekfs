# seekfs v1.0.0

This is the first stable (1.0) release. It ships the v9 mapped query engine as
the default for the resident service, a native windowless desktop UI, and the
final query-planner coverage items from the Everything-parity plan (literal
prefilters and multi-volume `type:file` counts).

## Highlights

- **Native GUI exe (`seekfs-ui.exe`)**. The desktop UI is built with the Windows
  GUI subsystem, so double-clicking it opens the search window with no console
  terminal. The old `seekfs-service.exe`/launcher-script split is gone.
- **v9 mapped engine by default**. The resident service spawned by the UI now
  always loads v9 indexes as memory-mapped sections (previously opt-in via
  `SEEKFS_ENGINE_V9`), keeping service memory near the mapped working set
  (~70 MB for 27M+ records) instead of loading everything into RAM.
- **Literal prefilters for glob queries**. Plans for `glob:*` name filters now
  seed candidate postings from a literal name-substring source before expansion,
  cutting needless eager candidate materialization on broad path queries.
- **Multi-volume `type:file` counts**. `count type:file <term>` now runs across
  all loaded volumes using capped file-term postings, matching the single-volume
  behavior.
- **Cleaner artifact naming**. The release zip ships `seekfs.exe` (CLI/search
  tool) and `seekfs-ui.exe` (desktop UI). `seekfs.exe` matches the README and
  docs throughout; there is no separate `seekfs-service.exe` any more.
- **Fast multi-term name queries**. Plain multi-term queries (e.g. `aker log`)
  stay in the fast name-trigram path instead of being auto-inferred as path
  searches and dropped into a slow full-record scan; the built-in path-scan
  fallback is now budget-gated and declines early when the remaining deadline
  cannot cover the record scan. Same query went from ~20+ s to ~50 ms.

## Query Coverage

- Added acceptance tests for glob-literal prefilters (`glob_literal_test.go`)
  and multi-volume type:file counts (`type_file_test.go`).
- Expanded deterministic coverage for the mapped planner, v9 overlay state, and
  broad path/ext-family fallback behavior.

## Validation

```text
go test ./cmd/seekfs -count=1
go vet ./cmd/seekfs
go build -trimpath -ldflags "-s -w -X main.version=1.0.0 ..." -o seekfs.exe ./cmd/seekfs
go build -trimpath -tags "seekfs_ui production" -ldflags "-s -w ... -H windowsgui" -o seekfs-ui.exe ./cmd/seekfs
```

The release was verified against the production v9 indexes (C: and F: volumes,
~27.6M records): the windowless UI spawns the resident service and searches with
low memory. High-churn service soak testing remains separate from this release
gate.

## Compatibility

- Existing v8 `.gsi` indexes remain readable; v9 indexes (with derived query
  sections) get the fast mapped paths.
- The release artifact is unsigned.

## Upgrade

1. Stop any running seekfs processes.
2. Replace `seekfs.exe` and `seekfs-ui.exe` from the release zip.
3. Launch the UI by double-clicking `seekfs-ui.exe` (no terminal window).
4. Use `seekfs.exe` for CLI/index/search commands.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.