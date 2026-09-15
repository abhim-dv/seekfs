# seekfs v1.10.2

A torn WAL tail no longer puts the service into a back-to-back fold loop, and
its memory with it.

## Fixed

- **A corrupt WAL tail blocked the post-fold WAL rewrite.** After a fold, the
  service rewrites the WAL to keep only the frames that are not yet in the
  persisted index. If the WAL ended in a partly-written frame -- which happens
  when a persist is killed mid-append -- the read failed and the rewrite was
  skipped entirely, so the WAL kept every frame, including the ones just folded
  in, and stayed above its 64 MB size trigger. The next fold was then due
  immediately.

  Observed on a live F: index:

  ```text
  10:46:58 background persist wal rewrite skipped volume=F: err=wal frame crc mismatch
  10:46:58 background persist complete volume=F: duration=3m15.538s
  10:46:59 background persist start volume=F: wal_bytes=93659099
  ```

  Every 3m15s fold writes ~4 GB and rebuilds the name trigram, which is what
  held the service at ~10 GB RSS. The rewrite now keeps the frames that read
  cleanly and drops the unreadable remainder, so the WAL returns under its
  trigger. A dropped tail is unrecoverable from the WAL either way, and the
  journal still covers that USN range on the next startup catch-up.

## Validation

- `go test ./...`, `go vet ./...`, and the CI `-race` subset are green.
- New tests cover the corrupt-tail read (the good prefix is returned with the
  error) and a rewrite of that prefix (the WAL reads cleanly afterwards), plus
  rewriting with no frames resetting the WAL.

## Upgrade

Drop-in for v1.10.1. No index rebuild or configuration change is required. A
service already stuck in the loop stops folding back to back as soon as the
first fold completes on the new build.
