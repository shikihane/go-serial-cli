$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$RepoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $RepoRoot

New-Item -ItemType Directory -Force -Path "bin" | Out-Null
go build -o "bin/sio.exe" ./cmd/sio
Write-Host "built bin/sio.exe"
go build -o "bin/gs.exe" ./cmd/gs
Write-Host "built bin/gs.exe"
