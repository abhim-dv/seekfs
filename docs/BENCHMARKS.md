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

The JSON summary includes iteration count, query count, failure count, and
latency stats in milliseconds: min, median, p90, p95, and max.

Do not commit generated benchmark output.
