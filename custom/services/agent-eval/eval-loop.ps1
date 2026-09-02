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

function Set-RunnerEnvironmentValues {
    param(
        [Parameter(Mandatory)] [Collections.IDictionary]$Updates
    )
    if (-not (Test-Path -LiteralPath $runnerEnv)) {
        throw "runner.env is missing: $runnerEnv"
    }
    $lines = [Collections.Generic.List[string]]::new()
    $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($line in Get-Content -LiteralPath $runnerEnv) {
        if ($line -match '^(?<key>[A-Za-z_][A-Za-z0-9_]*)=') {
            $key = $Matches.key
            if ($Updates.Contains($key)) {
                $lines.Add("$key=$($Updates[$key])")
                [void]$seen.Add($key)
                continue
            }
        }
        $lines.Add($line)
    }
    foreach ($key in $Updates.Keys) {
        if (-not $seen.Contains([string]$key)) {
            $lines.Add("$key=$($Updates[$key])")
        }
    }
    [IO.File]::WriteAllText(
        [IO.Path]::GetFullPath($runnerEnv),
        (($lines -join [Environment]::NewLine) + [Environment]::NewLine),
        [Text.UTF8Encoding]::new($false)
    )
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
        $profileHostPath = if ($Profiles -match '^/workspace/(.+)$') {
            Join-Path $PSScriptRoot ($matches[1] -replace '/', '\')
        } else {
            [System.IO.Path]::GetFullPath($Profiles)
        }
        & (Join-Path $PSScriptRoot "prepare-runner-env.ps1") -ProfilePath (
            $profileHostPath
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
    $datasetText = [IO.File]::ReadAllText($datasetHostPath, [Text.Encoding]::UTF8)
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

    # Corpus preparation follows the frozen dataset's explicit environment
    # references, not its filename. A versioned matrix may intentionally mix
    # multiple independent KB families.
    $datasetUsesUnseenCorpus = $datasetText -match '\$\{AGENT_EVAL_KB_UNSEEN_[A-Z0-9_]+\}'
    if ($datasetUsesUnseenCorpus) {
        & (Join-Path $PSScriptRoot "prepare-unseen-capability-kbs.ps1")
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare unseen capability knowledge bases" }
    }
    $datasetUsesSemanticRoutingCorpus = $datasetText.Contains(
        '${AGENT_EVAL_KB_SEMANTIC_ROUTING_FACILITIES_ID}'
    )
    if ($datasetUsesSemanticRoutingCorpus) {
        & (Join-Path $PSScriptRoot "prepare-semantic-routing-regression-kb.ps1")
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare semantic-routing regression knowledge base" }
    }
    $datasetUsesFreshGeneralization = $datasetText.Contains(
        '${AGENT_EVAL_KB_FRESH_GENERALIZATION_MEDIA_ID}'
    )
    if ($datasetUsesFreshGeneralization) {
        & (Join-Path $PSScriptRoot "prepare-fresh-generalization-kb.ps1")
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare fresh-generalization knowledge base" }
    }
    $datasetUsesPostChangeCanary = $datasetText.Contains(
        '${AGENT_EVAL_KB_POST_CHANGE_LAB_ID}'
    )
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
    $datasetUsesRagPrimaryHR = $datasetText.Contains(
        '${AGENT_EVAL_KB_RAG_PRIMARY_HR_ID}'
    )
    if ($datasetUsesRagPrimaryHR) {
        & (Join-Path $PSScriptRoot "prepare-fresh-generalization-kb.ps1") `
            -Fixture (Join-Path $PSScriptRoot "fixtures/regression-corpora/remote-onboarding-handbook.v1.md") `
            -BindingOutput (Join-Path $PSScriptRoot "artifacts/rag-primary-hr-kb-binding.v1.json") `
            -EnvironmentKey "AGENT_EVAL_KB_RAG_PRIMARY_HR_ID" `
            -KnowledgeBaseName "Eval回归-Alder远程入职手册-v1" `
            -UploadName "remote-onboarding-handbook.v1.md" `
            -CorpusVersion "alder-remote-onboarding-v1"
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare RAG-primary HR knowledge base" }
    }
    $datasetUsesRagPrimarySupport = $datasetText.Contains(
        '${AGENT_EVAL_KB_RAG_PRIMARY_SUPPORT_ID}'
    )
    if ($datasetUsesRagPrimarySupport) {
        & (Join-Path $PSScriptRoot "prepare-fresh-generalization-kb.ps1") `
            -Fixture (Join-Path $PSScriptRoot "fixtures/regression-corpora/customer-device-support-handbook.v1.md") `
            -BindingOutput (Join-Path $PSScriptRoot "artifacts/rag-primary-support-kb-binding.v1.json") `
            -EnvironmentKey "AGENT_EVAL_KB_RAG_PRIMARY_SUPPORT_ID" `
            -KnowledgeBaseName "Eval回归-Northstar设备支持手册-v1" `
            -UploadName "customer-device-support-handbook.v1.md" `
            -CorpusVersion "northstar-customer-device-support-v1"
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare RAG-primary customer-support knowledge base" }
    }
    $datasetUsesRagPrimaryEditorial = $datasetText.Contains(
        '${AGENT_EVAL_KB_RAG_PRIMARY_EDITORIAL_ID}'
    )
    if ($datasetUsesRagPrimaryEditorial) {
        & (Join-Path $PSScriptRoot "prepare-fresh-generalization-kb.ps1") `
            -Fixture (Join-Path $PSScriptRoot "fixtures/regression-corpora/editorial-release-handbook.v1.md") `
            -BindingOutput (Join-Path $PSScriptRoot "artifacts/rag-primary-editorial-kb-binding.v1.json") `
            -EnvironmentKey "AGENT_EVAL_KB_RAG_PRIMARY_EDITORIAL_ID" `
            -KnowledgeBaseName "Eval回归-Juniper编辑发布手册-v1" `
            -UploadName "editorial-release-handbook.v1.md" `
            -CorpusVersion "juniper-editorial-release-v1"
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare RAG-primary editorial knowledge base" }
    }
    $datasetUsesRagPrimaryPrivacy = $datasetText.Contains(
        '${AGENT_EVAL_KB_RAG_PRIMARY_PRIVACY_ID}'
    )
    if ($datasetUsesRagPrimaryPrivacy) {
        & (Join-Path $PSScriptRoot "prepare-fresh-generalization-kb.ps1") `
            -Fixture (Join-Path $PSScriptRoot "fixtures/regression-corpora/privacy-request-handbook.v1.md") `
            -BindingOutput (Join-Path $PSScriptRoot "artifacts/rag-primary-privacy-kb-binding.v1.json") `
            -EnvironmentKey "AGENT_EVAL_KB_RAG_PRIMARY_PRIVACY_ID" `
            -KnowledgeBaseName "Eval回归-Meridian隐私请求手册-v1" `
            -UploadName "privacy-request-handbook.v1.md" `
            -CorpusVersion "meridian-privacy-request-v1"
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare RAG-primary privacy knowledge base" }
    }
    $datasetUsesRagPrimaryWater = $datasetText.Contains(
        '${AGENT_EVAL_KB_RAG_PRIMARY_WATER_ID}'
    )
    if ($datasetUsesRagPrimaryWater) {
        & (Join-Path $PSScriptRoot "prepare-fresh-generalization-kb.ps1") `
            -Fixture (Join-Path $PSScriptRoot "fixtures/regression-corpora/water-quality-incident-guide.v1.md") `
            -BindingOutput (Join-Path $PSScriptRoot "artifacts/rag-primary-water-kb-binding.v1.json") `
            -EnvironmentKey "AGENT_EVAL_KB_RAG_PRIMARY_WATER_ID" `
            -KnowledgeBaseName "Eval回归-Rivermark水质事件指南-v1" `
            -UploadName "water-quality-incident-guide.v1.md" `
            -CorpusVersion "rivermark-water-quality-incident-v1"
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare RAG-primary water-quality knowledge base" }
    }
    $datasetUsesPostTerminalQuality = $datasetText.Contains(
        '${AGENT_EVAL_KB_POST_TERMINAL_QUALITY_ID}'
    )
    if ($datasetUsesPostTerminalQuality) {
        & (Join-Path $PSScriptRoot "prepare-fresh-generalization-kb.ps1") `
            -Fixture (Join-Path $PSScriptRoot "fixtures/regression-corpora/quality-batch-release-manual.v1.md") `
            -BindingOutput (Join-Path $PSScriptRoot "artifacts/post-terminal-quality-kb-binding.v1.json") `
            -EnvironmentKey "AGENT_EVAL_KB_POST_TERMINAL_QUALITY_ID" `
            -KnowledgeBaseName "Eval回归-Orion批次放行手册-v1" `
            -UploadName "quality-batch-release-manual.v1.md" `
            -CorpusVersion "orion-quality-batch-release-v1"
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare post-terminal quality knowledge base" }
    }
    $datasetUsesReleaseCandidateArchive = $datasetText.Contains(
        '${AGENT_EVAL_KB_RELEASE_CANDIDATE_ARCHIVE_ID}'
    )
    if ($datasetUsesReleaseCandidateArchive) {
        & (Join-Path $PSScriptRoot "prepare-fresh-generalization-kb.ps1") `
            -Fixture (Join-Path $PSScriptRoot "fixtures/regression-corpora/digital-archive-ingest-guide.v1.md") `
            -BindingOutput (Join-Path $PSScriptRoot "artifacts/release-candidate-archive-kb-binding.v1.json") `
            -EnvironmentKey "AGENT_EVAL_KB_RELEASE_CANDIDATE_ARCHIVE_ID" `
            -KnowledgeBaseName "Eval回归-Solace数字档案入库指南-v1" `
            -UploadName "digital-archive-ingest-guide.v1.md" `
            -CorpusVersion "solace-digital-archive-ingest-v1"
        if ($LASTEXITCODE -ne 0) { throw "failed to prepare release-candidate archive knowledge base" }
    }

    # Bind the execution identity to every KB selected by this dataset. Each
    # individual preparation script records its own binding; this aggregate
    # prevents a mixed-corpus run from being identified only by the last one.
    $runnerValues = Read-RunnerEnvironment
    $datasetKbEnvironmentKeys = @(
        [regex]::Matches(
            $datasetText,
            '\$\{(?<name>AGENT_EVAL_KB_[A-Z0-9_]+)\}'
        ) |
            ForEach-Object { $_.Groups['name'].Value } |
            Sort-Object -Unique
    )
    if ($datasetKbEnvironmentKeys.Count -gt 0) {
        $bindingLines = [Collections.Generic.List[string]]::new()
        foreach ($key in $datasetKbEnvironmentKeys) {
            if (
                -not $runnerValues.ContainsKey($key) -or
                [string]::IsNullOrWhiteSpace([string]$runnerValues[$key])
            ) {
                throw "dataset KB binding is unresolved: $key"
            }
            $bindingLines.Add("$key=$($runnerValues[$key])")
        }
        $bindingArtifactPaths = [Collections.Generic.List[string]]::new()
        if ($datasetUsesUnseenCorpus) {
            $bindingArtifactPaths.Add(
                (Join-Path $PSScriptRoot "artifacts/unseen-capability-kb-bindings.v1.json")
            )
        }
        if ($datasetUsesSemanticRoutingCorpus) {
            $bindingArtifactPaths.Add(
                (Join-Path $PSScriptRoot "artifacts/semantic-routing-regression-kb-binding.v1.json")
            )
        }
        if ($datasetUsesFreshGeneralization) {
            $bindingArtifactPaths.Add(
                (Join-Path $PSScriptRoot "artifacts/fresh-generalization-kb-binding.v1.json")
            )
        }
        if ($datasetUsesPostChangeCanary) {
            $bindingArtifactPaths.Add(
                (Join-Path $PSScriptRoot "artifacts/post-change-evidence-canary-kb-binding.v1.json")
            )
        }
        if ($datasetUsesRagPrimaryHR) {
            $bindingArtifactPaths.Add(
                (Join-Path $PSScriptRoot "artifacts/rag-primary-hr-kb-binding.v1.json")
            )
        }
        if ($datasetUsesRagPrimarySupport) {
            $bindingArtifactPaths.Add(
                (Join-Path $PSScriptRoot "artifacts/rag-primary-support-kb-binding.v1.json")
            )
        }
        if ($datasetUsesRagPrimaryEditorial) {
            $bindingArtifactPaths.Add(
                (Join-Path $PSScriptRoot "artifacts/rag-primary-editorial-kb-binding.v1.json")
            )
        }
        if ($datasetUsesRagPrimaryPrivacy) {
            $bindingArtifactPaths.Add(
                (Join-Path $PSScriptRoot "artifacts/rag-primary-privacy-kb-binding.v1.json")
            )
        }
        if ($datasetUsesRagPrimaryWater) {
            $bindingArtifactPaths.Add(
                (Join-Path $PSScriptRoot "artifacts/rag-primary-water-kb-binding.v1.json")
            )
        }
        if ($datasetUsesReleaseCandidateArchive) {
            $bindingArtifactPaths.Add(
                (Join-Path $PSScriptRoot "artifacts/release-candidate-archive-kb-binding.v1.json")
            )
        }
        foreach ($bindingArtifactPath in $bindingArtifactPaths) {
            if (-not (Test-Path -LiteralPath $bindingArtifactPath)) {
                throw "prepared KB binding artifact is missing: $bindingArtifactPath"
            }
            $bindingArtifact = Get-Content -Raw -LiteralPath $bindingArtifactPath |
                ConvertFrom-Json
            $identityProperty = $bindingArtifact.PSObject.Properties[
                'binding_identity_sha256'
            ]
            $stableBindingIdentity = if ($null -eq $identityProperty) {
                ""
            } else {
                [string]$identityProperty.Value
            }
            if ([string]::IsNullOrWhiteSpace($stableBindingIdentity)) {
                $bindingEnvironment = [string]$bindingArtifact.environment_variable
                $bindingKnowledgeBaseID = [string]$bindingArtifact.knowledge_base_id
                $bindingSourceHash = [string]$bindingArtifact.source_sha256
                if (
                    [string]::IsNullOrWhiteSpace($bindingEnvironment) -or
                    [string]::IsNullOrWhiteSpace($bindingKnowledgeBaseID) -or
                    [string]::IsNullOrWhiteSpace($bindingSourceHash)
                ) {
                    throw "KB binding artifact lacks a stable source identity: $bindingArtifactPath"
                }
                $stableBindingIdentity = (
                    "$bindingEnvironment|$bindingKnowledgeBaseID|$bindingSourceHash"
                )
            }
            $bindingLines.Add(
                "artifact:$([IO.Path]::GetFileName($bindingArtifactPath))=$stableBindingIdentity"
            )
        }
        $hasher = [Security.Cryptography.SHA256]::Create()
        try {
            $combinedBindingHash = [Convert]::ToHexString(
                $hasher.ComputeHash(
                    [Text.Encoding]::UTF8.GetBytes($bindingLines -join "`n")
                )
            ).ToLowerInvariant()
        } finally {
            $hasher.Dispose()
        }
        $declaredCorpusVersions = @(
            $datasetRows |
                ForEach-Object { [string]$_.corpus_version } |
                Where-Object {
                    -not [string]::IsNullOrWhiteSpace($_) -and
                    $_ -notmatch '^\$\{'
                } |
                Sort-Object -Unique
        )
        $aggregateUpdates = [ordered]@{
            AGENT_EVAL_KB_BINDINGS_SHA256 = $combinedBindingHash
        }
        if ($declaredCorpusVersions.Count -eq 1) {
            $aggregateUpdates['AGENT_EVAL_CORPUS_VERSION'] = $declaredCorpusVersions[0]
        } elseif ($declaredCorpusVersions.Count -gt 1) {
            throw "dataset declares multiple corpus versions; use one aggregate corpus identity"
        }
        Set-RunnerEnvironmentValues -Updates $aggregateUpdates
        $runnerValues = Read-RunnerEnvironment
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
