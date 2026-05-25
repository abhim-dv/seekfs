# seekfs 0.2.0 Release Notes

`seekfs` 0.2.0 focuses on resident service reliability, live NTFS/USN
incremental updates, and simpler agent-facing CLI calls.

## Highlights

- `search` and `count` use the resident service by default when no `-db` is
  supplied.
- `output_format = "json"` and `default_limit` in `seekfs.toml` make short agent
  calls possible, for example `seekfs search "gh.exe"`.
- Resident service exact filename lookup is indexed in memory.
- `--under <path>` service searches use the FRN child map for fast workspace
  subtree queries.
- Live USN replay updates the resident index without blocking on full `.gsi`
  rewrites.
- `.gsi.wal` sidecar replay preserves debounced incremental updates across
  service restarts.
- `loaded --json` reports service PID, per-volume state, checkpoints, and FRN
  record counts.
- `info --json` reports index layout size contributors.

## Local Benchmark Snapshot

Measured on the development machine against full C: + F: indexes with the
installed service:

```text
seekfs search gh.exe                         59 ms
seekfs search --json -n 20 gh.exe            81 ms
seekfs search --json -path -n 20 gh.exe      65 ms
--under F:\git\seekfs type:file glob:*.md    51 ms
```

Live incremental benchmark, 10 iterations against Everything 1.5a:

```text
seekfs create median:     350 ms
seekfs delete median:     280 ms
Everything create median: 733 ms
Everything delete median: 750 ms
failures: 0
```

## Known Limitations

- Broad count queries, such as `count -path "type:file ext:go"`, can still take
  several seconds because extension/type count indexes are not implemented yet.
- Current v8 indexes are larger than desired. On the development C: + F: corpus
  they total about 1.59 GB. A smaller v9 format is deferred to a future release.
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
