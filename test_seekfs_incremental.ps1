param(
  [string]$Exe = ".\seekfs.exe",
  [int]$TimeoutSeconds = 10
)

$ErrorActionPreference = "Stop"

function Invoke-SeekfsJson {
  param([string[]]$ToolArgs)
  $out = & $Exe @ToolArgs
  if ($LASTEXITCODE -ne 0) {
    throw "seekfs failed: $($ToolArgs -join ' ')"
  }
  return $out | ConvertFrom-Json
}

function Wait-SearchCount {
  param(
    [string]$Db,
    [string]$Query,
    [int]$Expected
  )
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  do {
    $result = Invoke-SeekfsJson -ToolArgs @("count", "-db", $Db, "-path", "--json", $Query)
    if ($result.count -eq $Expected) {
      return
    }
    Start-Sleep -Milliseconds 200
  } while ((Get-Date) -lt $deadline)
  throw "expected count $Expected for '$Query' within $TimeoutSeconds seconds"
}

$Root = Join-Path $PWD ("tmp-seekfs-incremental-" + [guid]::NewGuid().ToString("N"))
$Db = Join-Path $Root "incremental.gsi"

New-Item -ItemType Directory -Path $Root | Out-Null
New-Item -ItemType Directory -Path (Join-Path $Root "src") | Out-Null
Set-Content -LiteralPath (Join-Path $Root "src\initial-alpha.txt") -Value "alpha"

try {
  & $Exe index -db $Db -root $Root | Out-Host
  if ($LASTEXITCODE -ne 0) {
    throw "initial index failed"
  }

  Wait-SearchCount -Db $Db -Query "initial-alpha" -Expected 1

  Set-Content -LiteralPath (Join-Path $Root "src\created-beta.txt") -Value "beta"
  # Current release is static. Until automatic incremental updates are
  # implemented, this explicit reindex documents the expected end state.
  & $Exe index -db $Db -root $Root | Out-Null
  Wait-SearchCount -Db $Db -Query "created-beta" -Expected 1

  Rename-Item -LiteralPath (Join-Path $Root "src\created-beta.txt") -NewName "renamed-gamma.txt"
  & $Exe index -db $Db -root $Root | Out-Null
  Wait-SearchCount -Db $Db -Query "created-beta" -Expected 0
  Wait-SearchCount -Db $Db -Query "renamed-gamma" -Expected 1

  New-Item -ItemType Directory -Path (Join-Path $Root "dst") | Out-Null
  Move-Item -LiteralPath (Join-Path $Root "src\renamed-gamma.txt") -Destination (Join-Path $Root "dst\renamed-gamma.txt")
  & $Exe index -db $Db -root $Root | Out-Null
  Wait-SearchCount -Db $Db -Query "dir:dst renamed-gamma" -Expected 1

  Remove-Item -LiteralPath (Join-Path $Root "dst\renamed-gamma.txt")
  & $Exe index -db $Db -root $Root | Out-Null
  Wait-SearchCount -Db $Db -Query "renamed-gamma" -Expected 0

  Write-Host "seekfs incremental target-state test passed"
}
finally {
  Remove-Item -LiteralPath $Root -Recurse -Force -ErrorAction SilentlyContinue
}
