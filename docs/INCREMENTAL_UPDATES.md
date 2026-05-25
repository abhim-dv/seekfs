# Incremental Updates Plan

`seekfs` should keep NTFS indexes fresh from the background service without
manual update commands. The service should use the NTFS USN Change Journal as
the primary update stream and rebuild only when journal replay cannot be proven
correct.

## Goals

- Reuse the service as the owner of raw-volume access and background work.
- Catch up automatically on service startup from the last saved USN checkpoint.
- Continuously apply USN changes while the service is running.
- Keep search queries consistent while updates are being applied.
- Periodically persist compact `.gsi` files atomically.
- Detect journal loss, journal ID changes, and checkpoint truncation.
- Trigger a background full rebuild when incremental replay is unsafe.

## Non-Goals For The First Pass

- Exact Everything ranking or query compatibility.
- Non-NTFS live monitoring.
- UI features.
- Cross-machine distributed indexes.

## Current Limitations

- The compact index stores parent record indexes, not persistent NTFS file
  reference numbers.
- The service loads read-optimized `Index` values and does not maintain a
  mutable FRN map.
- The service does not replay USN records on startup.
- The service does not run a per-volume background journal reader.
- `status` does not expose checkpoint lag, stale state, or rebuild state.
- Persisted DBs do not contain enough metadata to patch records safely after
  rename, move, delete, and recreate operations.

## Design

### Persisted Metadata

Add a v8 index format with per-record NTFS identity:

- `frn`
- `parent_frn`
- `name`
- `mode`
- `size`
- `modified`
- deleted/tombstone state, if needed for cheap mutation

Per-volume metadata:

- normalized volume, for example `C:`
- USN journal ID
- last processed USN
- build time
- index source

The packed v7 format can remain readable for migration, but new USN indexes
should write v8.

### Mutable Service Index

On service load, convert each USN-backed `Index` into a mutable volume state:

- `frn -> mutable record`
- `frn -> compact/search record id`
- parent/child identity through `parent_frn`
- current checkpoint and journal ID
- dirty flag and last flush time
- stale/rebuilding status

Queries should run against immutable snapshots. The simplest first version is:

1. Apply a batch under the volume write lock.
2. Rebuild the compact search snapshot for that volume after the batch.
3. Swap the pointer used by search.

That is not the most efficient possible design, but it is correct and easier to
verify. Optimize later by batching more aggressively or updating secondary
indexes incrementally.

### Startup Catch-Up

For each USN-backed DB:

1. Open the volume through the service.
2. Query the current USN journal.
3. Validate journal ID matches the DB.
4. Validate saved checkpoint is within `[LowestValidUsn, NextUsn]`.
5. Replay records from saved checkpoint to current `NextUsn`.
6. Save the new checkpoint if any changes were applied.
7. Start the continuous reader loop.

If validation fails, mark the volume stale and start a background full rebuild.

### Continuous Reader

Each indexed NTFS volume gets one goroutine:

1. Read USN records from the current checkpoint.
2. Batch records for a short interval or record count.
3. Apply the batch to the mutable FRN map.
4. Advance checkpoint only after successful application.
5. Mark dirty.
6. Periodically flush dirty snapshots to disk atomically.

### USN Record Handling

The update applier should handle:

- create/new file: add or update FRN record
- delete: remove record or mark tombstone
- rename old/new name: use the final name record and parent FRN
- move: update `parent_frn`
- metadata changes: update size and modified time when available
- directory rename/move: update the directory record; child full paths should
  resolve through parent FRNs without rewriting every child

### Rebuild Fallback

When the journal cannot be replayed safely:

1. Mark the volume stale in `status` and `loaded`.
2. Continue serving the old index with `stale=true` metadata.
3. Start a full USN rebuild in the background.
4. Save the rebuilt DB atomically.
5. Swap the loaded snapshot.
6. Clear stale/rebuilding status.

## Test Plan

Add Windows integration tests and keep them opt-in for cases that require
service installation or elevation.

Recommended files:

- `cmd/seekfs/incremental_test.go`
- `test_seekfs_incremental.ps1`
- `test_seekfs_service_incremental.ps1`
- `docs/BENCHMARKS.md`

Scenarios:

- create file, wait for service update, search finds it
- delete file, wait for service update, search no longer finds it
- rename file, old name disappears and new name appears
- move file across directories on the same volume
- rename directory containing matching children
- update modified time and verify `--recent` / `--modified-after`
- stop service, make changes, start service, verify startup catch-up
- simulate stale checkpoint and verify background rebuild status
- run concurrent searches while applying a batch of filesystem changes

## Benchmark Plan

Compare against Everything through `es.exe` where available, but keep the
benchmark harness generic and do not commit generated local results.

Metrics:

- startup catch-up latency after N changes
- live create-to-search visibility latency
- live delete-to-search disappearance latency
- query median, p90, p95, max during update load
- rebuild fallback duration
- service memory after load and after update batches
- DB size after rebuild and after flush

Initial thresholds:

- 1,000 simple create/delete/rename updates visible within 2 seconds after the
  final operation on a local SSD
- p90 service query latency under 250 ms during update load
- no query failures during concurrent update batches
- DB size remains within the current v7 release target unless v8 metadata is
  explicitly accepted as a size tradeoff

## Implementation Checklist

1. Add v8 record metadata for FRN and parent FRN.
2. Keep v7 reader support and write new USN indexes as v8.
3. Add mutable volume state in the service.
4. Convert loaded USN indexes into mutable volume states.
5. Add USN journal reader for bounded catch-up replay.
6. Implement USN batch parsing for v2/v3 records.
7. Implement mutation application for create, delete, rename, move, and metadata.
8. Rebuild/swap search snapshots safely after applied batches.
9. Add background continuous reader goroutines per volume.
10. Add periodic atomic flush with checkpoint persistence.
11. Add journal invalidation detection and background rebuild fallback.
12. Extend `status` and `loaded --json` with freshness, lag, stale, and rebuild fields.
13. Add integration tests for live updates and restart catch-up.
14. Add benchmark scripts for update latency and query latency under update load.
15. Update service, benchmark, and security docs.
