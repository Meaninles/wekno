[CmdletBinding()]
param(
    [ValidateSet("dev", "gate", "sealed_holdout", "grader_calibration")]
    [string]$Split = "gate",
    [string]$Dataset = "",
    [string]$Manifest = "",
    [string]$Policy = "",
    [string]$Baseline = "",
    [string[]]$CaseId = @(),
    [int]$MaxConcurrency = 1,
    [ValidateRange(1, 3600)]
    [int]$ResponseDeadlineSeconds = 240,
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
$calibrationResult = "/workspace/artifacts/calibration-$timestamp.json"
$rescoredBaseline = "/workspace/artifacts/baseline-$timestamp-rescored.json"
$judgedBaseline = "/workspace/artifacts/baseline-$timestamp-judged.json"

if (-not $Dataset) {
    $Dataset = if ($Split -eq "sealed_holdout") {
        "/workspace/sealed/multiturn-holdout.v1.jsonl"
    } else {
        "/workspace/datasets/multiturn-ready.v3.jsonl"
    }
}
if (-not $Manifest) {
    $Manifest = if ($Split -eq "sealed_holdout") {
        "/workspace/manifests/multiturn-holdout.v1.manifest.json"
    } else {
        "/workspace/manifests/multiturn-ready.v3.manifest.json"
    }
}
if (-not $Policy) {
    $Policy = if ($Split -eq "sealed_holdout") {
        "/workspace/policies/multiturn-sealed-gate.v1.json"
    } else {
        "/workspace/policies/multiturn-release-gate.v2.json"
    }
}

function Invoke-Runner {
    param(
        [Parameter(Mandatory)] [string[]]$RunnerArgs,
        [int[]]$AllowedExitCodes = @(0)
    )
    & docker compose --env-file $mainEnv --env-file $evalEnv `
        -p weknora-agent-eval-platform -f $composeFile `
        run --rm agent-eval-runner @RunnerArgs | ForEach-Object { Write-Host $_ }
    $exitCode = $LASTEXITCODE
    if ($AllowedExitCodes -notcontains $exitCode) {
        throw "eval runner failed with exit code $exitCode"
    }
    return $exitCode
}

if ($MaxConcurrency -lt 1) { throw "MaxConcurrency must be >= 1" }
if ($Split -eq "sealed_holdout" -and -not $AllowSealed) {
    throw "sealed_holdout requires the explicit -AllowSealed switch"
}

$repositoryRoot = (& git -C $PSScriptRoot rev-parse --show-toplevel).Trim()
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($repositoryRoot)) {
    throw "failed to resolve repository root"
}
$env:AGENT_EVAL_FRAMEWORK_COMMIT = (& git -C $repositoryRoot rev-parse "HEAD:custom/services/agent-eval").Trim()
if ($LASTEXITCODE -ne 0) { throw "failed to resolve eval framework tree identity" }
$dirtyLines = @(& git -C $repositoryRoot status --porcelain -- custom/services/agent-eval)
if ($LASTEXITCODE -ne 0) { throw "failed to inspect eval framework worktree" }
$env:AGENT_EVAL_WORKTREE_DIRTY = if ($dirtyLines.Count -gt 0) { "true" } else { "false" }
$env:AGENT_EVAL_SUT_COMMIT = (& git -C $repositoryRoot rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0) { throw "failed to resolve SUT source commit" }
$sutDirtyLines = @(& git -C $repositoryRoot status --porcelain)
if ($LASTEXITCODE -ne 0) { throw "failed to inspect SUT worktree" }
$env:AGENT_EVAL_SUT_WORKTREE_DIRTY = if ($sutDirtyLines.Count -gt 0) { "true" } else { "false" }

foreach ($project in @("weknora", "weknora-runtime-profile-e2e")) {
    $running = @(& docker ps --quiet --filter "label=com.docker.compose.project=$project")
    if ($LASTEXITCODE -ne 0) { throw "failed to inspect Docker project $project" }
    if ($running.Count -gt 0) {
        throw "main Docker project '$project' is still running; stop it before eval preparation or execution"
    }
}

& docker version --format "{{.Server.Version}}" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Docker Desktop is not running" }

function Get-RunningImageId {
    param([Parameter(Mandatory)] [string]$ContainerName)
    $running = (& docker inspect --format "{{.State.Running}}" $ContainerName).Trim()
    if ($LASTEXITCODE -ne 0 -or $running -ne "true") {
        throw "required eval container '$ContainerName' is not running"
    }
    $imageID = (& docker inspect --format "{{.Image}}" $ContainerName).Trim()
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($imageID)) {
        throw "failed to resolve running image for '$ContainerName'"
    }
    return $imageID
}

$env:AGENT_EVAL_RUNTIME_IMAGE_ID = Get-RunningImageId "weknora-agent-eval-runtime-api-1"
$env:AGENT_EVAL_GENERAL_AGENT_IMAGE_ID = Get-RunningImageId "weknora-agent-eval-general-agent"
$prepareRunnerEnv = -not (Test-Path -LiteralPath $runnerEnv)
if (-not $prepareRunnerEnv) {
    $runnerValues = @{}
    Get-Content -LiteralPath $runnerEnv | ForEach-Object {
        if ($_ -match '^([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
            $runnerValues[$matches[1]] = $matches[2]
        }
    }
    foreach ($requiredJudgeVariable in @(
        "AGENT_EVAL_JUDGE_BASE_URL",
        "AGENT_EVAL_JUDGE_API_KEY",
        "AGENT_EVAL_JUDGE_MODEL"
    )) {
        if (-not $runnerValues.ContainsKey($requiredJudgeVariable) -or `
            [string]::IsNullOrWhiteSpace($runnerValues[$requiredJudgeVariable])) {
            $prepareRunnerEnv = $true
        }
    }
}
if ($prepareRunnerEnv) {
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

$judgeEnabled = $Judge -or -not [string]::IsNullOrWhiteSpace($Baseline)
if ($judgeEnabled) {
    Invoke-Runner -RunnerArgs @(
        "calibration", "run",
        "--input", "/workspace/calibration/judge-multiturn.v1.json",
        "--output", $calibrationResult
    ) | Out-Null
}

$gateBaseline = $Baseline
if ($Baseline -and $judgeEnabled) {
    Invoke-Runner -RunnerArgs @(
        "score", "--dataset", $Dataset, "--run", $Baseline,
        "--output", $rescoredBaseline
    ) | Out-Null
    Invoke-Runner -RunnerArgs @(
        "judge", "--dataset", $Dataset, "--run", $rescoredBaseline,
        "--calibration-result", $calibrationResult,
        "--output", $judgedBaseline
    ) -AllowedExitCodes @(0, 2) | Out-Null
    $gateBaseline = $judgedBaseline
}

$runArgs = @(
    "run", "--dataset", $Dataset, "--output", $rawRun,
    "--split", $Split, "--max-concurrency", [string]$MaxConcurrency,
    "--timeout", [string]$ResponseDeadlineSeconds,
    "--label", "codex-loop-$timestamp",
    "--profiles", "/workspace/profiles/multiturn-agents.v1.json"
)
foreach ($selectedCase in $CaseId) {
    if (-not [string]::IsNullOrWhiteSpace($selectedCase)) {
        $runArgs += @("--case-id", $selectedCase.Trim())
    }
}
if ($Split -eq "sealed_holdout") { $runArgs += "--allow-sealed" }
if ($PublishToLangfuse) {
    $runArgs += @("--langfuse-experiment", "codex-$Split-$timestamp")
}
# `run` returns 2 when one or more cases are INVALID (for example a WAF 403).
# The raw artifact is still complete and must flow through report/gate so infra
# failures stay isolated from both the eval loop and the product request path.
Invoke-Runner -RunnerArgs $runArgs -AllowedExitCodes @(0, 2) | Out-Null

$candidate = $rawRun
if ($judgeEnabled) {
    $judgeArgs = @(
        "judge", "--dataset", $Dataset, "--run", $rawRun,
        "--calibration-result", $calibrationResult,
        "--output", $judgedRun
    )
    if ($gateBaseline) { $judgeArgs += @("--baseline", $gateBaseline) }
    Invoke-Runner -RunnerArgs $judgeArgs -AllowedExitCodes @(0, 2) | Out-Null
    $candidate = $judgedRun
}

$gateExit = 0
if ($Baseline) {
    $gateExit = Invoke-Runner -RunnerArgs @(
        "gate", "--dataset", $Dataset, "--candidate", $candidate,
        "--baseline", $gateBaseline,
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
if ($judgeEnabled) { Write-Host "Judge calibration: $calibrationResult" }
Write-Host "Report: $report"
if ($Baseline) { Write-Host "Gate: $gateResult (exit=$gateExit)" }
if ($Baseline) { Write-Host "Adjudicated baseline: $gateBaseline" }
exit $gateExit
