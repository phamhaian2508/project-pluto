param(
    [string]$Root = ".pluto-network",
    [string]$Node = ""
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$rootPath = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $Root))
$manifestPath = Join-Path $rootPath "processes.json"
if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
    Write-Host "No managed managed network node process file found; nothing to stop."
    return
}

$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
$remaining = @()
foreach ($entry in @($manifest.nodes)) {
    if ($Node -and $entry.name -ne $Node) {
        $remaining += $entry
        continue
    }
    $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $($entry.pid)" -ErrorAction SilentlyContinue
    $expectedHome = [System.IO.Path]::GetFullPath($entry.home)
    $expectedBinary = [System.IO.Path]::GetFullPath($entry.binary)
    if (-not $proc) {
        Write-Host "$($entry.name) is already stopped."
        continue
    }
    if ($proc.ExecutablePath -ne $expectedBinary -or -not $proc.CommandLine.Contains($expectedHome)) {
        Write-Warning "PID $($entry.pid) no longer matches the recorded managed network node; leaving it untouched."
        continue
    }
    Stop-Process -Id $entry.pid -Force
    Write-Host "Stopped managed node $($entry.name) (PID $($entry.pid)); its data directory was preserved."
}

if ($Node) {
    $remaining += @($manifest.nodes | Where-Object { $_.name -eq $Node -and (Get-CimInstance Win32_Process -Filter "ProcessId = $($_.pid)" -ErrorAction SilentlyContinue) })
}
[PSCustomObject]@{ nodes = @($remaining) } | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $manifestPath -Encoding utf8
