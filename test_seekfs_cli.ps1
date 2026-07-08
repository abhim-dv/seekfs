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

  $implicitGlob = & $Exe search -db $Db -n 10 "*.py"
  if (($implicitGlob | Measure-Object).Count -ne 1) {
    throw "expected one implicit *.py glob result"
  }

  $implicitGlobCompat = & $Exe -db $Db -n 10 "*.py"
  if (($implicitGlobCompat | Measure-Object).Count -ne 1) {
    throw "expected one commandless implicit *.py glob result"
  }

  $typed = & $Exe search -db $Db -path -n 10 "type:dir subdir"
  if (($typed | Measure-Object).Count -lt 1) {
    throw "expected type:dir subdir result"
  }

  $under = & $Exe search -db $Db -path -n 10 --under (Join-Path $Root "subdir") needle
  if (($under | Measure-Object).Count -ne 1) {
    throw "expected one --under needle result"
  }

  $compat = & $Exe -db $Db --under (Join-Path $Root "subdir") needle
  if (($compat | Measure-Object).Count -ne 1) {
    throw "expected commandless search compatibility result"
  }

  $json = & $Exe search -db $Db -path --json -n 1 "ext:go" | ConvertFrom-Json
  if (-not $json.ok -or $json.results[0].index_source -ne "walk") {
    throw "json output missing expected index_source"
  }

  $info = & $Exe info -db $Db
  if (-not ($info -match "entries:")) {
    throw "info output missing entries"
  }

  $Pipe = "\\.\pipe\seekfs-test-$([guid]::NewGuid().ToString("N"))"
  $Service = $null
  try {
    $ResolvedExe = (Resolve-Path $Exe).Path
    $Service = Start-Process -FilePath $ResolvedExe -ArgumentList @("service", "-db", $Db, "-pipe", $Pipe, "-lowmem") -PassThru -WindowStyle Hidden
    $loaded = $false
    for ($i = 0; $i -lt 40; $i++) {
      try {
        $loadedInfo = & $Exe loaded -pipe $Pipe --json | ConvertFrom-Json
        if ($loadedInfo.ok -and $loadedInfo.entries -ge 5) {
          $loaded = $true
          break
        }
      } catch {
        Start-Sleep -Milliseconds 250
      }
    }
    if (-not $loaded) {
      throw "resident service did not report loaded test index"
    }

    $serviceNeedle = & $Exe search -service -pipe $Pipe -path -n 10 needle
    if (($serviceNeedle | Measure-Object).Count -ne 2) {
      throw "expected 2 service path results for needle"
    }

    $serviceCount = (& $Exe count -service -pipe $Pipe needle).Trim()
    if ($serviceCount -ne "2") {
      throw "expected service count 2, got $serviceCount"
    }

    $serviceUnder = & $Exe search -service -pipe $Pipe -path -n 10 --under (Join-Path $Root "subdir") needle
    if (($serviceUnder | Measure-Object).Count -ne 1) {
      throw "expected one service --under needle result"
    }

    $serviceGo = & $Exe search -service -pipe $Pipe -path --json -n 10 "ext:go" | ConvertFrom-Json
    if (-not $serviceGo.ok -or ($serviceGo.results | Measure-Object).Count -ne 1) {
      throw "expected one service ext:go json result"
    }

    $serviceGlob = & $Exe search -service -pipe $Pipe -path --json -n 10 "glob:*.py" | ConvertFrom-Json
    if (-not $serviceGlob.ok -or ($serviceGlob.results | Measure-Object).Count -ne 1) {
      throw "expected one service glob:*.py json result"
    }
  }
  finally {
    if ($null -ne $Service -and -not $Service.HasExited) {
      Stop-Process -Id $Service.Id -Force -ErrorAction SilentlyContinue
      $Service.WaitForExit(5000) | Out-Null
    }
  }

  Write-Host "seekfs CLI integration test passed"
}
finally {
  Remove-Item -LiteralPath $Root -Recurse -Force -ErrorAction SilentlyContinue
}
