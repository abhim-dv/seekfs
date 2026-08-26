# seekfs v1.4.0

Adds `seekfs watch` — saved queries as event sources.

## Highlights

- **`seekfs watch "<query>" [flags]`** — a long-lived process that polls
  the resident service, diffs each result set against the previous tick,
  and streams change events to stdout as JSON lines:

  ```text
  {"ts":"2026-08-26T18:16:40Z","event":"created","path":"F:\\reports\\Q3.pdf","size":371752}
  {"ts":"...","event":"modified","path":"F:\\reports\\Q3.pdf","size":380001}
  {"ts":"...","event":"deleted","path":"F:\\drafts\\old.pdf"}
  ```

  - **Silent baseline**: pre-existing matches are not emitted; stderr
    reports the watch count.
  - **All planner features apply**: filters (`ext:`, `dir:`), fuzzy
    chaining, sorting — the query string is passed to the service verbatim.
  - **Outage-tolerant**: service blips retry without emitting a deletion
    storm, then re-baseline silently on reconnect.
  - **Flags reorderable**: `-interval`, `-n`, `-pipe`, `-config` work before
    or after the query.
  - Events: `created`, `modified` (size/mtime change), `deleted`. Renames
    appear as deleted+created pairs.

  Flags: `-interval` (default 2s), `-n` (default 10000 result cap per tick).

- Engine, planner, and service are unchanged — `watch` is a thin client
  over the existing pipe API, so it works against any running service.

## Known limitation

Queries carrying `ext:` (or `path:`/`dir:`) filters currently land in the
bounded-scan lane and can take seconds per tick (a pre-existing planner
issue, surfaced by watch testing). Bare-term queries are fast. A dedicated
fix for the `ext:` scan cliff is planned.
