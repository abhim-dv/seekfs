# seekfs v1.7.0

Client/server groundwork lands and the service stops wedging: a principal-aware
pipe, a read-only remote transport, and a persist that no longer freezes the
whole index. Broad queries get materially faster on the grammar and regex
lanes.

## Highlights

- **Principal-aware service pipe.** The named pipe is now deny-by-default:
  read-only commands (`search`, `info`, `status`, `watch-delta`) are available
  to any caller, while the privileged `index-usn` mutation requires an elevated
  caller. The service derives the caller's principal by impersonating the pipe
  client (`ImpersonateNamedPipeClient`), inspects the token while impersonating,
  and always reverts before doing engine work. Mutation is granted only to
  LocalSystem or a UAC-elevated member of Builtin Administrators; every
  token/group-query failure fails closed to read-only. The pipe is created with
  `PIPE_REJECT_REMOTE_CLIENTS`, so SMB clients cannot reach the local-only pipe.

- **Mode L remote transport (opt-in, disabled by default).** A loopback TCP
  listener carries a versioned length-prefixed frame envelope
  (`{type,id,v,payload}`: hello/request/response/cancel/error) and dispatches
  through the same command handler with a read-only remote principal. Non-loopback
  binds are refused; frames are size-bounded; unknown versions and malformed
  frames close the connection. Every remote reply passes through one
  `sanitizeRemoteResponse` projection that strips process identity, pipe name,
  memory, DB paths, journal ids, checkpoints, and planner internals while
  preserving results, counts, health, and coarse planner source. Remote
  requests carry connection-scoped cancellation and a server-owned deadline
  clamp, and cannot cancel another client's or the local GUI's query. Enable
  with `-remote-addr` / the `remote_addr` config key; empty (default) is off.

- **Persist no longer freezes search.** The background persist used to hold the
  global index lock for the entire multi-GB fold+stage, blocking every
  `info`/`search` for the persist window (measured 8m14s of staleness on
  production volumes). Persist now snapshots the overlay in milliseconds,
  releases the lock so USN replay keeps applying changes, carries the
  post-snapshot overlay delta into the replacement base, and swaps under a brief
  lock. The WAL is rewritten down to the post-snapshot frames so nothing folded
  is kept twice and nothing applied is lost on crash.

- **No more duplicate service.** A single 500 ms info attempt treated any
  hiccup (such as a persist swap holding a lock) as a dead service and spawned a
  *second* service over the same pipe and index files; the two then fought over
  the `.gsi` files (commit failures, lock errors, interleaved WAL frames, CRC
  failures, multi-GB duplicate memory). `pollServiceInfoReady` now retries slow
  replies and only falls through to spawning after repeated fast connect
  failures.

## Search

- Global bounded-scan fallbacks are pre-filtered by `ext:`/`glob-ext:` postings
  and a bounded `type:dir` subtree union, so scans skip non-matching records
  while preserving exact scan order and top-N semantics.
- The global components lane streams candidates and keeps only the top-N in a
  bounded heap (one verification pass, O(limit) memory): a broad regex path
  went from 28s to tens of ms.
- Pure-substring single-literal regexes (`regex:.*test.*`) on multi-volume
  services are declined from the components lane and served by the bounded-scan
  literal pre-filter. `regex:README\.(md|txt)$` dropped from a 2m13s full-volume
  scan to ~0.5s and `regex:.*test.*` from 28s to ~1.6s, both order-correct.
- Filename globs with a literal run now drive the gram lane instead of always
  declining to a bounded scan (`glob:*report*`: 21.9s → 88ms; a mid-word
  extension glob: 1.9s → 165ms). Globs with character classes or single-char
  wildcards still decline.
- Short (1–2 rune) companion terms ride the gram-intersection lane as
  verify-only companions, with the fold run before the `maxIDs` gate so a broad
  driver cannot decline first. `-fuzzy` short terms get insertion-variant
  rewrite trials, ranked ahead of longer terms' deletion variants.
- Bare path-separator tokens (`/`, `\`) are dropped at parse time so they stop
  gating the fast lanes. `seekfs agent` gained dogfood-driven guidance and
  `seekfs --help` now points agents at it.

## Fixes

- `index-usn` persists the applied WAL checkpoint on truncated replay batches and
  logs persist windows, so a truncated batch no longer forces a re-replay of
  frames already folded.
- Stale-recovery no longer re-enters its rebuild loop on a locked index; rebuilt
  index memory is released on failure.
- The replay loop backs off instead of re-running a full locked-index rebuild on
  every tick.
- The UI binary embeds a `requireAdministrator` manifest so it self-elevates on
  launch, and the window icon is generated into the same `.rsrc` section at the
  resource id Wails loads (fixes the missing title-bar icon).

## Notes

- Mode L is a development/proof-of-transport loopback only. Production LAN
  exposure (broker + auth) is future work; the default configuration does not
  open any socket.
- The release artifact remains unsigned. Windows will warn on first run.

## Upgrade

Extract over the previous install. The service uses the v9 columnar `.gsi`
format (`GOSRCH09`); an index written by an older format is rejected and
rebuilt on first start. No configuration change is required and Mode L stays
off unless `-remote-addr` / `remote_addr` is set.
