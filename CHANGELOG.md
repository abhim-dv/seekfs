# Changelog

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
