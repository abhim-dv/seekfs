param(
  [string]$Seekfs = ".\seekfs.exe",
  [string]$Everything = "",
  [string]$EverythingInstance = "1.5a",
  [string]$Repo = "",
  [string]$Pipe = "",
  [int]$Iterations = 5,
  [switch]$Json
)

$ErrorActionPreference = "Stop"

if ($Iterations -le 0) {
  throw "Iterations must be positive."
}

if (-not $Repo) {
  $Repo = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
} else {
  $Repo = (Resolve-Path $Repo).Path
}

if (-not $Everything) {
  $cmd = Get-Command es.exe -ErrorAction SilentlyContinue
  if ($cmd) {
    $Everything = $cmd.Source
  } else {
    $Everything = (& $Seekfs search "type:file glob:es.exe" | Select-Object -First 1)
  }
}
if (-not $Everything) {
  throw "Could not find es.exe. Pass -Everything <path>."
}

$seekPrefix = @()
if ($Pipe) {
  $seekPrefix += @("-pipe", $Pipe)
}
$esPrefix = @()
if ($EverythingInstance) {
  $esPrefix += @("-instance", $EverythingInstance)
}

$cases = @(
  @{
    name = "repo go files"
    seek = @("count") + $seekPrefix + @("--under", $Repo, "type:file ext:go")
    es = $esPrefix + @("-get-result-count", "-n", "0", "-path", $Repo, "/a-d", "ext:go")
  },
  @{
    name = "repo cmd go path"
    seek = @("count") + $seekPrefix + @("-path", "--under", $Repo, "dir:cmd ext:go")
    es = $esPrefix + @("-get-result-count", "-n", "0", "-path", $Repo, "-match-path", "cmd", "ext:go")
  },
  @{
    name = "repo complex test glob"
    seek = @("count") + $seekPrefix + @("-path", "--under", $Repo, "glob:*test*.go")
    es = $esPrefix + @("-get-result-count", "-n", "0", "-path", $Repo, "*test*.go")
  },
  @{
    name = "repo readme"
    seek = @("count") + $seekPrefix + @("--under", $Repo, "readme")
    es = $esPrefix + @("-get-result-count", "-n", "0", "-path", $Repo, "readme")
  }
)

function Median($values) {
  $sorted = @($values | Sort-Object)
  return $sorted[[int][math]::Floor($sorted.Count / 2)]
}

$rows = foreach ($case in $cases) {
  & $Seekfs @($case.seek) | Out-Null
  & $Everything @($case.es) | Out-Null

  $seekTimes = @()
  $everythingTimes = @()
  $seekCount = ""
  $everythingCount = ""
  for ($i = 0; $i -lt $Iterations; $i++) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $seekCount = (& $Seekfs @($case.seek) | Select-Object -First 1)
    $sw.Stop()
    $seekTimes += $sw.Elapsed.TotalMilliseconds

    $sw.Restart()
    $everythingCount = (& $Everything @($case.es) | Select-Object -First 1)
    $sw.Stop()
    $everythingTimes += $sw.Elapsed.TotalMilliseconds
  }

  [pscustomobject]@{
    case = $case.name
    seekfs_count = $seekCount
    seekfs_median_ms = [math]::Round((Median $seekTimes), 3)
    everything_count = $everythingCount
    everything_median_ms = [math]::Round((Median $everythingTimes), 3)
  }
}

if ($Json) {
  $rows | ConvertTo-Json -Depth 4
} else {
  $rows | Format-Table -AutoSize
}
