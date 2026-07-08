param(
  [string]$Exe = ".\seekfs.exe",
  [string]$Pipe = "\\.\pipe\seekfs-service",
  [string[]]$Db = @(),
  [string]$QueryFile = "",
  [int]$Limit = 20,
  [int]$MaxServiceMs = 2000,
  [int]$ReadyTimeoutSec = 0,
  [switch]$FullCountParity,
  [switch]$StrictFirstPage,
  [string]$ExpectedServiceExe = ""
)

$ErrorActionPreference = "Stop"

# Default queries are the Phase 7 live service/direct acceptance matrix. For
# broader matrices such as queries-regression-user.txt, pass -ReadyTimeoutSec so
# broad/no-hit queries are measured after background trigram indexes are ready.

function Resolve-SeekfsExe {
  param([string]$Path)
  if (Test-Path -LiteralPath $Path) {
    return (Resolve-Path -LiteralPath $Path).Path
  }
  return $Path
}

function Resolve-ExpectedServiceExe {
  param([string]$Path)
  if ($Path -ne "") {
    return Resolve-SeekfsExe $Path
  }
  $repoRoot = Split-Path -Parent $PSScriptRoot
  return Resolve-SeekfsExe (Join-Path $repoRoot "seekfs.exe")
}

function Invoke-SeekfsJson {
  param([string[]]$CmdArgs)
  $output = & $script:Seekfs @CmdArgs 2>&1
  if ($LASTEXITCODE -ne 0) {
    throw "seekfs $($CmdArgs -join ' ') failed with exit $LASTEXITCODE`: $($output -join [Environment]::NewLine)"
  }
  try {
    return ($output | Out-String | ConvertFrom-Json)
  } catch {
    throw "seekfs $($CmdArgs -join ' ') did not emit valid JSON: $($output -join [Environment]::NewLine)"
  }
}

function Measure-SeekfsJson {
  param([string[]]$CmdArgs)
  $sw = [Diagnostics.Stopwatch]::StartNew()
  $json = Invoke-SeekfsJson -CmdArgs $CmdArgs
  $sw.Stop()
  [pscustomobject]@{
    Json = $json
    ElapsedMs = [math]::Round($sw.Elapsed.TotalMilliseconds, 1)
  }
}

function Measure-SeekfsJsonWithMemoryMode {
  param([string[]]$CmdArgs, [string]$MemoryMode)
  if ($MemoryMode -eq "") {
    return Measure-SeekfsJson -CmdArgs $CmdArgs
  }
  $oldMode = $env:SEEKFS_MEMORY_MODE
  $hadOldMode = Test-Path Env:\SEEKFS_MEMORY_MODE
  try {
    $env:SEEKFS_MEMORY_MODE = $MemoryMode
    return Measure-SeekfsJson -CmdArgs $CmdArgs
  } finally {
    if ($hadOldMode) {
      $env:SEEKFS_MEMORY_MODE = $oldMode
    } else {
      Remove-Item Env:\SEEKFS_MEMORY_MODE -ErrorAction SilentlyContinue
    }
  }
}

function Add-DbArgs {
  param([string[]]$CmdArgs, [string[]]$Dbs)
  $out = @($CmdArgs)
  foreach ($dbPath in $Dbs) {
    $out += @("-db", $dbPath)
  }
  return $out
}

function Normalize-DbArgs {
  param([string[]]$Dbs)
  $out = @()
  foreach ($dbPath in $Dbs) {
    foreach ($part in "$dbPath".Split(",", [System.StringSplitOptions]::RemoveEmptyEntries)) {
      $trimmed = $part.Trim()
      if ($trimmed -ne "") {
        $out += $trimmed
      }
    }
  }
  return $out
}

function IntValue {
  param($Value)
  if ($null -eq $Value -or "$Value" -eq "") {
    return 0
  }
  try {
    return [int64]$Value
  } catch {
    return 0
  }
}

function Test-ServiceActiveOverlay {
  param($Info)
  foreach ($dbInfo in @($Info.dbs)) {
    if ($dbInfo.dirty -eq $true) {
      return $true
    }
    if ((IntValue $dbInfo.recent) -gt 0) {
      return $true
    }
    if ((IntValue $dbInfo.frn_overlay_entries) -gt 0) {
      return $true
    }
  }
  return $false
}

function ResultPaths {
  param($Json)
  if ($null -eq $Json.results) {
    return @()
  }
  return @($Json.results | ForEach-Object { $_.path } | Where-Object { $_ })
}

function SamePathSet {
  param([string[]]$A, [string[]]$B)
  $left = @($A | Sort-Object)
  $right = @($B | Sort-Object)
  if ($left.Count -ne $right.Count) {
    return $false
  }
  for ($i = 0; $i -lt $left.Count; $i++) {
    if ($left[$i] -ne $right[$i]) {
      return $false
    }
  }
  return $true
}

function SameOrderedPaths {
  param([string[]]$A, [string[]]$B)
  if ($A.Count -ne $B.Count) {
    return $false
  }
  for ($i = 0; $i -lt $A.Count; $i++) {
    if ($A[$i] -ne $B[$i]) {
      return $false
    }
  }
  return $true
}

function FirstPathsText {
  param([string[]]$Paths)
  $first = @($Paths | Select-Object -First 3)
  if ($first.Count -eq 0) {
    return ""
  }
  return ($first -join " | ")
}

function ServiceIdentityText {
  param($Info)
  $parts = @()
  if ($Info.executable) {
    $parts += "exe=$($Info.executable)"
  }
  if ($Info.executable_hash) {
    $parts += "hash=$($Info.executable_hash)"
  }
  if ($Info.version) {
    $parts += "version=$($Info.version)"
  }
  if ($Info.commit) {
    $parts += "commit=$($Info.commit)"
  }
  if ($Info.date) {
    $parts += "date=$($Info.date)"
  }
  if ($Info.build_flavor) {
    $parts += "flavor=$($Info.build_flavor)"
  }
  if ($Info.process_mode) {
    $parts += "mode=$($Info.process_mode)"
  }
  return ($parts -join " ")
}

function Assert-ServiceIdentity {
  param($Info, [string]$ExpectedExe, [System.Collections.Generic.List[string]]$Failures)
  if (-not (Test-Path -LiteralPath $ExpectedExe)) {
    $Failures.Add("expected service executable does not exist: $ExpectedExe")
    return
  }
  if (-not $Info.executable) {
    $Failures.Add("service did not report executable identity")
    return
  }
  $actualExe = Resolve-SeekfsExe $Info.executable
  $expectedResolved = Resolve-SeekfsExe $ExpectedExe
  if ($actualExe -and $expectedResolved -and ([IO.Path]::GetFullPath($actualExe) -ne [IO.Path]::GetFullPath($expectedResolved))) {
    $Failures.Add("service executable mismatch: service=$actualExe expected=$expectedResolved")
  }
  if (-not $Info.executable_hash) {
    $Failures.Add("service did not report executable_hash identity")
    return
  }
  $expectedHash = (Get-FileHash -LiteralPath $expectedResolved -Algorithm SHA256).Hash.ToLowerInvariant()
  $actualHash = "$($Info.executable_hash)".ToLowerInvariant()
  if ($actualHash -ne $expectedHash) {
    $Failures.Add("service executable hash mismatch: service=$actualHash expected=$expectedHash")
  }
}

$script:Seekfs = Resolve-SeekfsExe $Exe
$expectedServiceExePath = Resolve-ExpectedServiceExe $ExpectedServiceExe

function Assert-ServiceReady {
  param([int]$TimeoutSec)
  if ($TimeoutSec -le 0) {
    return
  }
  $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSec)
  do {
    $info = Invoke-SeekfsJson -CmdArgs @("loaded", "-pipe", $Pipe, "--json")
    $states = @($info.dbs | ForEach-Object { $_.name_trigram_state } | Where-Object { $_ })
    $pending = @($states | Where-Object { $_ -eq "pending" -or $_ -eq "building" })
    if ($info.ok -and -not $info.loading -and $pending.Count -eq 0) {
      return
    }
    Start-Sleep -Milliseconds 500
  } while ([DateTime]::UtcNow -lt $deadline)
  throw "service did not become fully ready within $TimeoutSec seconds"
}

Assert-ServiceReady -TimeoutSec $ReadyTimeoutSec

$loaded = Invoke-SeekfsJson -CmdArgs @("loaded", "-pipe", $Pipe, "--json")
if (-not $loaded.ok) {
  throw "resident service is not loaded: $($loaded.message)"
}

$Db = @(Normalize-DbArgs -Dbs $Db)
if ($Db.Count -eq 0) {
  $Db = @($loaded.dbs | ForEach-Object { $_.path } | Where-Object { $_ })
  if ($Db.Count -eq 0) {
    throw "resident service did not report DB paths"
  }
}
$Db = @(Normalize-DbArgs -Dbs $Db)

$usingDefaultQueries = $false
if ($QueryFile -ne "") {
  $queries = @(Get-Content -LiteralPath $QueryFile | ForEach-Object { $_.Trim() } | Where-Object { $_ -and -not $_.StartsWith("#") })
} else {
  $usingDefaultQueries = $true
  $queries = @(
    "Downloads md",
    "Downloads nrrd",
    "Downloads .nrrd",
    "path:Downloads md",
    "path:Downloads .nrrd",
    "path:F:\fixtureproj trainingdata",
    "path:F: fixtureproj trainingdata",
    "Downloads fixturemetrics",
    "path:Downloads fixturemetrics",
    "ext:nrrd path:Downloads",
    "dir:Downloads nrrd"
  )
}

if ($queries.Count -eq 0) {
  throw "no acceptance queries configured"
}

$failures = New-Object System.Collections.Generic.List[string]
$rows = New-Object System.Collections.Generic.List[object]
$serviceIdentity = ServiceIdentityText -Info $loaded
Assert-ServiceIdentity -Info $loaded -ExpectedExe $expectedServiceExePath -Failures $failures
$activeOverlay = Test-ServiceActiveOverlay -Info $loaded
$directMemoryMode = ""
if ("$($loaded.build_flavor)" -match "(^|,)lowmem(,|$)") {
  $directMemoryMode = "lowmem"
}
Write-Host "service identity: $serviceIdentity"
if ($activeOverlay) {
  Write-Host "active overlay/recent state detected: direct base-index parity is advisory"
}
if ($directMemoryMode -ne "") {
  Write-Host "direct local memory mode: $directMemoryMode"
}

$coldQuery = $queries[0]
$coldArgs = @("search", "-service", "-pipe", $Pipe, "-path", "--json", "-n", "$Limit", $coldQuery)
try {
  $coldSearch = Measure-SeekfsJson -CmdArgs $coldArgs
  $coldPaths = @(ResultPaths -Json $coldSearch.Json)
  if ($coldPaths.Count -gt $Limit) {
    $failures.Add("cold service query '$coldQuery' returned $($coldPaths.Count) results, max $Limit")
  }
  Write-Host "cold check: query='$coldQuery' service_ms=$($coldSearch.ElapsedMs) count=$($coldSearch.Json.count) source=$($coldSearch.Json.source)"
} catch {
  $failures.Add("cold service query '$coldQuery' failed or timed out: $($_.Exception.Message)")
}

foreach ($query in $queries) {
  Write-Host "checking: $query"
  try {
    $serviceSearchArgs = @("search", "-service", "-pipe", $Pipe, "-path", "--json", "-n", "$Limit", $query)
    $directSearchArgs = Add-DbArgs -CmdArgs @("search", "-local", "-path", "--json", "-n", "$Limit") -Dbs $Db
    $directSearchArgs += $query

    $serviceSearch = Measure-SeekfsJson -CmdArgs $serviceSearchArgs
    $directSearch = Measure-SeekfsJsonWithMemoryMode -CmdArgs $directSearchArgs -MemoryMode $directMemoryMode
    $serviceTotal = $null
    $directTotal = $null
    if ($FullCountParity) {
      $serviceCountArgs = @("count", "-service", "-pipe", $Pipe, "-path", "--json", $query)
      $directCountArgs = Add-DbArgs -CmdArgs @("count", "-local", "-path", "--json") -Dbs $Db
      $directCountArgs += $query
      $serviceCount = Measure-SeekfsJson -CmdArgs $serviceCountArgs
      $directCount = Measure-SeekfsJsonWithMemoryMode -CmdArgs $directCountArgs -MemoryMode $directMemoryMode
      $serviceTotal = $serviceCount.Json.count
      $directTotal = $directCount.Json.count
    }

    $servicePaths = @(ResultPaths -Json $serviceSearch.Json)
    $directPaths = @(ResultPaths -Json $directSearch.Json)
    $expectNonZero = $usingDefaultQueries -or ($query -notmatch "^zzzz-no-hit" -and $directSearch.Json.count -gt 0)
    $serviceFirstPaths = FirstPathsText -Paths $servicePaths
    $directFirstPaths = FirstPathsText -Paths $directPaths

    if ($FullCountParity -and $serviceTotal -ne $directTotal -and -not $activeOverlay) {
      $failures.Add("count mismatch for '$query': service=$serviceTotal direct=$directTotal")
    }
    if ($expectNonZero -and $serviceSearch.Json.count -eq 0) {
      $failures.Add("known non-empty query '$query' returned zero service results")
    }
    if ($usingDefaultQueries -and $directSearch.Json.count -eq 0) {
      $failures.Add("known non-empty query '$query' returned zero direct local results")
    }
    $orderedPathsMatch = SameOrderedPaths -A $servicePaths -B $directPaths
    $pathSetsMatch = SamePathSet -A $servicePaths -B $directPaths
    $allowLowmemRankOrderDrift = ($directMemoryMode -eq "lowmem" -and "$($serviceSearch.Json.source)" -eq "path-component-trigram" -and $pathSetsMatch)
    $skipStrictParity = ($usingDefaultQueries -and ($query -eq "Downloads fixturemetrics" -or $query -eq "path:Downloads fixturemetrics"))
    $parityAdvisory = $activeOverlay
    $enforceParity = (-not $skipStrictParity -and -not $parityAdvisory)
    if (-not $orderedPathsMatch -and -not $allowLowmemRankOrderDrift -and $enforceParity) {
      $failures.Add("ordered top-$Limit mismatch for '$query': service='$serviceFirstPaths' direct='$directFirstPaths'")
    }
    if (-not $pathSetsMatch -and $enforceParity) {
      $failures.Add("top-$Limit path set mismatch for '$query': service='$serviceFirstPaths' direct='$directFirstPaths'")
    }
    if ($serviceSearch.ElapsedMs -gt $MaxServiceMs) {
      $failures.Add("warm service query '$query' took $($serviceSearch.ElapsedMs) ms, max $MaxServiceMs ms")
    }
    if ($StrictFirstPage -and -not $parityAdvisory -and $servicePaths.Count -gt 0 -and $directPaths.Count -gt 0 -and -not (SamePathSet -A $servicePaths -B $directPaths)) {
      $failures.Add("first page path set mismatch for '$query'")
    }

    Write-Host "result: service_ms=$($serviceSearch.ElapsedMs) direct_ms=$($directSearch.ElapsedMs) service_count=$($serviceSearch.Json.count) direct_count=$($directSearch.Json.count) source=$($serviceSearch.Json.source) first='$serviceFirstPaths' identity='$serviceIdentity'"

    $rows.Add([pscustomobject]@{
      query = $query
      service_total = $serviceTotal
      direct_total = $directTotal
      service_search_count = $serviceSearch.Json.count
      direct_search_count = $directSearch.Json.count
      service_ms = $serviceSearch.ElapsedMs
      direct_ms = $directSearch.ElapsedMs
      source = $serviceSearch.Json.source
      candidates = $serviceSearch.Json.candidates
      direct_memory_mode = $directMemoryMode
      ordered_paths_match = $orderedPathsMatch
      path_sets_match = $pathSetsMatch
      strict_parity = $enforceParity
      active_overlay = $activeOverlay
      parity_advisory = $parityAdvisory
      service_first_paths = $serviceFirstPaths
      direct_first_paths = $directFirstPaths
      service_identity = $serviceIdentity
    })
  } catch {
    $failures.Add("query '$query' failed: $($_.Exception.Message)")
  }
}

$rows | Format-Table -AutoSize | Out-Host

if ($failures.Count -gt 0) {
  throw "service/direct acceptance failed:`n$($failures -join [Environment]::NewLine)"
}

Write-Host "service/direct acceptance passed"
