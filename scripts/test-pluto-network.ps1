param(
    [string]$Root = ".pluto-network",
    [string]$PrivateKey = $env:PLUTO_TEST_KEY,
    [int]$LoadTransactions = 10,
    [switch]$RunExecutorComparison
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$rootPath = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $Root))
$network = Get-Content -LiteralPath (Join-Path $rootPath "network.json") -Raw | ConvertFrom-Json
$rpcUrl = $network.nodes[0].ethereum_rpc

if ([string]::IsNullOrWhiteSpace($PrivateKey)) {
    throw "Set PLUTO_TEST_KEY to the local test account's private key first. Do not use a real wallet key."
}
foreach ($tool in @("forge", "cast")) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "$tool is required and must be on PATH." }
}

function Invoke-Tool {
    param([string]$Name, [string[]]$Arguments)
    $output = & $Name @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) { throw "$Name failed: $($output -join [Environment]::NewLine)" }
    return ($output -join [Environment]::NewLine).Trim()
}

function Get-Receipt {
    param([string]$Hash)
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        try {
            $json = Invoke-Tool "cast" @("receipt", $Hash, "--rpc-url", $rpcUrl, "--json")
            $receipt = $json | ConvertFrom-Json
        }
        catch {
            if ($attempt -eq 59) { throw "Timed out waiting for transaction $($Hash): $_" }
            Start-Sleep -Milliseconds 500
            continue
        }
        if ([string]$receipt.status -notin @("0x1", "1")) { throw "Transaction $Hash failed with status $($receipt.status)." }
        return $receipt
    }
}

Push-Location $repoRoot
try {
    $chainId = Invoke-Tool "cast" @("chain-id", "--rpc-url", $rpcUrl)
    if ($chainId -ne "31337") { throw "Unexpected EVM chain ID $chainId; expected 31337." }

    Write-Host "Running the existing transfer + Counter deployment/call smoke test on validator1..."
    & (Join-Path $PSScriptRoot "foundry-e2e.ps1") -RpcUrl $rpcUrl -PrivateKey $PrivateKey
    if ($LASTEXITCODE -ne 0) { throw "Foundry end-to-end smoke test failed." }

    Write-Host "Deploying PlutoToken to the four-validator network..."
    Invoke-Tool "forge" @("build") | Out-Null
    $bytecode = Invoke-Tool "forge" @("inspect", "PlutoToken", "bytecode")
    if (-not $bytecode.StartsWith("0x")) { $bytecode = "0x$bytecode" }
    $sendOutput = Invoke-Tool "cast" @("send", "--create", $bytecode, "--private-key", $PrivateKey, "--rpc-url", $rpcUrl, "--async")
    $txMatch = [regex]::Match($sendOutput, "0x[0-9a-fA-F]{64}")
    if (-not $txMatch.Success) { throw "Could not read deployment transaction hash from cast output: $sendOutput" }
    $deployHash = $txMatch.Value
    $receipt = Get-Receipt $deployHash
    $tokenAddress = [string]$receipt.contractAddress
    if ([string]::IsNullOrWhiteSpace($tokenAddress) -or $tokenAddress -match "^0x0+$") {
        throw "Deployment receipt has no contract address: $receiptJson"
    }
    $symbol = Invoke-Tool "cast" @("call", $tokenAddress, "symbol()(string)", "--rpc-url", $rpcUrl)
    if ($symbol -notmatch "PTKN") { throw "Unexpected PlutoToken symbol: $symbol" }
    Write-Host "[ok] PlutoToken deployed at $tokenAddress ($symbol)"

    foreach ($node in $network.nodes) {
        $code = Invoke-Tool "cast" @("code", $tokenAddress, "--rpc-url", $node.ethereum_rpc)
        if ($code -eq "0x") { throw "Validator $($node.name) has not committed the deployed contract yet." }
        Write-Host "[ok] $($node.name) reports the deployed contract code"
    }

    $recipients = @(
        "0x0000000000000000000000000000000000001001",
        "0x0000000000000000000000000000000000001002",
        "0x0000000000000000000000000000000000001003",
        "0x0000000000000000000000000000000000001004"
    )
    for ($i = 0; $i -lt $LoadTransactions; $i++) {
        $recipient = $recipients[$i % $recipients.Count]
        $sendOutput = Invoke-Tool "cast" @("send", $recipient, "--value", "1wei", "--private-key", $PrivateKey, "--rpc-url", $rpcUrl, "--async")
        $txMatch = [regex]::Match($sendOutput, "0x[0-9a-fA-F]{64}")
        if (-not $txMatch.Success) { throw "Could not read load transaction hash: $sendOutput" }
        Get-Receipt $txMatch.Value | Out-Null
    }
    Write-Host "[ok] Submitted and confirmed $LoadTransactions network transfer transactions"
    Write-Host "Check all validator heights and AppHashes with .\scripts\pluto-network-dashboard.ps1 -Root `"$Root`""

    if ($RunExecutorComparison) {
        Write-Host "Running the existing sequential-versus-parallel executor report (this is a controlled block-executor benchmark, not network TPS)..."
        go test ./test/harness -run '^TestSequentialParallelPerformanceReport$' -count=1 -v
        if ($LASTEXITCODE -ne 0) { throw "Sequential/parallel executor comparison failed." }
    }
}
finally { Pop-Location }
