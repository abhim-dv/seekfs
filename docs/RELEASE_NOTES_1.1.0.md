# seekfs v1.1.0

This release retires the v8 engine entirely and finishes the Everything-style
posting-intersection fast path for multi-term name queries. It also fixes a
resident-service wedge where background persistence and index refresh held the
global index lock for the entire multi-GB write and reload, fixes index-usn
swapping in an in-memory index that never carried derived query sections, makes
CLI searches prefer the resident service, and delivers a round of Explorer-style
UI features: a native shell context menu, rubber-band marquee selection, and
per-row file-type icons.

## Highlights

- **v8 engine removed**. Readers and savers are v9-only now; non-compact walk
  indexes auto-compact on save. Existing v8-only paths are gone.
- **Multi-term posting intersection (`completeMultiTermNameGramCandidates`)**.
  Multi-word name queries such as `aker log`, `coterra log`, and `discovery log`
  now intersect PNGR/PNGC postings across all query terms (driver term = the
  smallest complete gram) instead of dropping into a bounded full-record scan.
  These queries answer in ~10-30 ms rather than 2s+. This matches the
  Everything posting-intersection model.
- **Service wedge fixed**. Background persist and `index-usn` previously held
  the global index lock for the entire multi-GB v9 write + reload, blocking every
  `info`/`search` pipe request for minutes (0 CPU, `status` responsive but
  `info`/`search` hang). The disk write is now staged outside the global lock
  (per-volume `vol.mu` only) and the global lock is held only for the fast
  swap-in; searches hold an RLock through the query so the mmap swap cannot race
  them. Measured after the fix: startup load ~431 ms (was ~30s), `service-info`
  0.06 s (was 30 s timeout), and no wedge during an active background persist.
- **Stale derived-sections bug fixed**. `index-usn` swapped in an in-memory
  index that never carried `Index.Derived`, leaving the live volume without
  RANK/PNGR/PNGC so multi-term queries declined to the bounded scan. Indexes are
  now reloaded from disk after commit in `index-usn`,
  `rebuildVolumeInPlace`, and `rebuildServiceVolumeIndex`.
- **Startup tmp sweep**. Startup sweeps stale `*.gsi.*.tmp` files (7.8 GB had
  accumulated).
- **Fast MFT/USN volume builds**. Direct-v9 volume builds use fast MFT/USN
  enumeration with phase timing and I/O fixes, plus seekfs-dir consolidation.
- **CLI searches prefer the resident service**. A configured `dbs` list in
  `seekfs.toml` used to force every CLI `search`/`count` into local mode,
  cold-loading the multi-GB index per invocation (~15 s per query). CLI queries
  now go through the resident service first (fast, warm mmap) and only fall back
  to a local load when the service is unavailable and configured DBs exist.
  `seekfs search hadespy` dropped from ~15 s to ~300 ms; `--under` scoped
  searches to ~30 ms.
- **Native shell context menu (UI)**. Right-clicking results opens the real
  Explorer context menu (Open/Open Path/Copy/Properties/Rename/Delete via
  `SHCreateDefaultContextMenu`, grouped by parent folder). Menus are built on a
  dedicated STA service thread, pre-built on row hover, and cached so the menu
  opens instantly.
- **Explorer-style selection (UI)**. Drag on empty space draws a rubber-band
  marquee box that selects every row it touches, with edge auto-scroll; dragging
  on a row extends the selection range; Ctrl/Shift multi-select and Ctrl+A work.
  Row hit-testing uses cached metrics, so marquee updates are layout-free.
- **Resizable, shrink-wrapped columns (UI)**. Columns no longer stretch to fill
  the window: widths are saved per-column, the table shrink-wraps so narrower
  columns leave empty space on the right (native behavior), and a horizontal
  scrollbar appears only when columns spill past the window.
- **File-type icons (UI)**. Each row shows the real 16x16 shell icon for its
  file type (drawn via `SHGetFileInfoW` + `DrawIconEx`, cached per extension),
  matching Everything.

## Query Coverage

- Multi-term name queries (e.g. `aker log`, `coterra log`, `discovery log`,
  `exxon log`) now answer in 20-30 ms via `posting-intersection-pngc` /
  `posting-intersection-pngr-multi` instead of falling to a bounded scan.
- Multi-term queries that cannot be satisfied by the posting-intersection fast
  path keep the budget-gated bounded-scan fallback.

## Validation

```text
go test ./cmd/seekfs -count=1
go vet ./cmd/seekfs
go build -trimpath -ldflags "-s -w -X main.version=1.1.0 ..." -o seekfs.exe ./cmd/seekfs
go build -trimpath -tags "seekfs_ui production" -ldflags "-s -w ... -H windowsgui" -o seekfs-ui.exe ./cmd/seekfs
go test -tags "seekfs_ui production" ./cmd/seekfs
```

The release was verified against the production v9 indexes: startup load ~431 ms
(was ~30 s), `aker log` / `coterra log` / `discovery log` / `exxon log` all
20-30 ms via posting intersection, `service-info` 0.06 s, and no wedge during an
active background persist. CLI queries use the resident service (`size:>100mb`
counts 8068 files in well under a second); `seekfs search hadespy` answers in
~300 ms. The UI was exercised end to end: native context menu with hover
prebuild, marquee selection with auto-scroll, per-row icons, column resizing,
and `size:` queries. The full test suite (plain and `seekfs_ui`-tagged) was
validated before this release.

## Compatibility

- v8 `.gsi` indexes are no longer readable; non-compact walk indexes
  auto-compact on save. Existing indexes should be rebuilt by the resident
  service on next startup.
- The release artifact is unsigned.

## Upgrade

1. Stop any running seekfs processes.
2. Replace `seekfs.exe` and `seekfs-ui.exe` from the release zip.
3. Launch the UI by double-clicking `seekfs-ui.exe` (no terminal window).
4. Use `seekfs.exe` for CLI/index/search commands.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.
