# seekfs v0.11.0

This release removes the v8 engine, adds a fast multi-term name posting
intersection path, and fixes a resident-service wedge plus derived-index
reload gaps in the direct-v9 volume build flow.

## Highlights

- **Fast multi-term name queries.** `completeMultiTermNameGramCandidates`
  intersects PNGR/PNGC postings across all query terms (driver term = smallest
  complete gram), so multi-word name queries such as `aker log`, `acme log`,
  and `discovery log` answer in ~10-30 ms instead of falling to a 2s+ bounded
  scan. This matches the Everything posting-intersection model.
- **Resident-service wedge fixed.** Background persist and index-usn no longer
  hold the global index lock for the entire multi-GB v9 write + reload (which
  blocked every `info`/`search` pipe request for minutes while still answering
  `status`). The disk write is now staged outside the global lock (per-volume
  `vol.mu` only), and the global lock is taken only for the fast swap-in;
  searches hold an RLock through the query so the mmap swap cannot race them.
  Measured after the fix: startup load ~431 ms (was ~30s), `service-info`
  0.06 s (was 30 s timeout), and no wedge during an active background persist.
- **Derived index reloads.** `index-usn`, `rebuildVolumeInPlace`, and
  `rebuildServiceVolumeIndex` reload from disk after commit instead of swapping
  in an in-memory index that never carries `Index.Derived`, so the live volume
  keeps RANK/PNGR/PNGC and multi-term queries no longer decline to the bounded
  scan.
- **v8 engine removed.** Readers and savers are v9-only; non-compact walk
  indexes auto-compact on save.
- **Direct-v9 volume builds.** Fast MFT/USN volume builds with phase timing and
  I/O fixes, plus seekfs-dir consolidation.
- **Startup tmp sweep.** Stale `*.gsi.*.tmp` files (7.8 GB had accumulated) are
  swept at startup.

## Query Coverage

- Multi-word name queries (e.g. `aker log`, `acme log`, `discovery log`,
  `exxon log`) answer via posting-intersection-pngc/pngr-multi in ~10-30 ms
  instead of the slow bounded-scan fallback.

## Validation

```text
go test ./cmd/seekfs -count=1
go build -trimpath -ldflags "-s -w -X main.version=0.11.0 ..." -o seekfs.exe ./cmd/seekfs
go build -trimpath -tags "seekfs_ui production" -ldflags "-s -w ... -H windowsgui" -o seekfs-ui.exe ./cmd/seekfs
```

The release was validated against production v9 indexes: startup load ~431 ms
(was ~30 s), multi-term name queries answer in ~10-30 ms, `service-info`
returns in 0.06 s (was 30 s timeout), and the service stays responsive during an
active background persist. High-churn service soak testing remains separate from
this release gate.

## Compatibility

- The v8 engine is removed; indexes are v9-only and non-compact walk indexes
  auto-compact on save.
- The release artifact is unsigned.

## Upgrade

1. Stop any running seekfs processes.
2. Replace `seekfs.exe` and `seekfs-ui.exe` from the release zip.
3. Launch the UI by double-clicking `seekfs-ui.exe` (no terminal window).
4. Use `seekfs.exe` for CLI/index/search commands.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.
