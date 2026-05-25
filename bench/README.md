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
