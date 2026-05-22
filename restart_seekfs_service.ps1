param(
    [string]$Pipe = "\\.\pipe\seekfs-service"
)

$ErrorActionPreference = "Stop"

$Here = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $Here

Write-Host "Stopping seekfs service..."
Stop-Service seekfs -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

Write-Host "Starting seekfs service..."
Start-Service seekfs
Start-Sleep -Seconds 1

Write-Host "Service status:"
.\seekfs.exe service-status -pipe $Pipe
