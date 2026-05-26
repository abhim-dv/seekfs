# seekfs v0.4.0

This release focuses on resident-service memory usage and fast path-style searches for agent workflows.

## Highlights

- Reduced resident service memory substantially on large multi-volume indexes.
- Kept extension, dotted filename, drive/path substring, and common filename queries fast through the service.
- Added a bounded resident query planner that uses compact postings for extension, volume, and file/directory type filters before verifying full query semantics.
- Reduced large service-only maps by using lazy fallback structures on large indexes.
- Loaded compact names from the shared name blob instead of allocating a separate string per record.
- Replaced the full FRN-to-record Go map with a compact sorted FRN table plus a small overlay for new journal records.
- Avoided retaining a full compact name sort order for large resident service indexes when candidate-backed search can avoid it.
- Added generic regression tests for dotted suffixes, drive tokens, full-path terms, and safe `--under` behavior.

## Measured Locally

On a large local two-volume index, the resident service working set dropped from roughly 11 GB to roughly 4 GB while preserving sub-100 ms median latency for the targeted service-backed searches.

Everything remained lower in resident memory on the same machine. Matching that footprint will require a packed in-memory record store, planned for a later release.

## Notes

- This is an independent project and is not affiliated with Everything or voidtools.
- Existing `.gsi` indexes remain compatible; no rebuild is required for this release.
- The release artifact is unsigned.
