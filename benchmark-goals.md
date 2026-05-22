# seekfs benchmark goals

## Baseline Measurements

Everything 1.5a active database:

```text
C:\Users\abhism12\AppData\Local\Everything\Everything-1.5a.db
752,910,043 bytes (~718 MB)
```

Current `seekfs` packed v7 USN indexes:

```text
F:\seekfs_c_usn_v7packed.gsi  291,622,783 bytes
F:\seekfs_f_usn_v7packed.gsi  460,212,392 bytes
combined                         751,835,175 bytes
```

Current `seekfs` is `1,074,868` bytes smaller than Everything's active DB
for the C: + F: scope, before optional query-token sidecars. The target-query
sidecars add `2,130,327` bytes, keeping the total under the required cap but
slightly above the stretch cap when counted as part of the index.

## Search Latency Target

Manual Everything measurements through `es.exe -instance 1.5a`:

```powershell
.\extracted\es.exe -instance 1.5a -n 20 "linkmerge w6 full e10"
```

Initial result:

```text
603.67 ms
```

```powershell
.\extracted\es.exe -instance 1.5a -match-path -n 20 "linkmerge w6 full e10"
```

Initial result:

```text
933.84 ms
```

Rerun after the v7 `seekfs` build:

```text
name mode: median 155.738 ms, min 146.364 ms, max 920.666 ms, 0 rows
path mode: median 241.459 ms, min 232.601 ms, max 1,220.921 ms, 0 rows
```

Current `seekfs` C: + F: resident measurements for:

```powershell
.\seekfs.exe search -addr 127.0.0.1:<port> -path -n 20 "linkmerge w6 full e10"
```

```text
CLI-through-server: median 67.741 ms, p90 101.291 ms, max 1,151.080 ms, 9 rows
Direct TCP service: median 4.960 ms, p95 6.977 ms, max 25.895 ms, 9 rows
```

Installed Windows service measurements for:

```powershell
.\seekfs.exe search -service -path -n 20 "linkmerge w6 full e10"
```

```text
named-pipe service CLI: median 100.498 ms, p90 120.833 ms, max 1,207.344 ms, 9 rows, 0 failures over 30 runs
service startup loaded 2 DBs / 22,479,440 entries in 4.633 s
```

One-shot C: + F: CLI loading both databases from disk remains load-bound:

```text
median approximately 5.56 s over 5 runs
```

Looser validation query:

```powershell
.\extracted\es.exe -instance 1.5a -match-path -n 10 "linkmerge"
```

Returned F: results including:

```text
F:\git\rnd-surf-det-v1-vsa\runs\breach_route_vis_linkmerge_full_w6
```

## Goals

### Goal 1: Full-Path Query Latency

For the query:

```text
linkmerge w6 full e10
```

against a full C: + F: index, `seekfs` must meet:

- Required: `<= 1,000 ms` for `-path -n 20`
- Stretch: `<= 500 ms`
- Long-term target: `<= 250 ms` in resident-server mode

This is measured end-to-end from CLI invocation, matching the `es.exe` gateway
style benchmark.

### Goal 2: Index Size

For an index covering the same scope as Everything's active DB, `seekfs` must
meet:

- Required: `<= 1.1x` Everything DB size
- Stretch: `<= Everything DB size`

With the current Everything DB size of `752,910,043` bytes:

- Required max: `828,201,047` bytes
- Stretch max: `752,910,043` bytes

### Goal 3: Full-Volume Coverage

`seekfs` must build a combined or queryable C: + F: index and return the same
class of hits as Everything for:

```text
linkmerge
linkmerge full
linkmerge full w6 e10
oversample linkmerge full w6 e10
```

Correctness target:

- Top-N overlap must be explainable.
- Missing results are only acceptable if caused by a documented indexing-scope
  difference.

## Required Design Changes

To hit these goals, the current compact format still needs:

1. No stored lowercase names; normalize during build into a compact side index
   or query-time lowercase only for candidate terms.
2. Intern common extensions and repeated short names where useful.
3. Store name bytes in one contiguous blob with offsets, not Go string records.
4. Use packed parent indexes and metadata arrays.
5. Add a path-term index so full-path searches do not reconstruct every path.
6. Add combined multi-volume query support.
7. Add resident-server benchmarks separate from process startup benchmarks.

Implemented in v7:

- Compact records use packed 24-bit parent and 24-bit name-id fields.
- Repeated names are interned into a unique-name table.
- Unused size, timestamp, and mode metadata are omitted from compact search
  records.
- Path-token acceleration lives in optional `.tok` sidecars.
- `search` and `serve` support repeated `-db` flags for multi-volume querying.
- `install-service` and `service` support repeated `-db` flags so the Windows
  service can keep C: + F: loaded and `search -service` / `count -service` can
  query it over the named pipe without cold-loading the databases.
