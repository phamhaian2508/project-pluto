param(
    [string]$Root = ".pluto-network",
    [string]$Node = "",
    [ValidateSet("sequential", "parallel")][string]$Executor = "parallel",
    [string]$Workers = "auto",
    [ValidateSet("p0", "p1", "p2", "p3")][string]$CheckpointPolicy = "p0",
    [UInt64]$CheckpointInterval = 100,
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

$sourcePaths = @(
    (Join-Path $repoRoot "cmd"),
    (Join-Path $repoRoot "internal"),
    (Join-Path $repoRoot "go.mod"),
    (Join-Path $repoRoot "go.sum")
)
$sourceFiles = @()
foreach ($sourcePath in $sourcePaths) {
    if (Test-Path -LiteralPath $sourcePath -PathType Container) {
        $sourceFiles += Get-ChildItem -LiteralPath $sourcePath -Filter "*.go" -File -Recurse
    }
    elseif (Test-Path -LiteralPath $sourcePath -PathType Leaf) {
        $sourceFiles += Get-Item -LiteralPath $sourcePath
    }
}
$needsBuild = $Rebuild -or -not (Test-Path -LiteralPath $binary -PathType Leaf)
if (-not $needsBuild) {
    $binaryWriteTime = (Get-Item -LiteralPath $binary).LastWriteTimeUtc
    $newestSourceWriteTime = ($sourceFiles | Measure-Object -Property LastWriteTimeUtc -Maximum).Maximum
    if ($newestSourceWriteTime -gt $binaryWriteTime) {
        $needsBuild = $true
        Write-Host "Go source is newer than the managed plutod binary; rebuilding it."
    }
}

if ($needsBuild) {
    foreach ($record in $records) {
        $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $($record.pid)" -ErrorAction SilentlyContinue
        if ($proc -and $proc.ExecutablePath -eq [System.IO.Path]::GetFullPath($record.binary) -and $proc.CommandLine.Contains([System.IO.Path]::GetFullPath($record.home))) {
            throw "Stop managed node '$($record.name)' before rebuilding its executable: .\scripts\stop-pluto-network.ps1"
        }
    }
}

if ($needsBuild) {
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
if ($CheckpointInterval -lt 1) {
    throw "CheckpointInterval must be a positive integer."
}
if ($Executor -eq "sequential" -and $CheckpointPolicy -ne "p0") {
    throw "Checkpoint policies p1-p3 require -Executor parallel."
}
if ($TimeoutCommit -and $TimeoutCommit -notmatch '^\d+(ms|s|m|h)$') {
    throw "TimeoutCommit must be a positive duration such as 1500ms, 3s, or 1m."
}

# Checkpointing changes how each application replays speculative EVM work. All
# validators in one consensus network must use the same execution settings.
$requestedWorkers = $Workers
foreach ($record in $records) {
    if ($selectedNodes.name -contains $record.name) { continue }
    $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $($record.pid)" -ErrorAction SilentlyContinue
    if (-not $proc) { continue }

    $recordExecutor = if ($record.executor) { [string]$record.executor } else { "parallel" }
    $recordWorkers = if ($record.workers) { [string]$record.workers } else { "auto" }
    $recordPolicy = if ($record.checkpoint_policy) { [string]$record.checkpoint_policy } else { "p0" }
    $recordInterval = if ($record.checkpoint_interval) { [UInt64]$record.checkpoint_interval } else { [UInt64]100 }
    if ($recordExecutor -ne $Executor -or $recordWorkers -ne $requestedWorkers -or
        $recordPolicy -ne $CheckpointPolicy -or $recordInterval -ne $CheckpointInterval) {
        throw "Running validator '$($record.name)' has different executor/checkpoint settings. Stop the whole network before changing settings, then start all validators with the same options."
    }
}

foreach ($entry in $selectedNodes) {
    $homeFullPath = [System.IO.Path]::GetFullPath($entry.home)
    $existing = $records | Where-Object { $_.name -eq $entry.name } | Select-Object -First 1
    if ($existing) {
        $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $($existing.pid)" -ErrorAction SilentlyContinue
        if ($proc -and $proc.ExecutablePath -eq $binaryFullPath -and $proc.CommandLine.Contains($homeFullPath)) {
            $recordPolicy = if ($existing.checkpoint_policy) { [string]$existing.checkpoint_policy } else { "p0" }
            $recordInterval = if ($existing.checkpoint_interval) { [UInt64]$existing.checkpoint_interval } else { [UInt64]100 }
            if ($existing.executor -ne $Executor -or $existing.workers -ne $requestedWorkers -or
                $recordPolicy -ne $CheckpointPolicy -or $recordInterval -ne $CheckpointInterval) {
                throw "Validator '$($entry.name)' is already running with different executor/checkpoint settings. Stop the whole network before changing settings."
            }
            Write-Host "$($entry.name) is already running (PID $($existing.pid))."
            continue
        }
        $records = @($records | Where-Object { $_.name -ne $entry.name })
    }

    $ethAddress = ([uri]$entry.ethereum_rpc).Authority
    $stdout = Join-Path $rootPath "logs\$($entry.name).out.log"
    $stderr = Join-Path $rootPath "logs\$($entry.name).err.log"
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $stdout) | Out-Null
    $argumentLine = "--home `"$homeFullPath`" --executor $Executor --workers $Workers --checkpoint-policy $CheckpointPolicy --checkpoint-interval $CheckpointInterval --eth-rpc `"$ethAddress`""
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
        checkpoint_policy = $CheckpointPolicy
        checkpoint_interval = $CheckpointInterval
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
Write-Host "Checkpoint policy: $CheckpointPolicy; interval: $CheckpointInterval opcode(s) (used by P2)"
Write-Host "Monitor: .\scripts\pluto-network-dashboard.ps1"
