# Benchmark Harnesses

Benchmark scripts in this directory are for local, machine-specific validation.
They do not commit generated corpora, DBs, or result files.

Recommended output location:

```powershell
F:\seekfs_bench_results\<timestamp>
```

Recommended sandbox location:

```powershell
F:\seekfs_bench_sandbox
```

## Query Baseline

```powershell
go build -o seekfs.exe ./cmd/seekfs
.\seekfs.exe doctor --json
.\seekfs.exe loaded --json
.\seekfs.exe bench -service --json -iterations 500 "ext:go" "type:dir docs" "glob:*.md"
```

R5 resident and isolated current-binary gates:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\bench\r5_service_gate.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File .\bench\r5_cold_persist_gate.ps1
```

The cold/persist gate uses an alternate named pipe and private index clones; it
never starts a second writer on the installed service's index paths. When the
gate will compact indexes it copies them rather than hard-linking them, because
NTFS hard links would make compaction rewrite the installed source index.

## Incremental Update Targets

Once background USN replay is implemented, benchmark:

- create-to-search visibility
- delete-to-search disappearance
- rename old-hidden and new-visible latency
- move old-path-hidden and new-path-visible latency
- restart catch-up latency
- query p50/p90/p95/p99/max while updates are applying

Compare with Everything through `es.exe` when available. Keep `es.exe` and all
Everything databases outside the repo.

## R5 mapped-v9 query-tail evidence

The low-memory service benchmark records a canonical `result_hash` (or count
hash) and representative source/driver, completeness, candidate, verification,
and block diagnostics for every query. The persisted v9 filename path uses
selective `PNGR` plus the optional complete-common `PNGC` companion. Its query
posting prefetch is bounded by `SEEKFS_QUERY_POSTING_PREFETCH_BYTES` (32 MiB by
default; `0` disables it for comparison), touches only selected posting block
ranges, and is cancellation-aware. Legacy or incomplete PNGR remains a safe
fallback and is not represented as PNGC-complete.
