# seekfs v0.5.0

This release focuses on resident-service memory use, cache hygiene, and incremental-update confidence for large NTFS indexes.

## Highlights

- Added a packed in-memory compact record store for large resident service indexes.
- Kept compact record access compatible with existing `.gsi` indexes; no index rebuild is required.
- Reduced service cache growth by bounding path, term, path-term, and extension caches.
- Rebuilt resident query postings after persisted incremental updates and after large recent-change batches.
- Added fast count handling for service-backed posting-only queries.
- Expanded `doctor` and service status JSON with loading, database, cache, dirty, recent-change, and query-index details.
- Added live service incremental test coverage for create, rename, move, and delete flows.
- Added regression tests for packed records and resident service cache behavior.

## Notes

- This is an independent project and is not affiliated with Everything or voidtools.
- Existing `.gsi` indexes remain compatible; no rebuild is required for this release.
- The release artifact is unsigned.
