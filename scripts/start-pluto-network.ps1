param(
    [string]$Root = ".pluto-network",
    [string]$Node = "",
    [ValidateSet("sequential", "parallel")][string]$Executor = "parallel",
    [string]$Workers = "auto",
    [ValidateSet("debug", "info", "error", "none")][string]$LogLevel = "",
    [string]$TimeoutCommit = "",
    [switch]$Rebuild
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$rootPath = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $Root))
$networkPath = Join-Path $rootPath "network.json"
$manifestPath = Join-Path $rootPath "processes.json"
$binaryDir = Join-Path $rootPath "bin"
$binary = Join-Path $binaryDir "plutod.exe"

if (-not (Test-Path -LiteralPath $networkPath -PathType Leaf)) {
    throw "Pluto validator network not found at $rootPath. Run .\scripts\setup-pluto-network.ps1 first."
}
$network = Get-Content -LiteralPath $networkPath -Raw | ConvertFrom-Json
$selectedNodes = @($network.nodes)
if ($Node) {
    $selectedNodes = @($selectedNodes | Where-Object { $_.name -eq $Node })
    if ($selectedNodes.Count -ne 1) { throw "Unknown managed network node '$Node'." }
}

$records = @()
if (Test-Path -LiteralPath $manifestPath -PathType Leaf) {
    $old = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    $records = @($old.nodes)
}

if ($Rebuild) {
    foreach ($record in $records) {
        $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $($record.pid)" -ErrorAction SilentlyContinue
        if ($proc -and $proc.ExecutablePath -eq [System.IO.Path]::GetFullPath($record.binary) -and $proc.CommandLine.Contains([System.IO.Path]::GetFullPath($record.home))) {
            throw "Stop managed node '$($record.name)' before rebuilding its executable: .\scripts\stop-pluto-network.ps1"
        }
    }
}

if ($Rebuild -or -not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    New-Item -ItemType Directory -Force -Path $binaryDir | Out-Null
    Push-Location $repoRoot
    try {
        go build -o $binary ./cmd/plutod
        if ($LASTEXITCODE -ne 0) { throw "Building plutod failed with exit code $LASTEXITCODE" }
    }
    finally { Pop-Location }
}

$binaryFullPath = [System.IO.Path]::GetFullPath($binary)
if ($Executor -eq "parallel" -and $Workers -ne "auto") {
    $workerCount = 0
    if (-not [int]::TryParse($Workers, [ref]$workerCount) -or $workerCount -lt 1) {
        throw "Workers must be 'auto' or a positive integer."
    }
}
elseif ($Executor -eq "sequential") {
    $Workers = "1"
}
if ($TimeoutCommit -and $TimeoutCommit -notmatch '^\d+(ms|s|m|h)$') {
    throw "TimeoutCommit must be a positive duration such as 1500ms, 3s, or 1m."
}

foreach ($entry in $selectedNodes) {
    $homeFullPath = [System.IO.Path]::GetFullPath($entry.home)
    $existing = $records | Where-Object { $_.name -eq $entry.name } | Select-Object -First 1
    if ($existing) {
        $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $($existing.pid)" -ErrorAction SilentlyContinue
        if ($proc -and $proc.ExecutablePath -eq $binaryFullPath -and $proc.CommandLine.Contains($homeFullPath)) {
            Write-Host "$($entry.name) is already running (PID $($existing.pid))."
            continue
        }
        $records = @($records | Where-Object { $_.name -ne $entry.name })
    }

    $ethAddress = ([uri]$entry.ethereum_rpc).Authority
    $stdout = Join-Path $rootPath "logs\$($entry.name).out.log"
    $stderr = Join-Path $rootPath "logs\$($entry.name).err.log"
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $stdout) | Out-Null
    $argumentLine = "--home `"$homeFullPath`" --executor $Executor --workers $Workers --eth-rpc `"$ethAddress`""
    if ($LogLevel) { $argumentLine += " --log-level $LogLevel" }
    if ($TimeoutCommit) { $argumentLine += " --timeout-commit $TimeoutCommit" }
    $process = Start-Process -FilePath $binaryFullPath -ArgumentList $argumentLine -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    $records += [PSCustomObject]@{
        name = $entry.name
        pid = $process.Id
        home = $homeFullPath
        binary = $binaryFullPath
        executor = $Executor
        workers = $Workers
    }
    Write-Host "Started $($entry.name) (PID $($process.Id)); Ethereum RPC $($entry.ethereum_rpc)"
}

$manifest = [PSCustomObject]@{ nodes = @($records) }
$manifest | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $manifestPath -Encoding utf8

foreach ($entry in $selectedNodes) {
    $ready = $false
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        try {
            $null = Invoke-RestMethod -Uri "$($entry.comet_rpc)/status" -TimeoutSec 2
            $ready = $true
            break
        }
        catch { Start-Sleep -Milliseconds 500 }
    }
    if (-not $ready) {
        Write-Warning "$($entry.name) RPC is not ready yet. Read logs under $rootPath\logs."
    }
}

if (-not $Node) {
    $networkReady = $false
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        try {
            $snapshots = @()
            foreach ($entry in $selectedNodes) {
                $response = Invoke-RestMethod -Uri "$($entry.comet_rpc)/abci_info" -TimeoutSec 2
                $snapshots += [PSCustomObject]@{ Name = $entry.name; Url = $entry.comet_rpc; Height = [long]$response.result.response.last_block_height }
            }
            $commonHeight = [long](($snapshots | Measure-Object -Property Height -Minimum).Minimum)
            if ($commonHeight -lt 1) { throw "The validators have not committed their first block yet." }
            $hashes = @()
            foreach ($snapshot in $snapshots) {
                $commit = Invoke-RestMethod -Uri "$($snapshot.Url)/commit?height=$commonHeight" -TimeoutSec 2
                $hashes += [string]$commit.result.signed_header.header.app_hash
            }
            if (@($hashes | Select-Object -Unique).Count -eq 1) {
                $networkReady = $true
                Write-Host "Four-validator network ready at common CometBFT height $commonHeight; AppHash matches."
                break
            }
        }
        catch { }
        Start-Sleep -Milliseconds 500
    }
    if (-not $networkReady) {
        & (Join-Path $PSScriptRoot "stop-pluto-network.ps1") -Root $Root
        throw "All node RPCs started, but validators did not reach a common committed height/AppHash. Managed managed network nodes were stopped; inspect logs under $rootPath\logs."
    }
}

Write-Host "Started executor mode: $Executor; workers: $Workers"
Write-Host "Monitor: .\scripts\pluto-network-dashboard.ps1"
