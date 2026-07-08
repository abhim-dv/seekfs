param(
  [string]$Seekfs = ".\seekfs.exe",
  [ValidateSet("Walk", "USN")]
  [string]$Mode = "Walk",
  [string]$Root = (Join-Path ([IO.Path]::GetTempPath()) "seekfs_bench_sandbox\churn-soak"),
  [string]$OutDir = (Join-Path ([IO.Path]::GetTempPath()) "seekfs_bench_results\churn-soak"),
  [string]$DB = "",
  [string]$Volume = "F:",
  [int]$DurationMinutes = 10,
  [int]$SeedFiles = 1000,
  [int]$MutationBatch = 25,
  [int]$QueryIntervalMs = 250,
  [int]$MassDeleteFiles = 1000,
  [int]$VisibilityTimeoutSeconds = 30,
  [int]$RestartEveryMinutes = 0,
  [int]$ServiceLoadTimeoutSeconds = 180,
  [switch]$EngineV9,
  [switch]$Lowmem,
  [switch]$DisableBackgroundPersist,
  [switch]$SkipBuild,
  [switch]$KeepSandbox
)

$ErrorActionPreference = "Stop"

function Assert-SandboxPath {
  param([string]$Path)
  $full = [IO.Path]::GetFullPath($Path)
  $rootFull = [IO.Path]::GetFullPath($Root)
  if (-not $full.StartsWith($rootFull, [StringComparison]::OrdinalIgnoreCase)) {
    throw "refusing to operate outside sandbox root: $full"
  }
}

function New-Stats {
  param([double[]]$Values)
  if ($Values.Count -eq 0) { return @{ count = 0 } }
  $sorted = @($Values | Sort-Object)
  function Pick([double[]]$Data, [double]$P) {
    $idx = [int][Math]::Ceiling(($Data.Count * $P)) - 1
    if ($idx -lt 0) { $idx = 0 }
    if ($idx -ge $Data.Count) { $idx = $Data.Count - 1 }
    return [Math]::Round($Data[$idx], 3)
  }
  return @{
    count = $sorted.Count
    min = [Math]::Round($sorted[0], 3)
    median = Pick $sorted 0.50
    p90 = Pick $sorted 0.90
    p95 = Pick $sorted 0.95
    p99 = Pick $sorted 0.99
    max = [Math]::Round($sorted[$sorted.Count - 1], 3)
  }
}

function Invoke-Json {
  param([string[]]$ToolArgs)
  $out = & $Seekfs @ToolArgs
  if ($LASTEXITCODE -ne 0) {
    throw "seekfs failed: $($ToolArgs -join ' ')"
  }
  return $out | ConvertFrom-Json
}

function New-Corpus {
  New-Item -ItemType Directory -Force -Path $Root | Out-Null
  foreach ($dir in @("alpha", "beta", "gamma", "moves")) {
    New-Item -ItemType Directory -Force -Path (Join-Path $Root $dir) | Out-Null
  }
  for ($i = 0; $i -lt $SeedFiles; $i++) {
    $dir = Join-Path $Root (@("alpha", "beta", "gamma")[$i % 3])
    $name = "seed_{0:D6}_seekfs_soak.txt" -f $i
    Set-Content -LiteralPath (Join-Path $dir $name) -Value "seed $i"
  }
}

function Build-Index {
  if ($DB -eq "") {
    $script:DB = Join-Path $OutDir ("churn-" + $Mode.ToLowerInvariant() + ".gsi")
  }
  New-Item -ItemType Directory -Force -Path (Split-Path -Parent $DB) | Out-Null
  if ($SkipBuild) {
    if (-not (Test-Path $DB)) {
      throw "SkipBuild requested but DB does not exist: $DB"
    }
    return
  }
  if ($Mode -eq "USN") {
    & $Seekfs index-usn -volume $Volume -db $DB | Out-Host
  } else {
    & $Seekfs index -root $Root -db $DB | Out-Host
  }
  if ($LASTEXITCODE -ne 0) {
    throw "index build failed"
  }
}

function Start-SeekfsService {
  $pipe = "\\.\pipe\seekfs-soak-$([guid]::NewGuid().ToString("N"))"
  $args = @("service", "-db", $DB, "-pipe", $pipe)
  if ($Lowmem) { $args += "-lowmem" }
  $oldV9 = $env:SEEKFS_ENGINE_V9
  $oldMem = $env:SEEKFS_MEMORY_MODE
  $oldPersist = $env:SEEKFS_DISABLE_BACKGROUND_PERSIST
  if ($EngineV9) { $env:SEEKFS_ENGINE_V9 = "1" }
  if ($Lowmem) { $env:SEEKFS_MEMORY_MODE = "lowmem" }
  if ($DisableBackgroundPersist) { $env:SEEKFS_DISABLE_BACKGROUND_PERSIST = "1" }
  $proc = Start-Process -FilePath (Resolve-Path $Seekfs).Path -ArgumentList $args -PassThru -WindowStyle Hidden
  $env:SEEKFS_ENGINE_V9 = $oldV9
  $env:SEEKFS_MEMORY_MODE = $oldMem
  $env:SEEKFS_DISABLE_BACKGROUND_PERSIST = $oldPersist
  $deadline = (Get-Date).AddSeconds($ServiceLoadTimeoutSeconds)
  while ((Get-Date) -lt $deadline) {
    try {
      $loaded = Invoke-Json @("loaded", "-pipe", $pipe, "--json")
      if ($loaded.ok -and $loaded.entries -gt 0) {
        return @{ process = $proc; pipe = $pipe; loaded = $loaded }
      }
    } catch {
    }
    Start-Sleep -Milliseconds 250
  }
  Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
  throw "service did not load $DB"
}

function Stop-SeekfsService {
  param($Service)
  if ($null -ne $Service -and $null -ne $Service.process -and -not $Service.process.HasExited) {
    Stop-Process -Id $Service.process.Id -Force -ErrorAction SilentlyContinue
    $Service.process.WaitForExit(5000) | Out-Null
  }
}

function Invoke-Mutations {
  param([int]$Batch, [int]$Iteration)
  for ($i = 0; $i -lt $Batch; $i++) {
    $id = "{0:D8}_{1:D4}" -f $Iteration, $i
    $created = Join-Path $Root "alpha\create_$id.log"
    Assert-SandboxPath $created
    Set-Content -LiteralPath $created -Value "create $id"

    $renamed = Join-Path $Root "beta\rename_$id.log"
    Assert-SandboxPath $renamed
    Move-Item -LiteralPath $created -Destination $renamed -Force

    $moved = Join-Path $Root "moves\moved_$id.log"
    Assert-SandboxPath $moved
    Move-Item -LiteralPath $renamed -Destination $moved -Force

    if (($i % 3) -eq 0) {
      Remove-Item -LiteralPath $moved -Force
    }
  }
}

function Invoke-QuerySample {
  param([string]$Pipe)
  $queries = @(
    @{ args = @("search", "-service", "-pipe", $Pipe, "-path", "--under", $Root, "-n", "50", "seekfs_soak"); name = "seed"; kind = "search" },
    @{ args = @("count", "-service", "-pipe", $Pipe, "-path", "--under", $Root, "moved_"); name = "moved_count"; kind = "count" },
    @{ args = @("search", "-service", "-pipe", $Pipe, "-path", "--under", (Join-Path $Root "moves"), "-n", "25", "ext:log"); name = "moves_ext"; kind = "search" }
  )
  $out = @()
  foreach ($query in $queries) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    try {
      & $Seekfs @($query.args) | Out-Null
      $ok = ($LASTEXITCODE -eq 0)
    } catch {
      $ok = $false
    }
    $sw.Stop()
    $out += @{ name = $query.name; kind = $query.kind; ok = $ok; ms = $sw.Elapsed.TotalMilliseconds }
  }
  return $out
}

function Add-Latency {
  param($Map, [string]$Name, [double]$Ms)
  if (-not $Map.ContainsKey($Name)) {
    $Map[$Name] = New-Object System.Collections.Generic.List[double]
  }
  $Map[$Name].Add($Ms)
}

function Convert-LatencyMap {
  param($Map)
  $out = [ordered]@{}
  foreach ($key in ($Map.Keys | Sort-Object)) {
    $out[$key] = New-Stats $Map[$key].ToArray()
  }
  return $out
}

function Invoke-CountValue {
  param([string]$Pipe, [string]$Under, [string]$Query)
  $json = & $Seekfs @("count", "-service", "-pipe", $Pipe, "-path", "--under", $Under, "--json", $Query)
  if ($LASTEXITCODE -ne 0) {
    throw "count failed for visibility probe: $Query"
  }
  $parsed = $json | ConvertFrom-Json
  return [int]$parsed.count
}

function Wait-CountAtLeast {
  param([string]$Pipe, [string]$Under, [string]$Query, [int]$Want, [int]$TimeoutSeconds)
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  $last = -1
  while ((Get-Date) -lt $deadline) {
    $last = Invoke-CountValue -Pipe $Pipe -Under $Under -Query $Query
    if ($last -ge $Want) {
      return @{ ok = $true; count = $last }
    }
    Start-Sleep -Milliseconds 250
  }
  return @{ ok = $false; count = $last }
}

function Wait-CountEquals {
  param([string]$Pipe, [string]$Under, [string]$Query, [int]$Want, [int]$TimeoutSeconds)
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  $last = -1
  while ((Get-Date) -lt $deadline) {
    $last = Invoke-CountValue -Pipe $Pipe -Under $Under -Query $Query
    if ($last -eq $Want) {
      return @{ ok = $true; count = $last }
    }
    Start-Sleep -Milliseconds 250
  }
  return @{ ok = $false; count = $last }
}

function Invoke-MassDeleteProbe {
  param([string]$Pipe)
  $dir = Join-Path $Root "burst"
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $query = "burst_seekfs_soak_delete_probe"
  $createSw = [Diagnostics.Stopwatch]::StartNew()
  for ($i = 0; $i -lt $MassDeleteFiles; $i++) {
    $path = Join-Path $dir ("burst_seekfs_soak_delete_probe_{0:D6}.tmp" -f $i)
    Assert-SandboxPath $path
    Set-Content -LiteralPath $path -Value "burst $i"
  }
  $createSw.Stop()
  $visibleCreateSw = [Diagnostics.Stopwatch]::StartNew()
  $created = Wait-CountAtLeast -Pipe $Pipe -Under $dir -Query $query -Want $MassDeleteFiles -TimeoutSeconds $VisibilityTimeoutSeconds
  $visibleCreateSw.Stop()

  $deleteSw = [Diagnostics.Stopwatch]::StartNew()
  Get-ChildItem -LiteralPath $dir -Filter "burst_seekfs_soak_delete_probe_*.tmp" | Remove-Item -Force
  $deleteSw.Stop()
  $visibleDeleteSw = [Diagnostics.Stopwatch]::StartNew()
  $deleted = Wait-CountEquals -Pipe $Pipe -Under $dir -Query $query -Want 0 -TimeoutSeconds $VisibilityTimeoutSeconds
  $visibleDeleteSw.Stop()

  return [ordered]@{
    files = $MassDeleteFiles
    create_fs_ms = [Math]::Round($createSw.Elapsed.TotalMilliseconds, 3)
    create_visible = [bool]$created.ok
    create_visible_ms = [Math]::Round($visibleCreateSw.Elapsed.TotalMilliseconds, 3)
    create_observed_count = $created.count
    delete_fs_ms = [Math]::Round($deleteSw.Elapsed.TotalMilliseconds, 3)
    delete_visible = [bool]$deleted.ok
    delete_visible_ms = [Math]::Round($visibleDeleteSw.Elapsed.TotalMilliseconds, 3)
    delete_observed_count = $deleted.count
  }
}

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
Assert-SandboxPath $Root
if (-not $KeepSandbox -and (Test-Path $Root)) {
  Remove-Item -LiteralPath $Root -Recurse -Force
}
New-Corpus
Build-Index

$service = $null
$latencies = New-Object System.Collections.Generic.List[double]
$searchLatencies = New-Object 'System.Collections.Generic.Dictionary[string,System.Collections.Generic.List[double]]'
$countLatencies = New-Object 'System.Collections.Generic.Dictionary[string,System.Collections.Generic.List[double]]'
$mutationLatencies = New-Object System.Collections.Generic.List[double]
$failures = 0
$mutations = 0
$restarts = 0
$maxWalBytes = 0L
$maxPrivateBytes = 0L
$started = $null
$deadline = [DateTime]::MaxValue
$nextRestart = [DateTime]::MaxValue
$iteration = 0

try {
  $service = Start-SeekfsService
  $loadedBefore = Invoke-Json @("loaded", "-pipe", $service.pipe, "--json")
  $massDeleteProbe = Invoke-MassDeleteProbe -Pipe $service.pipe
  $started = Get-Date
  $deadline = $started.AddMinutes($DurationMinutes)
  $nextRestart = if ($RestartEveryMinutes -gt 0) { $started.AddMinutes($RestartEveryMinutes) } else { [DateTime]::MaxValue }
  while ((Get-Date) -lt $deadline) {
    $mutationSw = [Diagnostics.Stopwatch]::StartNew()
    Invoke-Mutations -Batch $MutationBatch -Iteration $iteration
    $mutationSw.Stop()
    $mutationLatencies.Add([double]$mutationSw.Elapsed.TotalMilliseconds)
    $mutations += $MutationBatch
    foreach ($sample in (Invoke-QuerySample -Pipe $service.pipe)) {
      if ($sample.ok) {
        $latencies.Add([double]$sample.ms)
        if ($sample.kind -eq "count") {
          Add-Latency -Map $countLatencies -Name $sample.name -Ms ([double]$sample.ms)
        } else {
          Add-Latency -Map $searchLatencies -Name $sample.name -Ms ([double]$sample.ms)
        }
      } else {
        $failures++
      }
    }
    if (Test-Path "$DB.wal") {
      $maxWalBytes = [Math]::Max($maxWalBytes, (Get-Item "$DB.wal").Length)
    }
    try {
      $proc = Get-Process -Id $service.process.Id -ErrorAction Stop
      $maxPrivateBytes = [Math]::Max($maxPrivateBytes, [int64]$proc.PrivateMemorySize64)
    } catch {}
    if ((Get-Date) -ge $nextRestart) {
      Stop-SeekfsService $service
      $service = Start-SeekfsService
      $restarts++
      $nextRestart = (Get-Date).AddMinutes($RestartEveryMinutes)
    }
    $iteration++
    Start-Sleep -Milliseconds $QueryIntervalMs
  }
  $loadedAfter = Invoke-Json @("loaded", "-pipe", $service.pipe, "--json")
}
finally {
  Stop-SeekfsService $service
}

$ended = Get-Date
if ($null -eq $started) {
  $started = $ended
}
$summary = [ordered]@{
  ok = ($failures -eq 0)
  mode = $Mode
  engine_v9 = [bool]$EngineV9
  lowmem = [bool]$Lowmem
  disable_background_persist = [bool]$DisableBackgroundPersist
  skip_build = [bool]$SkipBuild
  root = $Root
  db = $DB
  duration_seconds = [Math]::Round(($ended - $started).TotalSeconds, 3)
  mutations = $mutations
  restarts = $restarts
  failures = $failures
  query_latency_ms = New-Stats $latencies.ToArray()
  search_latency_ms = Convert-LatencyMap $searchLatencies
  count_latency_ms = Convert-LatencyMap $countLatencies
  mutation_batch_latency_ms = New-Stats $mutationLatencies.ToArray()
  mass_delete_probe = $massDeleteProbe
  loaded_before = $loadedBefore
  loaded_after = $loadedAfter
  max_wal_bytes = $maxWalBytes
  max_private_bytes = $maxPrivateBytes
}

$json = $summary | ConvertTo-Json -Depth 8
$out = Join-Path $OutDir ("churn-soak-" + (Get-Date -Format "yyyyMMdd-HHmmss") + ".json")
$json | Set-Content -LiteralPath $out
$json
Write-Host "Wrote $out" -ForegroundColor Green
