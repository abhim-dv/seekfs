# seekfs v0.10.0

This release ships the v9 mapped query index work, lazy planner posting sources,
and expanded service/direct validation for persisted real-index behavior.

## Highlights

- v9 indexes can persist derived query sections for name ranks, child ranges,
  subtree intervals, FRN lookup, lowercase names, and extension/component/name
  postings.
- Low-memory service startup can map those derived sections instead of rebuilding
  resident query structures after load or persist.
- Query planning can intersect lazy mapped posting sources for extension,
  component, bounded-root, and name-gram filters before hydrating rows or
  reconstructing paths.
- Broad path and extension-family queries avoid more eager candidate
  materialization while preserving the existing v8 fallback behavior.
- Active v9 overlay search and count paths account for tombstoned and shadowed
  base records while still returning live overlay records.
- The desktop UI verifies backend binary identity and handles stale standalone
  service processes more defensively.

## Query Coverage

- Added service/direct acceptance coverage for persisted real indexes, mapped
  low-memory startup, overlay state, and service identity.
- Added deterministic benchmark harnesses for warm planner, mapped planner, and
  fallback scan behavior across broad and selective query families.
- Added public fixture coverage for extension, component, bounded directory,
  overlay delete/rename, and mapped count/search parity.

## Validation

```text
go test ./cmd/seekfs -count=1
go build -trimpath -o seekfs-service.exe ./cmd/seekfs
go build -trimpath -tags "seekfs_ui production" -o seekfs.exe ./cmd/seekfs
```

The release was also checked against large persisted local indexes with
service/direct parity acceptance tests. High-churn service soak testing remains
separate from this release gate.

## Compatibility

- Existing v8 `.gsi` indexes remain readable.
- v9 derived query sections are gated by v9 index creation/upgrade paths; older
  indexes continue to use the existing resident fallback paths.
- The release artifact is unsigned.

## Upgrade

1. Stop any running seekfs processes.
2. Replace both `seekfs.exe` and `seekfs-service.exe` from the release zip.
3. Start the UI with `seekfs.exe`.
4. Use `seekfs-service.exe` for backend/service CLI commands when needed.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.
