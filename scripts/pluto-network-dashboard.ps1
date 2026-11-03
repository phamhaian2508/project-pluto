param(
    [string]$Root = ".pluto-network",
    [int]$RefreshSeconds = 1,
    [switch]$Once
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$rootPath = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $Root))
$networkPath = Join-Path $rootPath "network.json"
if (-not (Test-Path -LiteralPath $networkPath -PathType Leaf)) {
    throw "Pluto validator network not found at $rootPath. Run .\scripts\setup-pluto-network.ps1 first."
}
$network = Get-Content -LiteralPath $networkPath -Raw | ConvertFrom-Json

function Get-CometInfo {
    param([string]$Url)
    $response = Invoke-RestMethod -Uri "$Url/abci_info" -TimeoutSec 2
    $app = $response.result.response
    [PSCustomObject]@{
        Height = [long]$app.last_block_height
        AppHash = ([string]$app.last_block_app_hash).ToLowerInvariant().Replace("0x", "")
    }
}

function Get-PlutoBlockMetrics {
    param([string]$Url, [long]$Height)
    $heightHex = "0x{0:x}" -f $Height
    $body = @{ jsonrpc = "2.0"; id = 1; method = "pluto_getBlockMetrics"; params = @($heightHex) } |
        ConvertTo-Json -Depth 5 -Compress
    $response = Invoke-RestMethod -Uri $Url -Method Post -ContentType "application/json" -Body $body -TimeoutSec 3
    if ($response.error) { throw $response.error.message }
    return $response.result
}

function Get-CometCommitAppHash {
    param([string]$Url, [long]$Height)
    $response = Invoke-RestMethod -Uri "$Url/commit?height=$Height" -TimeoutSec 3
    return ([string]$response.result.signed_header.header.app_hash).ToLowerInvariant().Replace("0x", "")
}

$lastMetricHeight = 0L
$windowTransactions = 0L
$windowRetries = 0L
$windowStarted = Get-Date
$totalRetries = 0L

while ($true) {
    $nodes = @()
    foreach ($entry in $network.nodes) {
        try {
            $info = Get-CometInfo $entry.comet_rpc
            $nodes += [PSCustomObject]@{
                Name = $entry.name
                Height = $info.Height
                AppHash = ""
                State = "online"
                CometRPC = $entry.comet_rpc
                EthereumRPC = $entry.ethereum_rpc
            }
        }
        catch {
            $nodes += [PSCustomObject]@{
                Name = $entry.name
                Height = -1
                AppHash = ""
                State = "offline"
                CometRPC = $entry.comet_rpc
                EthereumRPC = $entry.ethereum_rpc
            }
        }
    }

    $online = @($nodes | Where-Object { $_.State -eq "online" })
    $sameHeight = $online.Count -eq $network.nodes.Count
    $mismatch = $false
    $commonHeight = 0L
    if ($sameHeight) {
        $commonHeight = [long](($online | Measure-Object -Property Height -Minimum).Minimum)
        if ($commonHeight -gt 0) {
            foreach ($node in $online) {
                try { $node.AppHash = Get-CometCommitAppHash $node.CometRPC $commonHeight }
                catch { $sameHeight = $false; break }
            }
            if ($sameHeight) { $mismatch = (@($online.AppHash | Select-Object -Unique).Count -gt 1) }
        }
    }

    $newHeight = $lastMetricHeight
    if ($sameHeight) { $newHeight = $commonHeight }
    $windowTransactions = 0L
    $windowRetries = 0L
    if ($newHeight -gt $lastMetricHeight) {
        $first = [Math]::Max(1L, $lastMetricHeight + 1)
        for ($height = $first; $height -le $newHeight; $height++) {
            try {
                $metrics = Get-PlutoBlockMetrics $network.nodes[0].ethereum_rpc $height
                $windowTransactions += [long]$metrics.transactionCount
                $windowRetries += [long]$metrics.retryCount
            }
            catch { }
        }
        $lastMetricHeight = $newHeight
        $totalRetries += $windowRetries
    }
    $elapsed = [Math]::Max(0.001, ((Get-Date) - $windowStarted).TotalSeconds)
    $tps = [Math]::Round($windowTransactions / $elapsed, 2)

    Clear-Host
    Write-Host "PLUTO VALIDATOR NETWORK | EVM chain ID $($network.evm_chain_id)"
    Write-Host "Validators online: $($online.Count)/$($network.nodes.Count) | AppHash sampled at common height $commonHeight | TPS is committed transactions observed per refresh interval"
    Write-Host "Latest interval: $windowTransactions tx | $tps tx/s | retries: $windowRetries | retries observed total: $totalRetries"
    $nodes | Select-Object Name, State, Height, AppHash | Format-Table -AutoSize

    if ($mismatch) {
		Write-Host "AppHash mismatch at height $($online[0].Height). Stopping only the managed network node processes recorded in processes.json." -ForegroundColor Red
        & (Join-Path $PSScriptRoot "stop-pluto-network.ps1") -Root $Root
        exit 2
    }
    if (-not $sameHeight -and $online.Count -eq $network.nodes.Count) {
        Write-Host "Could not read a common committed height from every validator; waiting before comparing hashes."
    }
    if ($Once) { break }
    $windowStarted = Get-Date
    Start-Sleep -Seconds ([Math]::Max(1, $RefreshSeconds))
}
