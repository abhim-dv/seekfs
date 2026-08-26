# seekfs v1.5.0

`seekfs watch` becomes a real automation surface: delta-only polling (no more
full re-query every tick), full search-flag parity, and match-triggered
actions.

## Highlights

- **Delta polling.** watch previously re-ran the full query each poll and
  diffed the capped top-N result set, so broad watches silently missed
  events outside the top-N and paid a full re-query per tick. watch now asks
  the service for *only the changed records* since the last tick (per-volume
  overlay cursors), filtered through the query. Ticks cost O(changed), not
  O(result set).

- **Silent baseline, outage-tolerant reset.** Baseline establishes the
  per-volume cursors without emitting pre-existing matches. If a background
  persist rebuilds a volume's overlay, the client re-baselines that volume
  silently — a compaction never surfaces as a deletion storm.

- **`-exec "<command>"`** — run a program for each event; `{}` is replaced by
  the event path. `-exec-on created,modified,deleted` selects which events
  trigger it; `-exec-shell` passes the command through `cmd /C` for shell
  semantics. Launch is async, so watch never blocks on a slow action.

- **Full search flag parity** on watch: `--under`, `-path`, `-exists`,
  `-recent`, `-modified-after`, `-case`, `-fuzzy`, `-cwd-bias`,
  `-root-bias` — so scoped watches (a folder, an extension, a project) work
  exactly like scoped searches.

  ```text
  seekfs watch --under F:\inbox "ext:pdf" -exec "notify.vbs {}" -exec-on created
  ```

- **Server-side delta endpoint** (`watch-delta` pipe command): per-volume
  overlay watermark cursors, events matching poll-and-diff semantics
  (created/modified/deleted, renames as deleted+created, created-then-
  deleted-in-window silent), reset signal on overlay rebuild.

## Known limitation

The delta stream reflects each volume's published overlay snapshot. On the
C: volume in production the USN replay loop is currently stalled (checkpoint
frozen at 927093323584 despite journal data ahead, no error logged, state
`ready`), so C: watch events are not advancing. F: and healthy volumes work.
This is a pre-existing engine issue (the C: USN journal was recreated, its
journal ID changed) surfaced by watch testing, tracked for a dedicated fix.

## Fixes

- `watch-delta` reads the published snapshot, never `vol.mu`, so a long
  background persist (multi-GB overlay compaction) can no longer freeze all
  pipe requests while holding `indexMu.RLock`.
