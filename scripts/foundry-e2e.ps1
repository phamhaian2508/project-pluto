param(
    [string]$RpcUrl = "http://127.0.0.1:8545",
    [string]$PrivateKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
    [string]$Recipient = "0x0000000000000000000000000000000000001234"
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot

function Invoke-Tool {
    param([string]$Name, [string[]]$Arguments)
    $output = & $Name @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "$Name failed: $($output -join [Environment]::NewLine)"
    }
    return ($output -join [Environment]::NewLine).Trim()
}

function Get-Receipt {
    param([string]$Hash)
    $json = Invoke-Tool "cast" @("receipt", $Hash, "--rpc-url", $RpcUrl, "--json")
    $receipt = $json | ConvertFrom-Json
    if ([string]$receipt.status -notin @("0x1", "1")) {
        throw "Transaction $Hash failed with status $($receipt.status)"
    }
    return $receipt
}

foreach ($tool in @("forge", "cast")) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        throw "$tool is not installed or is missing from PATH"
    }
}

Push-Location $repoRoot
try {
    $chainId = Invoke-Tool "cast" @("chain-id", "--rpc-url", $RpcUrl)
    if ($chainId -ne "31337") {
        throw "Unexpected chain ID $chainId; expected 31337"
    }

    $sender = Invoke-Tool "cast" @("wallet", "address", "--private-key", $PrivateKey)
    $senderBalance = Invoke-Tool "cast" @("balance", $sender, "--rpc-url", $RpcUrl)
    if ([System.Numerics.BigInteger]::Parse($senderBalance) -le 0) {
        throw "Sender $sender has no genesis balance"
    }
    Write-Host "[ok] balance: $senderBalance wei"

    $transferHash = Invoke-Tool "cast" @(
        "send", $Recipient, "--value", "1wei", "--private-key", $PrivateKey,
        "--rpc-url", $RpcUrl, "--async"
    )
    $transferReceipt = Get-Receipt $transferHash
    $recipientBalance = Invoke-Tool "cast" @("balance", $Recipient, "--rpc-url", $RpcUrl)
    if ([System.Numerics.BigInteger]::Parse($recipientBalance) -lt 1) {
        throw "Recipient balance was not updated"
    }
    Write-Host "[ok] send + receipt: $($transferReceipt.transactionHash)"

    Invoke-Tool "forge" @("build") | Out-Null
    $bytecode = Invoke-Tool "forge" @("inspect", "Counter", "bytecode")
    $deployHash = Invoke-Tool "cast" @(
        "send", "--create", $bytecode, "--private-key", $PrivateKey,
        "--rpc-url", $RpcUrl, "--async"
    )
    $deployReceipt = Get-Receipt $deployHash
    $counter = [string]$deployReceipt.contractAddress
    if ([string]::IsNullOrWhiteSpace($counter) -or $counter -eq "0x0000000000000000000000000000000000000000") {
        throw "Deployment receipt has no contract address"
    }
    Write-Host "[ok] Counter deployed: $counter"

    $before = Invoke-Tool "cast" @("call", $counter, "count()(uint256)", "--rpc-url", $RpcUrl)
    if ($before -notmatch "^0(\s|$)") {
        throw "Initial Counter value is not zero: $before"
    }
    $incrementHash = Invoke-Tool "cast" @(
        "send", $counter, "increment()", "--private-key", $PrivateKey,
        "--rpc-url", $RpcUrl, "--async"
    )
    Get-Receipt $incrementHash | Out-Null
    $after = Invoke-Tool "cast" @("call", $counter, "count()(uint256)", "--rpc-url", $RpcUrl)
    if ($after -notmatch "^1(\s|$)") {
        throw "Counter value after increment is not one: $after"
    }
    Write-Host "[ok] contract call + transaction: count = 1"
    Write-Host "Foundry end-to-end test passed."
}
finally {
    Pop-Location
}

