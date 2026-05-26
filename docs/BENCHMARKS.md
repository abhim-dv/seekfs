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
p95, and max.

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
