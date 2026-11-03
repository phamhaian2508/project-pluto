param(
    [string]$Root = ".pluto-network",
    [int]$PortOffset = 0
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$target = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $Root))

Push-Location $repoRoot
try {
    Write-Host "Creating the Pluto network with four validators at $target"
    go run ./cmd/plutonetwork -root $target -port-offset $PortOffset
    if ($LASTEXITCODE -ne 0) {
        throw "Pluto validator network setup failed with exit code $LASTEXITCODE"
    }
    Write-Host "Setup complete. No existing Pluto home or database was modified."
    Write-Host "Next: .\scripts\start-pluto-network.ps1"
}
finally {
    Pop-Location
}
