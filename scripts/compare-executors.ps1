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

Push-Location $repoRoot
try {
    .\scripts\setup-pluto-network.ps1 -Root $sequentialRoot -PortOffset 100
    .\scripts\setup-pluto-network.ps1 -Root $parallelRoot -PortOffset 120

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
