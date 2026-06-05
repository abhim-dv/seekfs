# seekfs v0.7.0

This release focuses on resident-service memory reduction and clearer guidance
for coding agents using seekfs as a file-discovery tool.

## Highlights

- Reduced large-index resident memory by skipping full sorted name-order and
  child-range views above record-count thresholds.
- Removed the resident all-files posting list; broad `type:file` queries now
  fall back unless another selective posting such as `ext:go` is present.
- Compact packed records no longer store duplicate lowercase bytes when names
  are already lowercase.
- Size and modified-time packed arrays are now allocated only when nonzero
  metadata is present.
- Parent FRNs are derived from parent record IDs where possible, with sparse
  exceptional storage for unresolved parents.
- `loaded --json` now reports resident memory estimates for record storage,
  blobs, postings, child ranges, and sorted views.
- `seekfs help` and `seekfs agent` now explain that seekfs is for indexed file
  name/path discovery, not content or symbol search.
- Added regression tests for skipped resident views, path-term scan fallback,
  packed record optional storage, and agent-facing search semantics.

## Benchmark Notes

On a large local service index, the public benchmark suite ran with zero
failures before release packaging:

```text
name/query suite: median 49.353 ms, p90 58.162 ms, p95 74.388 ms, max 147.027 ms
path/query suite: median 49.973 ms, p90 68.920 ms, p95 71.300 ms, max 77.330 ms
```

The installed v0.6.0 service produced those timings before the v0.7.0 binary was
installed. The v0.7.0 changes are primarily memory-layout and resident-view
changes; the benchmark suite is expected to remain in the same latency range
after service restart.

## Compatibility

- Existing `.gsi` indexes remain compatible; no rebuild is required.
- The service may rebuild resident views at startup or after persistence.
- The release artifact is unsigned.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.
