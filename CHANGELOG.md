# Changelog

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
