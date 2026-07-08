# Benchmarks

Use the built-in `bench` command for a quick machine-readable benchmark over
common local queries.

Local indexes:

```powershell
.\seekfs.exe bench -db F:\seekfs_c.gsi -db F:\seekfs_f.gsi --json -iterations 100
```

Resident service:

```powershell
.\seekfs.exe bench -service --json -iterations 100
```

You can pass explicit benchmark queries after the flags:

```powershell
.\seekfs.exe bench -service --json -iterations 100 "ext:go" "type:dir docs" "glob:*.md"
```

You can also keep benchmark queries in a text file, one query per line:

```powershell
.\seekfs.exe bench -service --json -iterations 100 -query-file .\bench\queries-public.txt
```

By default, `bench` uses filename matching like `seekfs search`. Add `-path`
when measuring full-path behavior:

```powershell
.\seekfs.exe bench -service -path --json -iterations 100 "src test" "dir:src ext:go"
```

The JSON summary includes iteration count, query count, failure count, aggregate
latency stats, and per-query latency stats in milliseconds: min, median, p90,
p95, p99, and max. Use p95/p99 when evaluating planner uniformity; warm
median results are not evidence that cold-start, active-overlay, or
post-compaction states are also uniform.

## Query Shape Matters

Filename-only searches are the fastest path for exact names and executable
names:

```powershell
.\seekfs.exe search "gh.exe"
```

Use full-path matching only when the query needs path context:

```powershell
.\seekfs.exe search -path "ext:go dir:cmd main"
.\seekfs.exe search -path --under F:\git\seekfs "type:file glob:*.md"
```

On very large indexes, broad `-path` searches can be much slower than
filename-only searches because path matching may need to inspect parent
directories and reconstruct paths.

Do not commit generated benchmark output.

## R5 Lazy-Source Validation Notes

2026-07-08 local validation used a throwaway binary built from the working tree
against large real indexes outside the repository.

- Direct local, both indexes, `bench\queries-regression-user.txt`,
  path mode, 100 iterations: zero failures. The tail was dominated by
  broad/no-hit direct scans and per-query local index loading, especially
  `zzzz-no-hit-seekfs`.
- Direct local lowmem, both indexes, `bench\queries-lowmem.txt`, path mode,
  100 iterations: zero failures. Drive-scoped broad path terms and no-hit scans
  remain the slow direct-load tail.
- Standalone lowmem service on a unique pipe loaded both indexes and reported
  ready. Non-elevated USN catch-up left the volumes stale with
  `Access is denied`, so this is not a valid active-USN overlay gate.
- Service/direct acceptance from `bench\service_direct_acceptance.ps1` now
  detects active recent/overlay state and treats direct base-index parity as
  advisory in that state. Against the same standalone lowmem service, the
  acceptance matrix passed with full-count and first-page checks enabled.
- Resident service benchmark, `bench\queries-regression-user.txt`, path mode,
  100 iterations: zero failures. Notable planned sources included
  `planned:dir:downloads+ext:nrrd`, `planned:ext:nrrd`, and
  `planned:ext:nrrd+ext:raw`.
- Resident service benchmark, `bench\queries-lowmem.txt`, path mode,
  100 iterations: zero failures, with candidate counts staying bounded for the
  matrix.

The remaining cold/active-overlay/post-persist gate must be run elevated on
copied USN indexes with `bench\churn_soak.ps1`; do not run it against the live
production indexes.

## R2.6 Active-Overlay Churn Notes

2026-07-06 local validation:

- `bench/churn_soak.ps1` now reports separate search/count latency buckets,
  mutation batch latency, service `loaded --json` snapshots, and a mass-delete
  visibility probe.
- A walk-mode smoke run completed against a disposable sandbox, but it is not a
  valid active-overlay result: the service rebuilt/persisted the walk index
  rather than applying USN overlay changes.
- A disposable copy of a large real index loaded, but the service reported
  `state=stale` and `stale_reason="Access is denied."` for USN catch-up in a
  non-elevated shell. Moving the soak to a volume where an elevated service
  could read the USN journal produced a valid active-overlay run.
- Elevated active-overlay validation used a fresh v9 USN index copied into a
  disposable sandbox, with background persist disabled so the overlay/WAL
  stayed active. Service state stayed `ready`, `dirty=true`, checkpoint
  advanced, and there were zero command failures.
- Active-overlay latency from that run showed path searches remained the slow
  tail, while count and mutation batches stayed much lower. Mass create/delete
  visibility landed within the expected sub-second window.
- Two fixes were required to get a valid active-overlay run: startup v9
  catch-up now appends to WAL and leaves changes in the overlay instead of
  compacting the mmap file during service load; `bench/churn_soak.ps1` now
  measures active loop duration after service load and sleeps between
  not-ready `loaded` polls.
- Synthetic active-overlay count comparison:
  `go test ./cmd/seekfs -run TestOverlayAwareFastCount -bench BenchmarkOverlayAwareFastCountVsSearchFallback -benchtime=3x -count=1`
  reported `overlay_aware_count` at 17.742 ms/op and
  `search_merge_fallback` at 304.145 ms/op on a 200k-base-record fixture with
  10k overlay creates and 10k tombstones.
