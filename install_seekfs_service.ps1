param(
    [string[]]$Db = @(),
    [string]$Pipe = "\\.\pipe\seekfs-service"
)

$ErrorActionPreference = "Stop"

$Here = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $Here

if (-not (Test-Path ".\seekfs.exe")) {
    Write-Host "Building seekfs.exe..."
    go build -o seekfs.exe ./cmd/seekfs
}

Write-Host "Stopping existing service if present..."
Stop-Service seekfs -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

Write-Host "Removing existing service if present..."
.\seekfs.exe uninstall-service 2>$null

$args = @("install-service", "-pipe", $Pipe)
foreach ($path in $Db) {
    $args += @("-db", $path)
}

Write-Host "Installing seekfs service..."
& .\seekfs.exe @args

Write-Host "Starting seekfs service..."
Start-Service seekfs

Write-Host "Service status:"
.\seekfs.exe service-status -pipe $Pipe
