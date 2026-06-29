# seekfs v0.8.4

This backend-only patch release hardens query semantics, resident service
metadata, and deterministic coverage for broad path substring searches found
during dogfood use after v0.8.3. UI packaging is deferred to a later release.

## Highlights

- Dotted substring searches such as `.opencode` remain literal substring
  searches unless the query explicitly uses `ext:`.
- Strict whitespace tokenization is preserved for path and extension-like terms,
  so `path:Downloads .nrrd` is supported without inferring fused forms.
- Path-like queries containing `\` or `/` now infer full-path matching even
  without an explicit `path:` prefix.
- Drive-scoped broad searches such as `path:F: .nrrd`, `path:F: .raw`, and
  `path:F: .pdf` route to the requested resident volume before planning.
- Resident service responses can include structured result rows with size and
  modified-time metadata when available.
- Search requests can carry deadlines and request sequence IDs so clients can
  supersede stale work without waiting behind older resident searches.
- Resident `doctor` now considers a reachable, query-capable standalone service
  pipe healthy even when it is not the installed Windows SCM service.
- Deterministic fixtures and benchmarks now cover dotted substrings, path
  syntax permutations, implicit path separators, drive-scoped broad searches,
  and planner parity against full scans.

## Validation

```text
go test -count=1 ./...
go test -tags dev -count=1 ./...
go test -tags production -count=1 ./...
go vet ./...
```

## Compatibility

- Existing v8 `.gsi` indexes remain compatible.
- No index rebuild is required for this release.
- Result metadata quality depends on the index source; live filesystem stat data
  may fill size and modified-time fields for existing paths.
- The release artifact is unsigned.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.
