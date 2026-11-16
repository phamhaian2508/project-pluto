param(
    [int]$Transactions = 50,
    [int]$Samples = 5
)

$ErrorActionPreference = "Stop"
if ($Transactions -lt 1 -or $Transactions -gt 64) { throw "Transactions must be between 1 and 64 (the funded local test-account count)." }
if ($Samples -lt 3 -or $Samples -gt 20) { throw "Samples must be between 3 and 20; the first load is used as warm-up." }

$repoRoot = Split-Path -Parent $PSScriptRoot
$runId = Get-Date -Format "yyyyMMdd-HHmmss-fff"
$comparisonRoot = ".pluto-network\comparisons\$runId"
$sequentialRoot = "$comparisonRoot\sequential"
$parallelRoot = "$comparisonRoot\parallel"
$absoluteComparisonRoot = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $comparisonRoot))

function Test-PortAvailable {
    param([int]$Port)

    $listener = $null
    try {
        $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, $Port)
        $listener.Start()
        return $true
    }
    catch {
        return $false
    }
    finally {
        if ($listener) { $listener.Stop() }
    }
}

function Test-ComparisonPortOffsets {
    param([int]$SequentialOffset, [int]$ParallelOffset)

    # Keep these base ports aligned with internal/plutonetwork/setup.go.
    $portBases = @(27656, 27757, 18545) # P2P, CometBFT RPC, Ethereum RPC
    foreach ($offset in @($SequentialOffset, $ParallelOffset)) {
        foreach ($basePort in $portBases) {
            for ($node = 0; $node -lt 4; $node++) {
                if (-not (Test-PortAvailable ($basePort + $offset + $node))) { return $false }
            }
        }
    }
    return $true
}

# A small offset can overlap another service's range: e.g. P2P +100 is
# 27756-27759, which collides with the default network's CometBFT RPC ports.
$sequentialPortOffset = -1
$parallelPortOffset = -1
for ($candidate = 1000; $candidate -le 30000; $candidate += 20) {
    if (Test-ComparisonPortOffsets -SequentialOffset $candidate -ParallelOffset ($candidate + 20)) {
        $sequentialPortOffset = $candidate
        $parallelPortOffset = $candidate + 20
        break
    }
}
if ($sequentialPortOffset -lt 0) {
    throw "Could not find two free four-validator port ranges for the comparison. Stop other Pluto test networks or free their ports, then retry."
}
Write-Host "Selected free port offsets: sequential=$sequentialPortOffset; parallel=$parallelPortOffset"

Push-Location $repoRoot
try {
    .\scripts\setup-pluto-network.ps1 -Root $sequentialRoot -PortOffset $sequentialPortOffset
    .\scripts\setup-pluto-network.ps1 -Root $parallelRoot -PortOffset $parallelPortOffset

    Write-Host "Running $Transactions transfers on a fresh four-validator sequential network..."
    .\scripts\start-pluto-network.ps1 -Root $sequentialRoot -Executor sequential -Workers 1
    go run ./cmd/plutoload -root $sequentialRoot -count $Transactions
    if ($LASTEXITCODE -ne 0) { throw "Sequential mode warm-up failed with exit code $LASTEXITCODE" }
    $sequentialSamples = @()
    for ($sample = 1; $sample -le $Samples; $sample++) {
        $reportPath = Join-Path $absoluteComparisonRoot ("sequential-{0}.json" -f $sample)
        go run ./cmd/plutoload -root $sequentialRoot -count $Transactions -out $reportPath
        if ($LASTEXITCODE -ne 0) { throw "Sequential mode sample $sample failed with exit code $LASTEXITCODE" }
        $sequentialSamples += Get-Content -LiteralPath $reportPath -Raw | ConvertFrom-Json
    }
    .\scripts\stop-pluto-network.ps1 -Root $sequentialRoot

    Write-Host "Running the same transfer count on a fresh four-validator parallel network..."
    .\scripts\start-pluto-network.ps1 -Root $parallelRoot -Executor parallel -Workers auto
    go run ./cmd/plutoload -root $parallelRoot -count $Transactions
    if ($LASTEXITCODE -ne 0) { throw "Parallel mode warm-up failed with exit code $LASTEXITCODE" }
    $parallelSamples = @()
    for ($sample = 1; $sample -le $Samples; $sample++) {
        $reportPath = Join-Path $absoluteComparisonRoot ("parallel-{0}.json" -f $sample)
        go run ./cmd/plutoload -root $parallelRoot -count $Transactions -out $reportPath
        if ($LASTEXITCODE -ne 0) { throw "Parallel mode sample $sample failed with exit code $LASTEXITCODE" }
        $parallelSamples += Get-Content -LiteralPath $reportPath -Raw | ConvertFrom-Json
    }
    .\scripts\stop-pluto-network.ps1 -Root $parallelRoot

    function Get-Median {
        param([double[]]$Values)
        $ordered = @($Values | Sort-Object)
        $middle = [int][Math]::Floor($ordered.Count / 2)
        if (($ordered.Count % 2) -eq 1) { return [double]$ordered[$middle] }
        return ([double]$ordered[$middle - 1] + [double]$ordered[$middle]) / 2
    }
    $sequentialMillis = Get-Median @($sequentialSamples | ForEach-Object { [double]$_.elapsedMillis })
    $parallelMillis = Get-Median @($parallelSamples | ForEach-Object { [double]$_.elapsedMillis })
    $sequentialTPS = Get-Median @($sequentialSamples | ForEach-Object { [double]$_.transactionsPerSecond })
    $parallelTPS = Get-Median @($parallelSamples | ForEach-Object { [double]$_.transactionsPerSecond })
    $speedup = [Math]::Round(($sequentialMillis / [Math]::Max(1, $parallelMillis)), 3)
    $change = [Math]::Round((($parallelTPS - $sequentialTPS) / [Math]::Max(0.0001, $sequentialTPS)) * 100, 2)
    $sequentialRetries = [double](Get-Median @($sequentialSamples | ForEach-Object { [double]$_.retries }))
    $parallelRetries = [double](Get-Median @($parallelSamples | ForEach-Object { [double]$_.retries }))
    $sequentialWorkers = [string]$sequentialSamples[0].workers
    $parallelWorkers = [string]$parallelSamples[0].workers
    Write-Host ""
    Write-Host "Pluto validator network comparison (one warm-up + $Samples measured samples per mode; each mode used a fresh four-validator network)"
    [PSCustomObject]@{
        Transactions = $Transactions
        MeasuredSamples = $Samples
        SequentialMedianSeconds = [Math]::Round($sequentialMillis / 1000, 3)
        SequentialMedianTPS = [Math]::Round($sequentialTPS, 2)
        ParallelMedianSeconds = [Math]::Round($parallelMillis / 1000, 3)
        ParallelMedianTPS = [Math]::Round($parallelTPS, 2)
        ParallelSpeedup = "${speedup}x"
        ThroughputChangePercent = $change
        SequentialMedianRetries = $sequentialRetries
        ParallelMedianRetries = $parallelRetries
        SequentialWorkers = $sequentialWorkers
        ParallelWorkers = $parallelWorkers
    } | Format-List
    Write-Host "Raw JSON reports: $absoluteComparisonRoot"
}
finally {
    & (Join-Path $PSScriptRoot "stop-pluto-network.ps1") -Root $sequentialRoot
    & (Join-Path $PSScriptRoot "stop-pluto-network.ps1") -Root $parallelRoot
    Pop-Location
}
