$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$RepoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $RepoRoot

$targets = @(
    "bin",
    "dist",
    ".tmp-skills",
    ".tmp-tools"
)

foreach ($target in $targets) {
    if (-not (Test-Path -LiteralPath $target)) {
        continue
    }

    $resolved = Resolve-Path -LiteralPath $target
    if (-not $resolved.Path.StartsWith($RepoRoot + [IO.Path]::DirectorySeparatorChar)) {
        throw "Refusing to remove path outside repository: $($resolved.Path)"
    }

    Remove-Item -LiteralPath $resolved.Path -Recurse -Force
    Write-Host "removed $target"
}
