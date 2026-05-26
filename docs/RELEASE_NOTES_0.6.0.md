# seekfs v0.6.0

This release focuses on service search latency, memory stability under repeated
agent queries, and more robust path-aware candidate narrowing.

## Highlights

- Added compact sorted resident name-order views for large USN-backed service indexes.
- Added compact parent-child range arrays for subtree and path-term narrowing.
- Added exact-name lookup over sorted resident views so large indexes avoid large exact-name maps.
- Improved simple extension glob queries such as `glob:*.md` by planning them through extension postings.
- Reduced resident cache growth with bounded posting caches, a smaller path cache, and periodic memory release.
- Kept broad substring search on the scan-and-cache path instead of shipping the experimental global n-gram accelerator.
- Added public benchmark query files and per-query benchmark reporting.
- Added regression coverage for packed records, service cache trimming, extension-glob count planning, and live service incremental testing.

## Benchmark Notes

On the maintainer's full local C: + F: service index, the public benchmark suite
ran with zero failures and service-process memory stabilized during repeated
query loops after initial cache warmup.

Everything still uses less resident memory on the same machine. Matching that
footprint remains future work and will require deeper compact record storage
changes, especially around duplicate name/lowercase-name storage.

## Compatibility

- Existing `.gsi` indexes remain compatible; no rebuild is required for this release.
- The service may rebuild resident views at startup or after persistence, but the on-disk index format is unchanged.
- The release artifact is unsigned.

## Notes

- This is an independent project and is not affiliated with Everything or voidtools.
