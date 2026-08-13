param(
  [string]$Seekfs = (Join-Path $PSScriptRoot "..\seekfs.exe"),
  [string]$Pipe = "\\.\pipe\seekfs-service",
  [string]$QueryFile = (Join-Path $PSScriptRoot "queries-lowmem.txt"),
  [int]$Iterations = 500,
  [int]$Warmup = 36,
  [double]$MaxP95Ms = 100,
  [double]$MaxP99Ms = 200,
  [double]$MaxCountP95Ms = 100,
  [double]$MaxCountP99Ms = 200,
  [long]$MinEntries = 1000000
)

$ErrorActionPreference = "Stop"
$loaded = (& $Seekfs loaded -pipe $Pipe --json | ConvertFrom-Json)
if (-not $loaded.ok -or $loaded.entries -lt $MinEntries) {
  throw "resident service has $($loaded.entries) entries; need at least $MinEntries for the R5 real-index gate"
}

$search = (& $Seekfs bench -service -pipe $Pipe --json -iterations $Iterations -warmup $Warmup -query-file $QueryFile | ConvertFrom-Json)
if ($LASTEXITCODE -ne 0 -or -not $search.ok -or $search.failures -ne 0) {
  throw "resident-service search benchmark failed: exit=$LASTEXITCODE failures=$($search.failures)"
}
$count = (& $Seekfs bench -service -count -pipe $Pipe --json -iterations $Iterations -warmup $Warmup -query-file $QueryFile | ConvertFrom-Json)
if ($LASTEXITCODE -ne 0 -or -not $count.ok -or $count.failures -ne 0) {
  throw "resident-service count benchmark failed: exit=$LASTEXITCODE failures=$($count.failures)"
}

[pscustomobject]@{
  service_executable = $loaded.executable
  service_version = $loaded.version
  entries = $loaded.entries
  iterations = $search.iterations
  queries = $search.queries
  search_p95_ms = $search.stats_ms.p95
  search_p99_ms = $search.stats_ms.p99
  search_backend_p95_ms = $search.backend_stats_ms.p95
  search_backend_p99_ms = $search.backend_stats_ms.p99
  count_p95_ms = $count.stats_ms.p95
  count_p99_ms = $count.stats_ms.p99
  count_backend_p95_ms = $count.backend_stats_ms.p95
  count_backend_p99_ms = $count.backend_stats_ms.p99
} | ConvertTo-Json

if ($search.stats_ms.p95 -gt $MaxP95Ms -or $search.stats_ms.p99 -gt $MaxP99Ms) {
  throw "R5 resident-service search latency p95=$($search.stats_ms.p95)ms p99=$($search.stats_ms.p99)ms; budgets are $MaxP95Ms/$MaxP99Ms ms"
}
if ($count.stats_ms.p95 -gt $MaxCountP95Ms -or $count.stats_ms.p99 -gt $MaxCountP99Ms) {
  throw "R5 resident-service count latency p95=$($count.stats_ms.p95)ms p99=$($count.stats_ms.p99)ms; budgets are $MaxCountP95Ms/$MaxCountP99Ms ms"
}
