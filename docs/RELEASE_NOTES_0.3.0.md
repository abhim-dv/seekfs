# seekfs 0.3.0 Release Notes

`seekfs` 0.3.0 focuses on service search latency and regression coverage for
common agent-facing query patterns.

## Highlights

- Broad full-path term searches now use path-term candidate postings instead of
  falling back to full-volume scans.
- Filtered service searches now use resident candidate indexes for `ext:`,
  `dir:`, `type:dir`, and regex queries with extractable literal terms.
- Incremental USN replay no longer invalidates hot posting caches on every
  update batch; recent changed records are overlaid onto cached postings.
- The service keeps multiple named-pipe listeners pending to reduce connection
  contention.
- Added regression tests for common search syntax and parity between the full
  compact scan and service candidate fast paths.

## Local Benchmark Snapshot

Measured on the development machine against full C: + F: indexes with the
installed service after warmup:

```text
seekfs search -path "Downloads nrrd"                 ~50 ms
seekfs search -path "regex:Downloads.*\.nrrd$"        ~50 ms
seekfs search -path "ext:go"                          ~50 ms
seekfs search -path "dir:cmd ext:go"                  ~50 ms
seekfs search -path --under F:\git\seekfs "glob:*.md" ~50 ms
```

Against comparable Everything ES queries on the same machine, seekfs is now at
or below Everything median latency for these benchmarked cases. First query
after service restart can still pay cache-build cost.

## Known Limitations

- Current v8 indexes remain larger than desired. The smaller v9 format is still
  deferred to a future release.
- Regex acceleration depends on literal terms that can be extracted from the
  pattern. Regexes with no useful literals may still require broad scans.
- Windows and NTFS remain the primary target.
- The artifact is unsigned. Windows may show standard warnings for unsigned
  executables.

## Upgrade

1. Stop the service from an elevated shell:

   ```powershell
   sc stop seekfs
   ```

2. Replace `seekfs.exe`.
3. Start the service:

   ```powershell
   sc start seekfs
   ```

4. Check health:

   ```powershell
   .\seekfs.exe loaded --json
   .\seekfs.exe search "gh.exe"
   ```
