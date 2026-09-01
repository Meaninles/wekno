[CmdletBinding()]
param(
    [ValidateSet("dev", "gate", "sealed_holdout", "grader_calibration")]
    [string[]]$Split = @("dev"),
    [string]$Dataset = "/workspace/datasets/unseen-capability-matrix.v1.jsonl",
    [string]$Manifest = "",
    [string]$Policy = "",
    [string]$Profiles = "/workspace/profiles/unseen-capability-matrix.v1.json",
    [string]$Run = "",
    [string]$Baseline = "",
    [string[]]$CodexReview = @(),
    [string[]]$CaseId = @(),
    [int]$MaxConcurrency = 1,
    [ValidateRange(1, 3600)]
    [int]$ResponseDeadlineSeconds = 240,
    [bool]$PublishToLangfuse = $true,
    [switch]$EnableEvalAssistance,
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
$rawRun = if ([string]::IsNullOrWhiteSpace($Run)) {
    "/workspace/artifacts/run-$timestamp.json"
} else {
    $Run
}
$reviewedRun = "/workspace/artifacts/run-$timestamp-reviewed.json"
$productionReviewPacket = "/workspace/artifacts/codex-review-$timestamp-production.json"
$assistedReviewPacket = "/workspace/artifacts/codex-review-$timestamp-assisted.json"
$gateResult = "/workspace/artifacts/gate-$timestamp.json"
$report = "/workspace/artifacts/report-$timestamp.md"

if ($Split.Count -eq 0) { throw "at least one Split is required" }
$Split = @($Split | Select-Object -Unique)
if ($MaxConcurrency -lt 1) { throw "MaxConcurrency must be >= 1" }
if ($Split -contains "sealed_holdout" -and -not $AllowSealed) {
    throw "sealed_holdout requires the explicit -AllowSealed switch"
}
if ($PreflightOnly -and -not [string]::IsNullOrWhiteSpace($Run)) {
    throw "PreflightOnly cannot be combined with an existing Run"
}

if (-not $Policy) {
    $Policy = if ($Split.Count -eq 1 -and $Split[0] -eq "dev") {
        "/workspace/policies/eval-optimization-gate.v3.json"
    } else {
        "/workspace/policies/production-multiturn-release-gate.v3.json"
    }
}
if (-not $Manifest) {
    $Manifest = switch (Split-Path -Leaf $Policy) {
        "eval-optimization-gate.v3.json" {
            "/workspace/manifests/unseen-capability-matrix.v1-eval-optimization-v4.manifest.json"
        }
        "production-multiturn-release-gate.v3.json" {
            "/workspace/manifests/unseen-capability-matrix.v1-production-release-v4.manifest.json"
        }
        "repair-dependency-gate.v2.json" {
            "/workspace/manifests/unseen-capability-matrix.v1-repair-dependency-v3.manifest.json"
        }
        default {
            throw "a custom Policy requires an explicit frozen Manifest"
        }
    }
}

function Invoke-Runner {
    param(
        [Parameter(Mandatory)] [string[]]$RunnerArgs,
        [string[]]$ContainerEnvironment = @(),
        [int[]]$AllowedExitCodes = @(0)
    )
    $environmentArgs = @()
    foreach ($entry in $ContainerEnvironment) {
        $environmentArgs += @("--env", $entry)
    }
    & docker compose --env-file $mainEnv --env-file $evalEnv `
        -p weknora-agent-eval-platform -f $composeFile `
        run --rm @environmentArgs agent-eval-runner @RunnerArgs |
        ForEach-Object { Write-Host $_ }
    $exitCode = $LASTEXITCODE
    if ($AllowedExitCodes -notcontains $exitCode) {
        throw "eval runner failed with exit code $exitCode"
    }
    return $exitCode
}

function Read-RunnerEnvironment {
    $values = @{}
    if (-not (Test-Path -LiteralPath $runnerEnv)) { return $values }
    Get-Content -LiteralPath $runnerEnv | ForEach-Object {
        if ($_ -match '^([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
            $values[$matches[1]] = $matches[2]
        }
    }
    return $values
}

$repositoryRoot = (& git -C $PSScriptRoot rev-parse --show-toplevel).Trim()
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($repositoryRoot)) {
    throw "failed to resolve repository root"
}
$env:AGENT_EVAL_FRAMEWORK_COMMIT = (
    & git -C $repositoryRoot rev-parse "HEAD:custom/services/agent-eval/weknora_eval"
).Trim()
if ($LASTEXITCODE -ne 0) { throw "failed to resolve eval framework tree identity" }
$env:AGENT_EVAL_FRAMEWORK_SCOPE = "weknora_eval-tree-v1"
$policyHostPath = if ($Policy -match '^/workspace/(.+)$') {
    Join-Path $PSScriptRoot ($matches[1] -replace '/', '\')
} else {
    [System.IO.Path]::GetFullPath($Policy)
}
if (-not (Test-Path -LiteralPath $policyHostPath)) {
    throw "failed to resolve gate policy on host: $policyHostPath"
}
$policyConfig = Get-Content -Raw -LiteralPath $policyHostPath | ConvertFrom-Json
$policyRequiresBaseline = if ($null -eq $policyConfig.require_baseline) {
    $true
} else {
    [bool]$policyConfig.require_baseline
}
$env:AGENT_EVAL_GATE_POLICY_SHA256 = (
    Get-FileHash -LiteralPath $policyHostPath -Algorithm SHA256
).Hash.ToLowerInvariant()
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
        throw "main Docker project '$project' is still running; stop it before isolated eval"
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

if ([string]::IsNullOrWhiteSpace($Run)) {
    $env:AGENT_EVAL_RUNTIME_IMAGE_ID = Get-RunningImageId "weknora-agent-eval-runtime-api-1"
    $env:AGENT_EVAL_GENERAL_AGENT_IMAGE_ID = Get-RunningImageId "weknora-agent-eval-general-agent"

    $runnerValues = Read-RunnerEnvironment
    if (
        -not $runnerValues.ContainsKey("WEKNORA_E2E_TENANT_API_KEY") -or
        [string]::IsNullOrWhiteSpace($runnerValues["WEKNORA_E2E_TENANT_API_KEY"]) -or
        -not $runnerValues.ContainsKey("AGENT_EVAL_SUMMARY_MODEL_ID") -or
        [string]::IsNullOrWhiteSpace($runnerValues["AGENT_EVAL_SUMMARY_MODEL_ID"])
    ) {
        & (Join-Path $PSScriptRoot "prepare-runner-env.ps1") -ProfilePath (
            Join-Path $PSScriptRoot "profiles/unseen-capability-matrix.v1.json"
        )
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare isolated runner.env" }
        $runnerValues = Read-RunnerEnvironment
    }

    $datasetHostPath = if ($Dataset -match '^/workspace/(.+)$') {
        Join-Path $PSScriptRoot ($matches[1] -replace '/', '\')
    } else {
        [System.IO.Path]::GetFullPath($Dataset)
    }
    if (-not (Test-Path -LiteralPath $datasetHostPath)) {
        throw "failed to resolve dataset on host: $datasetHostPath"
    }
    $datasetRows = @(
        Get-Content -LiteralPath $datasetHostPath |
            Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
            ForEach-Object { $_ | ConvertFrom-Json }
    )
    $datasetNeedsNoKBProfiles = @(
        $datasetRows | Where-Object {
            [string]$_.setup.knowledge_selection_mode -eq "none"
        }
    ).Count -gt 0
    if ($datasetNeedsNoKBProfiles) {
        # Re-copy the current production agent configuration on every relevant
        # run. Only the knowledge-selection boundary changes, so stale Eval
        # clones cannot hide a production prompt/configuration update.
        & (Join-Path $PSScriptRoot "prepare-no-kb-agent-profiles.ps1")
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare Eval no-KB agent profiles" }
        $runnerValues = Read-RunnerEnvironment
    }

    $datasetUsesUnseenCorpus = (Split-Path -Leaf $Dataset) -match '^unseen-capability-matrix\.v[0-9]+\.jsonl$'
    if ($datasetUsesUnseenCorpus) {
        & (Join-Path $PSScriptRoot "prepare-unseen-capability-kbs.ps1")
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare unseen capability knowledge bases" }
    }
    $datasetUsesSemanticRoutingCorpus = (Split-Path -Leaf $Dataset) -eq "semantic-routing-regression.v1.jsonl"
    if ($datasetUsesSemanticRoutingCorpus) {
        & (Join-Path $PSScriptRoot "prepare-semantic-routing-regression-kb.ps1")
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare semantic-routing regression knowledge base" }
    }
    $datasetUsesFreshGeneralization = (Split-Path -Leaf $Dataset) -eq "fresh-generalization-regression.v1.jsonl"
    if ($datasetUsesFreshGeneralization) {
        & (Join-Path $PSScriptRoot "prepare-fresh-generalization-kb.ps1")
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare fresh-generalization knowledge base" }
    }
    $datasetUsesPostChangeCanary = (Split-Path -Leaf $Dataset) -eq "post-change-evidence-canary.v1.jsonl"
    if ($datasetUsesPostChangeCanary) {
        & (Join-Path $PSScriptRoot "prepare-fresh-generalization-kb.ps1") `
            -Fixture (Join-Path $PSScriptRoot "fixtures/regression-corpora/lab-sample-handoff.v1.md") `
            -BindingOutput (Join-Path $PSScriptRoot "artifacts/post-change-evidence-canary-kb-binding.v1.json") `
            -EnvironmentKey "AGENT_EVAL_KB_POST_CHANGE_LAB_ID" `
            -KnowledgeBaseName "Eval回归-Helix样本交接规程-v1" `
            -UploadName "lab-sample-handoff.v1.md" `
            -CorpusVersion "helix-lab-handoff-v1"
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare post-change evidence canary knowledge base" }
    }
}

& docker compose --env-file $mainEnv --env-file $evalEnv `
    -p weknora-agent-eval-platform -f $composeFile build agent-eval-runner
if ($LASTEXITCODE -ne 0) { throw "failed to build eval runner image" }

Invoke-Runner -RunnerArgs @("dataset", "validate", "--input", $Dataset) | Out-Null
if ([string]::IsNullOrWhiteSpace($Run)) {
    foreach ($selectedSplit in $Split) {
        $preflight = "/workspace/artifacts/preflight-$timestamp-$selectedSplit.json"
        Invoke-Runner -RunnerArgs @(
            "preflight",
            "--dataset", $Dataset,
            "--manifest", $Manifest,
            "--profiles", $Profiles,
            "--policy", $Policy,
            "--calibration", "/workspace/calibration/judge-multiturn.v1.json",
            "--split", $selectedSplit,
            "--output", $preflight
        ) | Out-Null
        Write-Host "Preflight: $preflight"
    }

    if ($PreflightOnly) {
        Write-Host "READY: no eval sessions or chat requests were created."
        Write-Host "Frozen dataset manifest: $Manifest"
        exit 0
    }

    $runArgs = @(
        "run", "--dataset", $Dataset, "--output", $rawRun,
        "--max-concurrency", [string]$MaxConcurrency,
        "--timeout", [string]$ResponseDeadlineSeconds,
        "--label", "codex-conversation-$timestamp",
        "--profiles", $Profiles
    )
    foreach ($selectedSplit in $Split) { $runArgs += @("--split", $selectedSplit) }
    foreach ($selectedCase in $CaseId) {
        if (-not [string]::IsNullOrWhiteSpace($selectedCase)) {
            $runArgs += @("--case-id", $selectedCase.Trim())
        }
    }
    if ($Split -contains "sealed_holdout") { $runArgs += "--allow-sealed" }
    if ($PublishToLangfuse) {
        $runArgs += @("--langfuse-experiment", "codex-$timestamp")
    }
    $runEnvironment = @(
        "AGENT_EVAL_ASSIST_ENABLED=$($EnableEvalAssistance.IsPresent.ToString().ToLowerInvariant())"
    )
    # Exit 2 means one or more conversations are INVALID. Preserve the complete
    # artifact so Codex can distinguish SUT, transport, and evaluator failures.
    Invoke-Runner -RunnerArgs $runArgs -ContainerEnvironment $runEnvironment `
        -AllowedExitCodes @(0, 2) | Out-Null
} else {
    Write-Host "Reviewing existing immutable run: $rawRun"
}

$reviewExportBase = @("--dataset", $Dataset, "--run", $rawRun)
foreach ($selectedSplit in $Split) { $reviewExportBase += @("--split", $selectedSplit) }
$productionExportArgs = @("codex-review-export") + $reviewExportBase + @(
    "--answer-track", "production_candidate",
    "--output", $productionReviewPacket
)
$assistedExportArgs = @("codex-review-export") + $reviewExportBase + @(
    "--answer-track", "eval_assisted_answer",
    "--output", $assistedReviewPacket
)
Invoke-Runner -RunnerArgs $productionExportArgs | Out-Null
Invoke-Runner -RunnerArgs $assistedExportArgs | Out-Null

$candidate = $rawRun
if ($CodexReview.Count -gt 0) {
    $reviewIndex = 0
    foreach ($reviewDocument in $CodexReview) {
        $reviewIndex++
        $output = if ($reviewIndex -eq $CodexReview.Count) {
            $reviewedRun
        } else {
            "/workspace/artifacts/run-$timestamp-reviewed-$reviewIndex.json"
        }
        Invoke-Runner -RunnerArgs @(
            "codex-review-apply",
            "--dataset", $Dataset,
            "--run", $candidate,
            "--reviews", $reviewDocument,
            "--output", $output
        ) | Out-Null
        $candidate = $output
    }
}

$gateExit = 0
if ($CodexReview.Count -eq 0) {
    Invoke-Runner -RunnerArgs @("report", "--run", $candidate, "--output", $report) | Out-Null
    Write-Warning "WAITING_FOR_CODEX_REVIEW: no quality verdict was issued."
} elseif ([string]::IsNullOrWhiteSpace($Baseline) -and $policyRequiresBaseline) {
    Invoke-Runner -RunnerArgs @("report", "--run", $candidate, "--output", $report) | Out-Null
    Write-Warning "Codex reviews were attached, but no paired reviewed baseline was supplied; no gate verdict was issued."
} else {
    $gateArgs = @(
        "gate", "--dataset", $Dataset, "--candidate", $candidate,
        "--policy", $Policy,
        "--output", $gateResult
    )
    if (-not [string]::IsNullOrWhiteSpace($Baseline)) {
        $gateArgs += @("--baseline", $Baseline)
    }
    $gateExit = Invoke-Runner -RunnerArgs $gateArgs -AllowedExitCodes @(0, 1, 2)
    Invoke-Runner -RunnerArgs @(
        "report", "--run", $candidate, "--gate", $gateResult, "--output", $report
    ) | Out-Null
}

Write-Host "Candidate: $candidate"
Write-Host "Production Codex review packet: $productionReviewPacket"
Write-Host "Eval-assisted Codex review packet: $assistedReviewPacket"
Write-Host "Dataset manifest: $Manifest"
Write-Host "Report: $report"
if ($CodexReview.Count -gt 0 -and (-not $policyRequiresBaseline -or -not [string]::IsNullOrWhiteSpace($Baseline))) {
    Write-Host "Gate: $gateResult (exit=$gateExit)"
}
exit $gateExit
