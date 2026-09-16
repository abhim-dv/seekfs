param(
  [string]$Exe = ".\seekfs.exe",
  [int]$TimeoutSeconds = 10,
  [switch]$ServiceLive,
  [string]$ScratchRoot = ""
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

function Wait-ServiceSearchCount {
  param(
    [string]$Query,
    [int]$Expected
  )
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  do {
    $result = Invoke-SeekfsJson -ToolArgs @("count", "-path", "--json", $Query)
    if ($result.count -eq $Expected) {
      return
    }
    Start-Sleep -Milliseconds 200
  } while ((Get-Date) -lt $deadline)
  throw "expected service count $Expected for '$Query' within $TimeoutSeconds seconds"
}

function Assert-Admin {
  $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
  $principal = [Security.Principal.WindowsPrincipal]::new($identity)
  if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "-ServiceLive requires an elevated PowerShell session"
  }
}

if ($ServiceLive) {
  Assert-Admin
  $status = Invoke-SeekfsJson -ToolArgs @("loaded", "--json")
  if ($status.loading) {
    throw "seekfs service is still loading indexes"
  }
  $readyVolumes = @($status.dbs | Where-Object { $_.state -eq "ready" -and $_.source -eq "usn" } | ForEach-Object { $_.volume })
  if ($readyVolumes.Count -eq 0) {
    throw "seekfs service has no ready USN-backed volumes"
  }
  if ($ScratchRoot -eq "") {
    $volume = $readyVolumes[0]
    $ScratchRoot = Join-Path ($volume + "\") ("seekfs-live-incremental-" + [guid]::NewGuid().ToString("N"))
  } else {
    $scratchVolume = [System.IO.Path]::GetPathRoot((Resolve-Path -LiteralPath (Split-Path -Parent $ScratchRoot)).Path).TrimEnd("\")
    if ($readyVolumes -notcontains $scratchVolume) {
      throw "scratch root volume is not loaded by seekfs service"
    }
  }

  $token = "seekfs_live_" + [guid]::NewGuid().ToString("N")
  $src = Join-Path $ScratchRoot "src"
  $dst = Join-Path $ScratchRoot "dst"
  New-Item -ItemType Directory -Path $src -Force | Out-Null
  New-Item -ItemType Directory -Path $dst -Force | Out-Null
  try {
    Wait-ServiceSearchCount -Query $token -Expected 0
    $created = Join-Path $src "$token-created.txt"
    Set-Content -LiteralPath $created -Value "created"
    Wait-ServiceSearchCount -Query $token -Expected 1

    $renamed = Join-Path $src "$token-renamed.txt"
    Rename-Item -LiteralPath $created -NewName (Split-Path -Leaf $renamed)
    Wait-ServiceSearchCount -Query "$token-created" -Expected 0
    Wait-ServiceSearchCount -Query "$token-renamed" -Expected 1

    $moved = Join-Path $dst "$token-renamed.txt"
    Move-Item -LiteralPath $renamed -Destination $moved
    Wait-ServiceSearchCount -Query "dir:dst $token-renamed" -Expected 1

    Remove-Item -LiteralPath $moved
    Wait-ServiceSearchCount -Query $token -Expected 0
    Write-Host "seekfs live service incremental test passed"
  }
  finally {
    Remove-Item -LiteralPath $ScratchRoot -Recurse -Force -ErrorAction SilentlyContinue
  }
  exit 0
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
