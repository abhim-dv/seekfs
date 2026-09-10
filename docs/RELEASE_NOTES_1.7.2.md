# seekfs v1.7.2

The USN replay loop can now be stopped promptly, and a retired replay loop can
no longer overwrite the state of the loop that replaced it.

## Highlights

- **Cancelable change-journal reads.** The replay loop read the USN journal
  through a synchronous `FSCTL_READ_USN_JOURNAL` that stayed outstanding until
  a filesystem change arrived, so stopping the service or retiring a stalled
  loop on a quiet volume could block for seconds (or until the next change).
  The read now runs on a volume handle opened with `FILE_FLAG_OVERLAPPED` and
  is canceled with `CancelIoEx` when the service is stopping or a watchdog
  restart bumps the replay generation.

- **Retired replay loops no longer clobber volume state.** A replay loop that
  was replaced while its read was in flight could still mark the volume `stale`
  with its own error, overwriting the reason the stall watchdog recorded. The
  watchdog now retires the loop's generation under the same lock that records
  the rebuild reason, and a retired loop's error path leaves the volume state
  alone. This also removes a race that made the replay-stall tests flaky under
  load.

- **Faster shutdown.** With a canceled wait the loop returns immediately
  instead of sleeping its idle delay before re-checking the stop signal.

## Testing

- New opt-in live test `TestUSNOverlappedReaderLive`
  (`SEEKFS_LIVE_USN_TEST=1`, elevated) verifies cancellation returns promptly
  and that a created file's USN record surfaces through the overlapped read.
  On the development machine a created file was observed in ~3.9 ms.
- `go test ./...` and `go vet ./...` are green.

## Upgrade

Drop-in for v1.7.1. No index rebuild or configuration change is required.
