# seekfs v1.10.1

The service no longer indexes its own files, so it can no longer drive its own
persist loop, and the persist trigger scales with index size instead of firing on
a fixed change count.

## Fixed

- **Self-write churn fed the persist loop.** Every USN change seekfs made to its
  own staged index, WAL, log, and builder spill files was replayed back into its
  own overlay. Measured on a live index: 91.7% of the 274,558 changes pending in
  the WAL came from seekfs's own files (251,870 of them), dominated by the `.wal`
  write events themselves. The service now resolves the directories that hold its
  own artifacts to NTFS file references and drops changes whose parent directory
  is one of them, before the WAL append and before the overlay apply, while still
  advancing the checkpoint past them. After the fix the same WALs carried **0**
  self-write changes.

- **The persist watermark was a fixed change count.** It had to reach 64K pending
  overlay entries, which on a large index is a small fraction of the records, so a
  single busy folder (a build tree, `node_modules`, a spool) could make a fold due
  the moment the previous one finished. The watermark now scales with the record
  count (about 1/32 of the records, never below 64K), keeping folds proportionate;
  the WAL size, tombstone ratio, and dirty-age triggers still bound how long
  changes stay unfolded.

- **A full disk became a permanent persist-failure loop.** An interrupted persist
  leaves a multi-GB staged temp file next to the index, and the sweep that reaps
  them only ran at service startup. A service that stayed up for days accumulated
  them until every later persist failed for lack of space (4.6 GB observed on a
  drive with 2.6 GB free). Running services now sweep abandoned temps at most once
  an hour, and only files older than an hour, so a live persist is never touched.

## Notes

- The name-gram spill directory now defaults to `%ProgramData%\seekfs\gram-spool`
  (inside the never-indexed seekfs dir) instead of the system temp directory, so
  the external builder's spill files fall under the same self-write exclusion.
  `SEEKFS_NAME_GRAM_SPOOL_DIR` still overrides it; see `docs/CONFIG.md`.

- Volumes that hold no seekfs files keep the previous behavior and pay only a map
  lookup per change.

## Validation

- `go test ./...`, `go vet ./...`, and the CI `-race` subset are green.

- Verified against the real C: (10.0M) and F: (17.2M) indexes: 0 self-write
  changes in both WALs (was 91.7% of C:'s), no fold in over 20 minutes where the
  service previously folded every ~2.5 minutes, and `persist_failures` back to 0
  from 4.

- The NTFS file-reference form was checked against a real index (a directory
  resolves to the FRN the index stores, with its children linked by `ParentFRN`),
  which caught a bug where the 16-bit MFT sequence number was not stripped and the
  filter would have matched nothing.

## Upgrade

Drop-in for v1.10.0. No index rebuild or configuration change is required, and
existing indexes load and search unchanged. Leftover `<index>.gsi.<n>.tmp` files
from an interrupted persist are reaped automatically on the next service start or
within an hour of running.
