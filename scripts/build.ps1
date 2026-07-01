param(
    [string]$Version = "dev",
    [string]$OutDir = "dist"
)

$ErrorActionPreference = "Stop"

$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

$Commit = "unknown"
try {
    $Commit = (git rev-parse --short HEAD 2>$null).Trim()
} catch {
}

$Date = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$Target = Join-Path $OutDir "seekfs-windows-amd64"

if (Test-Path $Target) {
    Remove-Item $Target -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $Target | Out-Null

$LdFlags = "-s -w -X main.version=$Version -X main.commit=$Commit -X main.date=$Date"
go build -trimpath -ldflags $LdFlags -o (Join-Path $Target "seekfs-service.exe") ./cmd/seekfs
go build -trimpath -tags "seekfs_ui production" -ldflags $LdFlags -o (Join-Path $Target "seekfs.exe") ./cmd/seekfs

Copy-Item README.md,LICENSE,NOTICE.md -Destination $Target

# Copy only files tracked by git so local research notes, private benchmark
# data, and other untracked docs cannot leak into release artifacts.
$DocFiles = git ls-files docs
foreach ($doc in $DocFiles) {
    $dest = Join-Path $Target $doc
    $destDir = Split-Path -Parent $dest
    New-Item -ItemType Directory -Force -Path $destDir | Out-Null
    Copy-Item $doc -Destination $dest
}

$Zip = Join-Path $OutDir "seekfs-windows-amd64.zip"
if (Test-Path $Zip) {
    Remove-Item $Zip -Force
}
Compress-Archive -Path (Join-Path $Target "*") -DestinationPath $Zip

Write-Host "Built $Zip"
