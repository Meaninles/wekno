[CmdletBinding()]
param(
    [ValidateSet("dev", "gate", "sealed_holdout", "grader_calibration")]
    [string]$Split = "gate",
    [string]$Dataset = "",
    [string]$Manifest = "",
    [string]$Policy = "",
    [string]$Baseline = "",
    [int]$MaxConcurrency = 1,
    [bool]$PublishToLangfuse = $true,
    [switch]$Judge,
    [switch]$AllowSealed,
    [switch]$PreflightOnly
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$mainEnv = "C:\weknora\.env"
$evalEnv = Join-Path $PSScriptRoot "eval.env"
$runnerEnv = Join-Path $PSScriptRoot "runner.env"
$composeFile = Join-Path $PSScriptRoot "docker-compose.yml"
$timestamp = Get-Date -Format "yyyyMMdd-HHmmss"
$rawRun = "/workspace/artifacts/run-$timestamp.json"
$judgedRun = "/workspace/artifacts/run-$timestamp-judged.json"
$gateResult = "/workspace/artifacts/gate-$timestamp.json"
$report = "/workspace/artifacts/report-$timestamp.md"
$preflight = "/workspace/artifacts/preflight-$timestamp.json"

if (-not $Dataset) {
    $Dataset = if ($Split -eq "sealed_holdout") {
        "/workspace/sealed/multiturn-holdout.v1.jsonl"
    } else {
        "/workspace/datasets/multiturn-ready.v1.jsonl"
    }
}
if (-not $Manifest) {
    $Manifest = if ($Split -eq "sealed_holdout") {
        "/workspace/manifests/multiturn-holdout.v1.manifest.json"
    } else {
        "/workspace/manifests/multiturn-ready.v1.manifest.json"
    }
}
if (-not $Policy) {
    $Policy = if ($Split -eq "sealed_holdout") {
        "/workspace/policies/multiturn-sealed-gate.v1.json"
    } else {
        "/workspace/policies/multiturn-release-gate.v1.json"
    }
}

function Invoke-Runner {
    param(
        [Parameter(Mandatory)] [string[]]$RunnerArgs,
        [int[]]$AllowedExitCodes = @(0)
    )
    $output = & docker compose --env-file $mainEnv --env-file $evalEnv `
        -p weknora-agent-eval-platform -f $composeFile `
        run --rm agent-eval-runner @RunnerArgs
    $exitCode = $LASTEXITCODE
    $output | ForEach-Object { Write-Host $_ }
    if ($AllowedExitCodes -notcontains $exitCode) {
        throw "eval runner failed with exit code $exitCode"
    }
    return $exitCode
}

if ($MaxConcurrency -lt 1) { throw "MaxConcurrency must be >= 1" }
if ($Split -eq "sealed_holdout" -and -not $AllowSealed) {
    throw "sealed_holdout requires the explicit -AllowSealed switch"
}

foreach ($project in @("weknora", "weknora-runtime-profile-e2e")) {
    $running = @(& docker ps --quiet --filter "label=com.docker.compose.project=$project")
    if ($LASTEXITCODE -ne 0) { throw "failed to inspect Docker project $project" }
    if ($running.Count -gt 0) {
        throw "main Docker project '$project' is still running; stop it before eval preparation or execution"
    }
}

& docker version --format "{{.Server.Version}}" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Docker Desktop is not running" }
if (-not (Test-Path -LiteralPath $runnerEnv)) {
    & (Join-Path $PSScriptRoot "prepare-runner-env.ps1")
    if ($LASTEXITCODE -ne 0) { throw "failed to prepare isolated runner.env" }
}
& docker compose --env-file $mainEnv --env-file $evalEnv `
    -p weknora-agent-eval-platform -f $composeFile build agent-eval-runner
if ($LASTEXITCODE -ne 0) { throw "failed to build eval runner image" }

Invoke-Runner -RunnerArgs @("dataset", "validate", "--input", $Dataset) | Out-Null
Invoke-Runner -RunnerArgs @(
    "calibration", "validate",
    "--input", "/workspace/calibration/judge-multiturn.v1.json"
) | Out-Null
Invoke-Runner -RunnerArgs @(
    "preflight",
    "--dataset", $Dataset,
    "--manifest", $Manifest,
    "--profiles", "/workspace/profiles/multiturn-agents.v1.json",
    "--policy", $Policy,
    "--calibration", "/workspace/calibration/judge-multiturn.v1.json",
    "--split", $Split,
    "--output", $preflight
) | Out-Null

if ($PreflightOnly) {
    Write-Host "READY: no eval sessions or chat requests were created."
    Write-Host "Preflight: $preflight"
    Write-Host "Frozen dataset manifest: $Manifest"
    exit 0
}

$runArgs = @(
    "run", "--dataset", $Dataset, "--output", $rawRun,
    "--split", $Split, "--max-concurrency", [string]$MaxConcurrency,
    "--label", "codex-loop-$timestamp",
    "--profiles", "/workspace/profiles/multiturn-agents.v1.json"
)
if ($Split -eq "sealed_holdout") { $runArgs += "--allow-sealed" }
if ($PublishToLangfuse) {
    $runArgs += @("--langfuse-experiment", "codex-$Split-$timestamp")
}
Invoke-Runner -RunnerArgs $runArgs | Out-Null

$candidate = $rawRun
if ($Judge) {
    $judgeArgs = @("judge", "--dataset", $Dataset, "--run", $rawRun, "--output", $judgedRun)
    if ($Baseline) { $judgeArgs += @("--baseline", $Baseline) }
    Invoke-Runner -RunnerArgs $judgeArgs | Out-Null
    $candidate = $judgedRun
}

$gateExit = 0
if ($Baseline) {
    $gateExit = Invoke-Runner -RunnerArgs @(
        "gate", "--dataset", $Dataset, "--candidate", $candidate,
        "--baseline", $Baseline,
        "--policy", $Policy,
        "--output", $gateResult
    ) -AllowedExitCodes @(0, 1, 2)
    Invoke-Runner -RunnerArgs @("report", "--run", $candidate, "--gate", $gateResult, "--output", $report) | Out-Null
} else {
    Invoke-Runner -RunnerArgs @("report", "--run", $candidate, "--output", $report) | Out-Null
    Write-Warning "No baseline supplied: experiment completed, but no release verdict was issued."
}

Write-Host "Candidate: $candidate"
Write-Host "Dataset manifest: $Manifest"
Write-Host "Preflight: $preflight"
Write-Host "Report: $report"
if ($Baseline) { Write-Host "Gate: $gateResult (exit=$gateExit)" }
exit $gateExit
