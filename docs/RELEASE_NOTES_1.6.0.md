# seekfs v1.6.0

The service now polices itself: a silently stalled USN replay loop is
detected and restarted in place instead of quietly missing every change on
the volume.

## Highlights

- **Replay-stall watchdog.** The C: volume failure mode was silent: state
  `ready`, no errors logged, checkpoint frozen while the OS journal reported
  data past it — every file change after the freeze was missed. A watchdog
  now checks each volume every 15s: if the checkpoint is frozen across two
  consecutive observations while the journal's next USN is ahead, the stall
  is acted on.

- **Graduated recovery, no reflex rebuilds.** Strikes 1-2 restart only the
  replay loop: a generation bump retires the wedged goroutine and a fresh
  loop resumes from the persisted checkpoint — the index is untouched, so
  recovery costs seconds, not a multi-minute rescan. Only after 3 strikes
  does the watchdog escalate to a full rebuild via the stale-recovery path.

- **Correctness hardening around recovery.** A restarted or rebuilt loop
  retires quietly instead of double-applying (generation guards in both the
  replay and persist loops), an in-flight USN batch read by the retired loop
  is dropped, recovery sets a `recovering` flag so the watchdog cannot
  interfere with itself, and healthy applies reset strike bookkeeping.

- **Replay health surfaced.** `info` JSON now reports per-volume
  `last_replay_at`, `last_replay_error`, and `last_replay_next`; `doctor`
  and `status` print per-volume state with the stall/replay-error reason, so
  a stalled volume is visible instead of invisible.

  ```text
  seekfs doctor
    volume C:: state=ready entries=10060292
    volume F:: state=ready entries=16895463
  ```

## Fixes

- Watch telemetry from v1.5.0 stays honest on C:: the frozen-checkpoint
  stall that kept C: watch events from advancing is now detected within
  minutes and recovered without operator intervention.

- Duplicate persist loops after a watchdog-triggered rebuild are prevented
  (persist generation retirement), matching the replay loop guard.
