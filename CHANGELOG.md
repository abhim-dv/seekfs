# Changelog

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
