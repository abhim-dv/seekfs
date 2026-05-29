# seekfs v0.8.0

This release focuses on resident-service query planning, Everything-style
filters, and metadata-aware NTFS indexes.

## Highlights

- Added always-on compact resident views for large service indexes, including
  sorted name order, child ranges, subtree intervals, extension postings, and
  path-term grams.
- Improved `--under` and path-aware planning so repo-scoped glob, extension,
  exact-name, and mixed path-term queries avoid broad volume scans where a
  selective resident source is available.
- Added a broad full-path scan path for queries such as `-path "src"` and
  `-path "src main"` so large, uncacheable postings are no longer rebuilt on
  every request.
- Added OR and NOT query operators, for example `ext:png|jpg` and
  `report !draft`.
- Added `size:` and `dm:` filters with comparison operators, byte units, date
  macros, durations, and absolute dates.
- NTFS `index-usn` now prefers an MFT-based initial build that captures file
  size and modification time, falling back to the previous USN enumeration path
  when raw MFT reading is unavailable.
- Unsupported `name:`-style filters such as `attrib:` and `parent:` now return
  clear errors instead of being treated as literal text.
- Added regression coverage for query planning, OR/NOT parsing, size/date
  filters, MFT record parsing, broad path scans, and service candidate parity.
- Release packaging now copies tracked documentation only, preventing local-only
  research notes or benchmark output from leaking into the zip.

## Benchmark Notes

On the maintainer's full local C: + F: service index, the v0.8 prototype was
validated against public benchmarks and local dogfood checks before release
packaging. Public benchmark query suites completed with zero failures.

Representative warm service CLI timings on that machine:

```text
-path "src main.go": about 50-100 ms
-path "src": about 500 ms
count ext:md: about 90-100 ms
```

Those numbers are hardware and index dependent; use `seekfs bench -service
--json` and the public query files under `bench/` for local validation.

## Compatibility

- Existing v8 `.gsi` indexes load without a rebuild; the on-disk format is
  unchanged from v0.7.0.
- `size:`, `dm:`, `--recent`, and `--modified-after` require indexes that carry
  file metadata. Rebuild an NTFS service index with this version to capture size
  and modification time from the MFT; indexes built without that metadata return
  a clear capability error for those filters rather than empty results.
- Directory sizes are reported as 0. Everything reports folders at the recursive
  size of their contents.
- The release artifact is unsigned.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.
