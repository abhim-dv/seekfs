# Changelog

## 0.11.0 - Multi-Term Posting Intersection and Service Wedge Fix

### Added

- Added `completeMultiTermNameGramCandidates` to intersect PNGR/PNGC postings
  across all query terms (driver term = smallest complete gram), so multi-word
  name queries such as `aker log`, `coterra log`, and `discovery log` answer in
  ~10-30 ms instead of falling to a 2s+ bounded scan. This matches the
  Everything posting-intersection model.
- Direct-v9 MFT/USN volume builds with phase timing and I/O fixes, and seekfs-dir
  consolidation.
- Startup sweep of stale `*.gsi.*.tmp` files (7.8 GB had accumulated).

### Changed

- Removed the v8 engine entirely; readers and savers are v9-only, and
  non-compact walk indexes auto-compact on save.
- Background persist and index-usn disk writes are staged outside the global
  index lock (per-volume `vol.mu` only); the global lock is held only for the
  fast swap-in, and searches hold an RLock through the query so the mmap swap
  cannot race them.

### Fixed

- Fixed a resident-service wedge where background persist and index-usn held the
  global index lock for the entire multi-GB v9 write + reload, blocking every
  `info`/`search` pipe request for minutes. Measured after the fix: startup load
  ~431 ms (was ~30s), `service-info` 0.06 s (was 30 s timeout), no wedge during
  an active background persist.
- Fixed `index-usn` swapping in an in-memory index that never carried
  `Index.Derived`, leaving the live volume without RANK/PNGR/PNGC so multi-term
  queries declined to the bounded scan. Indexes now reload from disk after
  commit in `index-usn`, `rebuildVolumeInPlace`, and `rebuildServiceVolumeIndex`.

### Validation

- `go test ./cmd/seekfs -count=1`
- `go build -trimpath -ldflags "-s -w -X main.version=0.11.0 ..." -o seekfs.exe ./cmd/seekfs`
- `go build -trimpath -tags "seekfs_ui production" -ldflags "-s -w ... -H windowsgui" -o seekfs-ui.exe ./cmd/seekfs`

## 0.10.0 - Mapped Query Index and Lazy Planner Sources

### Added

- Added v9 mapped query index sections for name ranks, child ranges, subtree
  intervals, FRN lookup, lowercase names, and extension/component/name-gram
  postings.
- Added lazy planner posting sources so extension, component, bounded-root, and
  mapped name-gram filters can be intersected without eagerly materializing
  broad candidate sets.
- Added service/direct acceptance coverage for real persisted indexes, active
  overlay state, service identity, and mapped low-memory query parity.
- Added deterministic benchmark harnesses for broad and selective query families
  across warm planner, mapped planner, and fallback scan paths.

### Changed

- Low-memory service startup can map persisted derived query sections instead of
  rebuilding the resident query index when v9 data is available.
- Broad path and extension-family planning now keeps more work in sorted posting
  iterators before row hydration or path reconstruction.
- Active v9 overlay search/count handling accounts for tombstoned and shadowed
  base rows while preserving live overlay records.
- The desktop UI verifies backend binary identity and handles stale standalone
  services more defensively when launching or reconnecting.

### Fixed

- Reduced cold-start and post-persist query latency cliffs caused by silently
  rebuilding derived planner structures in the background.
- Fixed mapped count/search parity gaps around overlay deletes, renames, and
  bounded directory scopes.
- Hardened test fixtures so public coverage avoids machine-specific paths and
  private search terms.

### Validation

- `go test ./cmd/seekfs -count=1`
- `go build -trimpath -o seekfs-service.exe ./cmd/seekfs`
- `go build -trimpath -tags "seekfs_ui production" -o seekfs.exe ./cmd/seekfs`

## 0.9.0 - Low-Memory mmap Engine and UI Binary Split

### Added

- Added a mmap-backed compact record load path for low-memory resident service
  mode.
- Added compressed, segmented name trigram indexes with bounded build and
  query-time verification.
- Added deterministic high-fanout path fixtures and generated multi-part path
  syntax matrices covering volume tokens, `path:` token placement, dotted
  extension promotion, `ext:`, `glob:`, negative terms, and service-volume
  routing.
- Added release packaging for both `seekfs.exe` and `seekfs-service.exe`.

### Changed

- `seekfs.exe` is now the desktop UI binary; `seekfs-service.exe` is the
  backend/CLI/service binary.
- Extension-bounded path searches verify path terms against compact parent
  chains before reconstructing full path strings.
- Low-memory trigram posting retention keeps selective broad path terms usable
  without returning to the prior large in-heap representation.
- Updated the UI window/taskbar icon and in-app logo assets.

### Fixed

- Fixed service timeouts for multi-part extension-bounded path searches such as
  `path:C: pretraining DVT .nrrd`.
- Prevented broad extension candidate sets from forcing thousands of full path
  reconstructions before path-term rejection.
- Hardened no-hit and volume-scoped multi-part path queries in low-memory mode.

### Validation

- `go test -count=1 ./...`
- `go build -trimpath -o seekfs-service.exe ./cmd/seekfs`
- `go build -trimpath -tags "seekfs_ui production" -o seekfs.exe ./cmd/seekfs`

## 0.8.4 - Backend Query Semantics and Service Parity

### Added

- Added deterministic query matrices for strict whitespace tokenization,
  path-filter permutations, implicit path separators, dotted substring terms,
  and drive-scoped broad extension-like searches.
- Added synthetic benchmarks covering broad path substring searches such as
  `path:F: .nrrd`, `path:F: .raw`, `path:F: .pdf`, and dotted non-extension
  substrings such as `.opencode`.
- Added service response rows that preserve indexed result metadata, including
  size and modified time when available.
- Added service request deadline and request sequence fields so clients can
  supersede stale searches without blocking behind older work.

### Changed

- Bare queries containing path separators now infer path matching, bringing
  path-like search strings closer to explicit `path:` behavior.
- Drive-scoped path queries route to the constrained resident volume before
  broad search planning, reducing multi-volume work for searches such as
  `path:F: .pdf`.
- Resident `doctor` now treats a reachable, query-capable standalone service
  pipe as healthy even when it is not the installed Windows SCM service.

### Fixed

- Preserved literal dotted substring semantics for terms such as `.opencode`
  instead of converting every dotted token into an extension filter.
- Kept strict space-split parsing for path and extension-like terms, so
  `path:Downloads .nrrd` is parsed differently from fused forms such as
  `path:Downloads.nrrd`.
- Hydrated live result metadata from filesystem stat data when indexed rows are
  missing size or modification time, while preserving real zero-byte files.
- Made resident search lock acquisition and scans honor cancellation/deadline
  checks more promptly.

### Validation

- `go test -count=1 ./...`
- `go test -tags dev -count=1 ./...`
- `go test -tags production -count=1 ./...`
- `go vet ./...`

## 0.8.3 - Large Index and Scoped Search Hardening

### Fixed

- Extended compact on-disk record references past the 24-bit packed limit so
  large resident indexes no longer persist stale `compact index too large for
  packed record format` failures.
- Added startup rebuild fallback for oversized WAL replay so stale resident
  indexes recover by rebuilding instead of hanging on large incremental logs.
- Added client-side timeouts for hung resident `search`/`status`/`info` calls so
  blocked pipe requests fail fast instead of hanging indefinitely.
- Released resident heap pages after large saves and rebuilds so service memory
  returns closer to steady-state after persisting wide indexes.
- Accepted search flags before or after the query, so
  `seekfs search main.go --under F:\workspace` scopes the query as expected.
- Bounded scoped filesystem fallback walks so no-hit `--under` searches on large
  roots cannot block the resident service behind a long recursive scan.

### Validation

- `go test .\...`
- `go vet .\...`

## 0.8.2 - Service Reliability and Path Query Recovery

### Added

- Rolled in release-candidate CLI compatibility support for commandless search
  invocations, including `seekfs --under <workspace> "main.go"`.
- Treated bare wildcard filename tokens such as `*_test.go` as filename globs
  without requiring an explicit `glob:` prefix.
- Added CLI compatibility and PowerShell integration coverage for commandless
  scoped search and implicit wildcard queries.

### Fixed

- Tightened resident planning for repo-scoped known-file searches so exact
  dotted filenames and extension postings drive `--under` queries before broad
  path scans.
- Treated dotted extension terms in path queries, for example `Downloads .docx`,
  as extension filters while preserving the remaining path terms.
- Added automatic service-side rebuild for unrecoverable USN checkpoints, such
  as checkpoints before the first valid USN or after the journal's next USN.
- Added pipe-call retries for transient named-pipe failures and clearer guidance
  when the service pipe denies access.
- Refreshed a loaded resident index after `service-index-usn`/`index-usn`
  rebuilds so users do not need to restart the service to see the fresh index.
- Updated README, help, and search syntax docs for the rolled-up CLI
  compatibility behavior.

### Validation

- `go test ./...`
- `go vet ./...`
- `.\test_seekfs_cli.ps1`

## 0.8.1 - Resident Memory and Repo-Scoped Search Fixes

### Fixed

- Stopped resident `NameBlob` and lowercase-name blob growth during live USN
  updates when a record's name has not changed.
- Added resident repacking after catch-up and background persistence when packed
  name blobs have grown beyond expected size.
- Reduced default resident memory by making subtree interval arrays and path
  component 3-gram postings opt-in (`SEEKFS_SUBTREE_INTERVALS=1` and
  `SEEKFS_PATH_GRAMS=1`).
- Reordered repo-scoped candidate planning so selective filename, extension,
  and glob postings can drive `--under` queries before materializing a subtree.
- Stale volumes that cannot match a query's `--under` root are skipped; stale
  matching volumes now return a clear stale-index error.
- Improved the error for omitted `search` subcommands, including flag ordering
  in the suggested replacement command.

### Validation

- Reproduced and fixed repo-scoped timeout cases where broad `--under` searches
  could take tens of seconds before selective candidate planning was applied.
- `go test ./...`, `go vet ./...`, and the CLI integration test passed before
  release packaging.

## 0.8.0 - Query Planning and Metadata Filters

### Added

- Always-on compact resident views for large service indexes, including sorted
  name order, child ranges, subtree intervals, extension postings, and path-term
  grams.
- Broad full-path scan planning for queries such as `-path "src"` and
  `-path "src main"` without rebuilding uncacheable multi-million-id postings.
- OR and NOT query operators, for example `ext:png|jpg` and `report !draft`.
- `size:` and `dm:` filters with comparisons, byte units, date macros,
  durations, and absolute dates.
- MFT-based NTFS initial indexing with file size and modification-time capture,
  with USN enumeration retained as fallback.
- Public Everything comparison helper for release validation.
- Regression coverage for query planning, OR/NOT parsing, size/date filters,
  MFT parsing, broad path scans, and service candidate parity.

### Changed

- `--under`, glob, extension, exact-name, and mixed path-term queries now use
  more selective resident planning paths before falling back to scans.
- Unsupported `name:`-style filters such as `attrib:` and `parent:` now return
  clear errors instead of silently producing empty literal searches.
- Release packaging now copies tracked docs only so local-only notes are not
  included in release zips.
- The on-disk index format is unchanged (v8); existing indexes load without a
  rebuild. Rebuild an NTFS service index only to add MFT size/date metadata.

### Known Limitations

- Directory sizes are reported as 0; Everything reports folders at recursive
  size.
- `size:` and `dm:` require indexes with metadata. Older indexes return a clear
  capability error.
- Windows and NTFS remain the primary target.
- Release artifacts are unsigned.

## 0.7.0 - Resident Memory and Agent Guidance

### Added

- Resident memory accounting in `loaded --json` for record blobs, postings,
  child ranges, and sorted resident views.
- Regression coverage for large-index fallback searches when sorted name views
  or child-range views are intentionally skipped.
- Agent-facing help text clarifying that seekfs searches indexed file names and
  paths, not file contents or symbols.
- Repo-scoped agent guidance for `--under <repo>` and PATH fallback guidance for
  shells that cannot resolve `seekfs`.

### Changed

- Reduced resident memory for large indexes by skipping full sorted name-order
  and child-range views above configured record-count thresholds.
- Removed the resident all-files posting list; `type:file` queries now need an
  additional narrowing posting such as an extension.
- Compact packed records now avoid redundant lowercase-name bytes for names that
  are already lowercase.
- Packed records now allocate size and modified-time arrays only when nonzero
  metadata is present.
- Parent FRNs are derived from parent record IDs where possible, with sparse
  storage only for exceptional parent values.

### Known Limitations

- Large indexes may use scan fallback for some broad path-term queries when
  resident child ranges are skipped.
- Windows and NTFS remain the primary target.
- Release artifacts are unsigned.

## 0.1.0 - Initial Release

### Added

- Windows-first CLI for indexed local file search.
- Directory-walk indexer.
- NTFS/USN initial indexing through elevated CLI or Windows service.
- Packed v7 index format with repeated-name interning.
- Resident Windows service search over named pipe.
- Multi-index C: + F: querying.
- Agent-oriented JSON output for search, count, info, service status, and
  service info.
- Agent query filters: `ext:`, `dir:`, `glob:`, `regex:`, `case:`,
  `type:file`, and `type:dir`.
- Agent search flags: `--under`, `--exists`, `--cwd-bias`, `--root-bias`,
  `--recent`, and `--modified-after`.
- `bench` JSON benchmark mode.
- Release build script for `seekfs-windows-amd64.zip`.

### Known Limitations

- Windows and NTFS are the primary target.
- Result ranking is simple and not Everything-compatible.
- Some Everything-style filters are not implemented, including `dm:`, `size:`,
  `attrib:`, `parent:`, OR, and NOT.
- Release artifacts are unsigned for now.
