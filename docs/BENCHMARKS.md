# Benchmarks

## Everything Baseline

Use `bench_everything.py` with `--tool-kind es` to benchmark Everything through
`es.exe`.

```powershell
python .\bench_everything.py `
  --tool-kind es `
  --tool .\extracted\es.exe `
  --instance 1.5a `
  --roots C:\ F:\ `
  --queries 100 `
  --iterations 3 `
  --out-prefix everything_bench
```

Do not commit generated CSV or summary files.

## seekfs Service Benchmark

After installing the service with C: and F: indexes:

```powershell
.\seekfs.exe search -service -path -n 20 "linkmerge w6 full e10"
.\seekfs.exe count  -service -path "linkmerge w6 full e10"
```

For repeatable agent-oriented benchmarks, use a script that records:

- command line
- query set
- iteration count
- min, median, p90, p95, and max latency
- failure count
- result count

The built-in agent benchmark mode is still on the release TODO list.
