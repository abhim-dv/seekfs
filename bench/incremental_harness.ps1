param(
  [string]$Seekfs = ".\seekfs.exe",
  [string]$Es = "",
  [string]$Root = "F:\seekfs_bench_sandbox\incremental",
  [string]$OutDir = "F:\seekfs_bench_results\incremental",
  [int]$Iterations = 25,
  [int]$TimeoutSeconds = 30
)

$ErrorActionPreference = "Stop"

function New-Stats {
  param([double[]]$Values)
  if ($Values.Count -eq 0) {
    return @{ count = 0 }
  }
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
  param([string]$Exe, [string[]]$Args)
  $out = & $Exe @Args
  if ($LASTEXITCODE -ne 0) {
    throw "$Exe failed: $($Args -join ' ')"
  }
  return $out | ConvertFrom-Json
}

function Wait-SeekfsCount {
  param([string]$Query, [int]$Expected)
  $sw = [Diagnostics.Stopwatch]::StartNew()
  do {
    $result = Invoke-Json $Seekfs @("count", "-service", "--json", "-path", $Query)
    if ($result.count -eq $Expected) {
      $sw.Stop()
      return $sw.Elapsed.TotalMilliseconds
    }
    Start-Sleep -Milliseconds 100
  } while ($sw.Elapsed.TotalSeconds -lt $TimeoutSeconds)
  throw "timeout waiting for seekfs count $Expected for '$Query'"
}

function Wait-EsCount {
  param([string]$Query, [int]$Expected)
  if ($Es -eq "") {
    return $null
  }
  $sw = [Diagnostics.Stopwatch]::StartNew()
  do {
    $out = & $Es -get-result-count $Query
    if ($LASTEXITCODE -eq 0 -and [int]$out -eq $Expected) {
      $sw.Stop()
      return $sw.Elapsed.TotalMilliseconds
    }
    Start-Sleep -Milliseconds 100
  } while ($sw.Elapsed.TotalSeconds -lt $TimeoutSeconds)
  throw "timeout waiting for Everything count $Expected for '$Query'"
}

New-Item -ItemType Directory -Force -Path $Root | Out-Null
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$seekfsCreate = New-Object System.Collections.Generic.List[double]
$seekfsDelete = New-Object System.Collections.Generic.List[double]
$everythingCreate = New-Object System.Collections.Generic.List[double]
$everythingDelete = New-Object System.Collections.Generic.List[double]
$failures = 0

for ($i = 0; $i -lt $Iterations; $i++) {
  $needle = "seekfs_inc_" + [guid]::NewGuid().ToString("N")
  $path = Join-Path $Root "$needle.txt"
  try {
    Set-Content -LiteralPath $path -Value $needle
    $seekfsCreate.Add((Wait-SeekfsCount -Query $needle -Expected 1))
    $esCreate = Wait-EsCount -Query $needle -Expected 1
    if ($null -ne $esCreate) { $everythingCreate.Add($esCreate) }

    Remove-Item -LiteralPath $path -Force
    $seekfsDelete.Add((Wait-SeekfsCount -Query $needle -Expected 0))
    $esDelete = Wait-EsCount -Query $needle -Expected 0
    if ($null -ne $esDelete) { $everythingDelete.Add($esDelete) }
  }
  catch {
    $failures++
    Write-Warning $_.Exception.Message
    Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
  }
}

$summary = [ordered]@{
  ok = ($failures -eq 0)
  iterations = $Iterations
  failures = $failures
  root = $Root
  seekfs = @{
    create_visible_ms = New-Stats $seekfsCreate.ToArray()
    delete_hidden_ms = New-Stats $seekfsDelete.ToArray()
  }
  everything = @{
    enabled = ($Es -ne "")
    create_visible_ms = New-Stats $everythingCreate.ToArray()
    delete_hidden_ms = New-Stats $everythingDelete.ToArray()
  }
}

$json = $summary | ConvertTo-Json -Depth 8
$out = Join-Path $OutDir ("incremental-" + (Get-Date -Format "yyyyMMdd-HHmmss") + ".json")
$json | Set-Content -LiteralPath $out
$json
Write-Host "Wrote $out" -ForegroundColor Green
