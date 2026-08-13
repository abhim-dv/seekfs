param(
  [string]$Seekfs = (Join-Path $PSScriptRoot "..\seekfs.exe"),
  [string[]]$DB,
  [string]$QueryFile = (Join-Path $PSScriptRoot "queries-lowmem.txt"),
  [string]$FirstQuery = "ext:nrrd",
  [string]$WorkRoot = (Join-Path ([IO.Path]::GetTempPath()) "seekfs-r5-gates"),
  [int]$LoadTimeoutSeconds = 300,
  [int]$PersistTimeoutSeconds = 900,
  [int]$Iterations = 100,
  [int]$Warmup = 36,
  [double]$MaxColdFirstMs = 2000,
  [double]$MaxP95Ms = 100,
  [double]$MaxP99Ms = 200,
  [double]$MaxCountP95Ms = 100,
  [double]$MaxCountP99Ms = 200,
  [long]$MinEntries = 1000000,
  [switch]$RequireHardLinks,
	[switch]$SkipCold,
  [switch]$SkipPostPersist,
  [switch]$KeepRunDirectory
)

$ErrorActionPreference = "Stop"
$exe = (Resolve-Path -LiteralPath $Seekfs).Path
$queryPath = (Resolve-Path -LiteralPath $QueryFile).Path
$gateScript = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "r5_service_gate.ps1")).Path
$rootFull = [IO.Path]::GetFullPath($WorkRoot)
$runDir = Join-Path $rootFull ("run-" + [guid]::NewGuid().ToString("N"))

function Stop-OwnedProcess {
  param([Diagnostics.Process]$Process)
  if ($null -eq $Process) { return }
  try {
    if (-not $Process.HasExited) {
      $Process.Kill()
      $Process.WaitForExit(10000) | Out-Null
    }
  } catch {
    throw "failed to stop spawned seekfs pid $($Process.Id): $_"
  }
}

function Invoke-SeekfsJson {
  param([string[]]$ToolArgs)
  $json = & $exe @ToolArgs
  if ($LASTEXITCODE -ne 0) {
    throw "seekfs failed: $($ToolArgs -join ' ')"
  }
  return $json | ConvertFrom-Json
}

function Invoke-CompactClone {
  param([string]$Clone)
  $cloneFull = [IO.Path]::GetFullPath($Clone)
  $cloneDir = [IO.Path]::GetDirectoryName($cloneFull)
  $cloneLeaf = [IO.Path]::GetFileName($cloneFull)
  $ownedArtifacts = @()
  $oldV9 = $env:SEEKFS_ENGINE_V9
  $oldMem = $env:SEEKFS_MEMORY_MODE
  $env:SEEKFS_ENGINE_V9 = "1"
  # The v9 writer builds several large derived sections. Low-memory mode makes
  # trigram generation sequential and releases its temporary maps between
  # phases; the serving process remains independently configured below.
  $env:SEEKFS_MEMORY_MODE = "lowmem"
  try {
    $process = Start-Process -FilePath $exe -ArgumentList @("compact-index", "-db", $Clone) -PassThru -WindowStyle Hidden
  } finally {
    $env:SEEKFS_ENGINE_V9 = $oldV9
    $env:SEEKFS_MEMORY_MODE = $oldMem
  }
  $deadline = (Get-Date).AddSeconds($PersistTimeoutSeconds)
  $peak = 0L
  try {
    while (-not $process.HasExited -and (Get-Date) -lt $deadline) {
      $process.Refresh()
      $peak = [Math]::Max($peak, $process.PrivateMemorySize64)
      Start-Sleep -Milliseconds 250
    }
    if (-not $process.HasExited) {
      Stop-OwnedProcess $process
      throw "compact-index timed out after $PersistTimeoutSeconds seconds for clone $Clone"
    }
    if ($process.ExitCode -ne 0) { throw "compact-index failed for clone $Clone with exit $($process.ExitCode)" }
    $exitCode = $process.ExitCode
  } finally {
    if (-not $process.HasExited) { Stop-OwnedProcess $process }
    $prefix = $cloneLeaf + "."
    $partials = @(Get-ChildItem -LiteralPath $cloneDir -File -ErrorAction SilentlyContinue |
      Where-Object { $_.Name.StartsWith($prefix, [StringComparison]::Ordinal) -and $_.Extension -eq ".tmp" })
    foreach ($partial in $partials) {
      $partialFull = [IO.Path]::GetFullPath($partial.FullName)
      if (-not $partialFull.StartsWith($cloneDir + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        throw "refusing to remove unverified compaction temp: $partialFull"
      }
      $ownedArtifacts += [pscustomobject]@{
        path = $partialFull
        bytes = $partial.Length
        last_write_utc = $partial.LastWriteTimeUtc
      }
    }
    foreach ($partial in $partials) {
      Remove-Item -LiteralPath $partial.FullName -Force -ErrorAction Stop
    }
    foreach ($partial in $partials) {
      if (Test-Path -LiteralPath $partial.FullName) {
        throw "owned compaction temp survived cleanup: $($partial.FullName)"
      }
    }
  }
  return [pscustomobject]@{ peak_private_bytes = $peak; exit_code = $exitCode; temp_artifacts = $ownedArtifacts }
}

function Start-GateService {
  param([string[]]$CloneDBs, [string]$Phase)
  $pipe = "\\.\pipe\seekfs-r5-$Phase-$([guid]::NewGuid().ToString("N"))"
  $processArgs = @("service", "-lowmem", "-pipe", $pipe)
  foreach ($path in $CloneDBs) { $processArgs += @("-db", $path) }

  $oldV9 = $env:SEEKFS_ENGINE_V9
  $oldMem = $env:SEEKFS_MEMORY_MODE
  $oldPersist = $env:SEEKFS_DISABLE_BACKGROUND_PERSIST
  $env:SEEKFS_ENGINE_V9 = "1"
  $env:SEEKFS_MEMORY_MODE = "lowmem"
  $env:SEEKFS_DISABLE_BACKGROUND_PERSIST = "1"
  $clock = [Diagnostics.Stopwatch]::StartNew()
  try {
    $proc = Start-Process -FilePath $exe -ArgumentList ($processArgs -join " ") -PassThru -WindowStyle Hidden
  } finally {
    $env:SEEKFS_ENGINE_V9 = $oldV9
    $env:SEEKFS_MEMORY_MODE = $oldMem
    $env:SEEKFS_DISABLE_BACKGROUND_PERSIST = $oldPersist
  }

  $deadline = (Get-Date).AddSeconds($LoadTimeoutSeconds)
  try {
    do {
      if ($proc.HasExited) { throw "spawned seekfs exited with code $($proc.ExitCode)" }
      try {
        $loaded = Invoke-SeekfsJson @("loaded", "-pipe", $pipe, "--json")
        if ($loaded.ok -and -not $loaded.loading -and $loaded.entries -ge $MinEntries) { break }
      } catch {
      }
      Start-Sleep -Milliseconds 100
    } while ((Get-Date) -lt $deadline)
    if ($null -eq $loaded -or -not $loaded.ok -or $loaded.loading -or $loaded.entries -lt $MinEntries) {
      throw "alternate-pipe service was not ready after $LoadTimeoutSeconds seconds"
    }
    $readyMs = $clock.Elapsed.TotalMilliseconds

    $firstClock = [Diagnostics.Stopwatch]::StartNew()
    $first = Invoke-SeekfsJson @("search", "-service", "-pipe", $pipe, "--json", "-n", "20", $FirstQuery)
    $firstClock.Stop()
    if (-not $first.ok) { throw "first query failed: $($first.message)" }
    $coldTotalMs = $clock.Elapsed.TotalMilliseconds

    $proc.Refresh()
    $privateReady = $proc.PrivateMemorySize64
    $gateArgs = @{
      Seekfs = $exe; Pipe = $pipe; QueryFile = $queryPath
      Iterations = $Iterations; Warmup = $Warmup; MinEntries = $MinEntries
      MaxP95Ms = $MaxP95Ms; MaxP99Ms = $MaxP99Ms
      MaxCountP95Ms = $MaxCountP95Ms; MaxCountP99Ms = $MaxCountP99Ms
    }
    $gate = (& $gateScript @gateArgs) | ConvertFrom-Json
    $proc.Refresh()
    return [pscustomobject]@{
      phase = $Phase
      pipe = $pipe
      pid = $proc.Id
      entries = $loaded.entries
      ready_ms = [Math]::Round($readyMs, 3)
      first_query_ms = [Math]::Round($firstClock.Elapsed.TotalMilliseconds, 3)
      cold_first_total_ms = [Math]::Round($coldTotalMs, 3)
      private_bytes_ready = $privateReady
      private_bytes_after_gate = $proc.PrivateMemorySize64
      search_p95_ms = $gate.search_p95_ms
      search_p99_ms = $gate.search_p99_ms
      count_p95_ms = $gate.count_p95_ms
      count_p99_ms = $gate.count_p99_ms
      dbs = @($loaded.dbs | ForEach-Object { [pscustomobject]@{
        path = $_.path; dirty = $_.dirty; last_persist = $_.last_persist
        name_order_state = $_.name_order_state; name_trigram_state = $_.name_trigram_state
        derived_sections = $_.derived_sections
      } })
    }
  } finally {
    Stop-OwnedProcess $proc
  }
}

if (-not $DB -or $DB.Count -eq 0) {
  $localList = Join-Path $PSScriptRoot "real-indexes.local.txt"
  if (-not (Test-Path -LiteralPath $localList)) {
    throw "pass -DB or create bench\real-indexes.local.txt"
  }
  $DB = @(Get-Content -LiteralPath $localList | ForEach-Object { $_.Trim() } | Where-Object { $_ -and -not $_.StartsWith("#") })
}

$clones = @()
$results = @()
$persistMs = 0
$persistPrivatePeak = 0L
$persistTempArtifacts = @()
New-Item -ItemType Directory -Force -Path $runDir | Out-Null
try {
  for ($i = 0; $i -lt $DB.Count; $i++) {
    $source = (Resolve-Path -LiteralPath $DB[$i]).Path
    if ([IO.Path]::GetExtension($source) -ne ".gsi") { throw "index is not a .gsi file: $source" }
    $clone = Join-Path $runDir ("index-{0}.gsi" -f $i)
    # Compaction rewrites the clone. NTFS hard links share the same file data,
    # so never hard-link when persistence will run; doing so would mutate the
    # installed index. Hard links are only safe for the read-only skip path.
    if (-not $SkipPostPersist -and $RequireHardLinks) {
      throw "-RequireHardLinks is unsafe when post-persist compaction is enabled"
    }
    if ($SkipPostPersist) {
      try {
        New-Item -ItemType HardLink -Path $clone -Target $source | Out-Null
      } catch {
        if ($RequireHardLinks) {
          throw "cannot safely hard-link $source into $runDir (source and temp root must share a volume): $_"
        }
        Copy-Item -LiteralPath $source -Destination $clone
      }
    } else {
      Copy-Item -LiteralPath $source -Destination $clone
    }
    $clones += $clone
  }

	if (-not $SkipCold) {
		$results += Start-GateService $clones "cold"
	}
  if (-not $SkipPostPersist) {
    $persistClock = [Diagnostics.Stopwatch]::StartNew()
    foreach ($clone in $clones) {
      $persist = Invoke-CompactClone $clone
      $persistPrivatePeak = [Math]::Max($persistPrivatePeak, $persist.peak_private_bytes)
      $persistTempArtifacts += @($persist.temp_artifacts)
    }
    $persistClock.Stop()
    $persistMs = $persistClock.Elapsed.TotalMilliseconds
    $results += Start-GateService $clones "post-persist"
  }

  $report = [pscustomobject]@{
    executable = $exe
    work_root = $rootFull
    run_directory = $runDir
    persist_ms = [Math]::Round($persistMs, 3)
    persist_private_bytes_peak = $persistPrivatePeak
    persist_temp_artifacts_cleaned = $persistTempArtifacts
    phases = $results
  }
  $report | ConvertTo-Json -Depth 8
  foreach ($phase in $results) {
    if ($phase.cold_first_total_ms -gt $MaxColdFirstMs) {
      throw "$($phase.phase) readiness plus first query was $($phase.cold_first_total_ms)ms; budget is $MaxColdFirstMs ms"
    }
  }
} finally {
  $runFull = [IO.Path]::GetFullPath($runDir)
  if (-not $runFull.StartsWith($rootFull + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase) -or
      -not ([IO.Path]::GetFileName($runFull)).StartsWith("run-", [StringComparison]::Ordinal)) {
    throw "refusing to remove unverified run directory: $runFull"
  }
  if (-not $KeepRunDirectory -and (Test-Path -LiteralPath $runFull)) {
    Remove-Item -LiteralPath $runFull -Recurse -Force -ErrorAction SilentlyContinue
  }
}
