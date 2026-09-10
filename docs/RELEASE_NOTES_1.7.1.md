# seekfs v1.7.1

Freshness: a file change is now picked up and applied as soon as the USN
journal reports it, instead of after a fixed delay.

## Fixes

- **No more 500 ms replay throttle.** The background USN replay loop slept
  500 ms after *every* batch, including successful ones, so a change arriving
  right after a batch waited up to 500 ms to become visible and a large
  backlog drained at one batch per half-second. The loop now sleeps only when
  a read reports no progress; a productive read re-arms the blocking journal
  wait immediately, so visibility latency is bounded by apply time.

- **Bounded in-call drain.** One replay pass now holds the volume handle open
  and applies up to eight batches (resuming from the applied checkpoint,
  including the truncated remainder of an oversized batch) before yielding.
  This avoids a `CreateFile` + journal query per tiny batch on a busy volume
  while still letting the stall watchdog retire the loop between passes.

The change reuses the existing blocking wait read (`BytesToWaitFor=1`); it does
not alter the persisted index, WAL format, or wire protocol.

## Upgrade

Drop-in for v1.7.0. No index rebuild or configuration change is required.
