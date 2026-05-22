param(
  [string]$Exe = ".\seekfs.exe"
)

$ErrorActionPreference = "Stop"
$Root = Join-Path $PWD ("tmp-seekfs-test-" + [guid]::NewGuid().ToString("N"))
$Db = Join-Path $Root "test.gsi"

New-Item -ItemType Directory -Path $Root | Out-Null
New-Item -ItemType Directory -Path (Join-Path $Root "subdir") | Out-Null
Set-Content -LiteralPath (Join-Path $Root "alpha-needle.txt") -Value "one"
Set-Content -LiteralPath (Join-Path $Root "subdir\beta-needle.log") -Value "two"
Set-Content -LiteralPath (Join-Path $Root "subdir\main.go") -Value "package main"
Set-Content -LiteralPath (Join-Path $Root "subdir\script.py") -Value "print('ok')"
Set-Content -LiteralPath (Join-Path $Root "other.txt") -Value "three"

try {
  & $Exe index -db $Db -root $Root | Out-Host
  $name = & $Exe search -db $Db -n 10 needle
  if (($name | Measure-Object).Count -ne 2) {
    throw "expected 2 name results for needle, got $($name | Measure-Object | Select-Object -ExpandProperty Count)"
  }

  $path = & $Exe search -db $Db -path -n 10 subdir
  if (($path | Measure-Object).Count -lt 1) {
    throw "expected at least one path result for subdir"
  }

  $count = (& $Exe count -db $Db needle).Trim()
  if ($count -ne "2") {
    throw "expected count 2, got $count"
  }

  $go = & $Exe search -db $Db -path -n 10 "ext:go"
  if (($go | Measure-Object).Count -ne 1) {
    throw "expected one ext:go result"
  }

  $glob = & $Exe search -db $Db -path -n 10 "glob:*.py"
  if (($glob | Measure-Object).Count -ne 1) {
    throw "expected one glob:*.py result"
  }

  $typed = & $Exe search -db $Db -path -n 10 "type:dir subdir"
  if (($typed | Measure-Object).Count -lt 1) {
    throw "expected type:dir subdir result"
  }

  $under = & $Exe search -db $Db -path -n 10 --under (Join-Path $Root "subdir") needle
  if (($under | Measure-Object).Count -ne 1) {
    throw "expected one --under needle result"
  }

  $json = & $Exe search -db $Db -path --json -n 1 "ext:go" | ConvertFrom-Json
  if (-not $json.ok -or $json.results[0].index_source -ne "walk") {
    throw "json output missing expected index_source"
  }

  $info = & $Exe info -db $Db
  if (-not ($info -match "entries:")) {
    throw "info output missing entries"
  }

  Write-Host "seekfs CLI integration test passed"
}
finally {
  Remove-Item -LiteralPath $Root -Recurse -Force -ErrorAction SilentlyContinue
}
