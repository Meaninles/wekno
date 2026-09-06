[CmdletBinding()]
param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$RunnerEnv = (Join-Path $PSScriptRoot "runner.env"),
    [string]$ManifestPath = (Join-Path $PSScriptRoot "artifacts/production-replay-v2/kb-manifests/source-manifest.json"),
    [string]$StatePath = (Join-Path $PSScriptRoot "artifacts/production-replay-v2/kb-manifests/import-state.json"),
    [string]$ReportPath = (Join-Path $PSScriptRoot "artifacts/production-replay-v2/kb-manifests/local-verification.json"),
    [switch]$RequireTerminal
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Read-EnvFile {
    param([Parameter(Mandatory)] [string]$Path)
    $values = @{}
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ($line -match '^(?<key>[A-Za-z_][A-Za-z0-9_]*)=(?<value>.*)$') {
            $values[$Matches.key] = $Matches.value
        }
    }
    return $values
}

function Invoke-LocalApi {
    param([Parameter(Mandatory)] [string]$Path)
    return Invoke-RestMethod -Uri ($BaseUrl.TrimEnd("/") + $Path) -Headers $script:Headers -TimeoutSec 120
}

function Get-AllKnowledges {
    param([Parameter(Mandatory)] [string]$KnowledgeBaseID)
    $items = [Collections.Generic.List[object]]::new()
    $page = 1
    do {
        $response = Invoke-LocalApi -Path "/api/v1/knowledge-bases/$KnowledgeBaseID/knowledge?page=$page&page_size=500"
        foreach ($item in @($response.data)) {
            $items.Add($item)
        }
        $total = [int]$response.total
        $page++
    } while ($items.Count -lt $total)
    return @($items)
}

function Get-StringSha256 {
    param([Parameter(Mandatory)] [string]$Value)
    $hasher = [Security.Cryptography.SHA256]::Create()
    try {
        return [Convert]::ToHexString($hasher.ComputeHash([Text.Encoding]::UTF8.GetBytes($Value))).ToLowerInvariant()
    } finally {
        $hasher.Dispose()
    }
}

function Get-NormalizedModelName {
    param([string]$Name)
    if ($null -eq $Name) { return "" }
    return (($Name.ToLowerInvariant() -replace 'int8','') -replace '[^a-z0-9]+','')
}

function Get-ExpectedModelID {
    param(
        [Parameter(Mandatory)] [object]$SourceModel,
        [Parameter(Mandatory)] [ValidateSet("embedding", "summary", "derivative", "vlm", "asr")] [string]$Purpose
    )
    $name = Get-NormalizedModelName -Name ([string]$SourceModel.name)
    switch ($Purpose) {
        "embedding" { "prod-qwen3-embedding-8b" }
        "summary" {
            if ($name -match 'deepseekv4flash') { "prod-deepseek-v4-flash-int8-chat" }
            elseif ($name -match 'qwen3627b') { "prod-qwen36-27b-chat" }
            else { throw "unsupported production summary model: $($SourceModel.name)" }
        }
        "derivative" { "prod-qwen36-35b-derivative" }
        "vlm" { "prod-qwen3-vl-32b-vlm" }
        "asr" { "prod-qwen25-omni-7b-asr" }
    }
}

function Add-Mismatch {
    param(
        [Collections.Generic.List[object]]$List,
        [Parameter(Mandatory)] [string]$Scope,
        [Parameter(Mandatory)] [string]$Field,
        [object]$Expected,
        [object]$Actual
    )
    $List.Add([ordered]@{ scope=$Scope; field=$Field; expected=$Expected; actual=$Actual })
}

function Assert-Equal {
    param(
        [Collections.Generic.List[object]]$List,
        [Parameter(Mandatory)] [string]$Scope,
        [Parameter(Mandatory)] [string]$Field,
        [object]$Expected,
        [object]$Actual
    )
    if ([string]$Expected -cne [string]$Actual) {
        Add-Mismatch -List $List -Scope $Scope -Field $Field -Expected $Expected -Actual $Actual
    }
}

if (-not (Test-Path -LiteralPath $RunnerEnv)) { throw "runner.env is missing: $RunnerEnv" }
if (-not (Test-Path -LiteralPath $ManifestPath)) { throw "source manifest is missing: $ManifestPath" }
if (-not (Test-Path -LiteralPath $StatePath)) { throw "import state is missing: $StatePath" }

$settings = Read-EnvFile -Path $RunnerEnv
$apiKey = [string]$settings["WEKNORA_E2E_TENANT_API_KEY"]
if ([string]::IsNullOrWhiteSpace($apiKey)) { throw "runner.env does not contain WEKNORA_E2E_TENANT_API_KEY" }
$script:Headers = @{ "X-API-Key" = $apiKey }
$manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json -Depth 100
$state = Get-Content -LiteralPath $StatePath -Raw | ConvertFrom-Json -Depth 100 -AsHashtable
$manifestRoot = Split-Path -Parent (Split-Path -Parent ([IO.Path]::GetFullPath($ManifestPath)))
$mismatches = [Collections.Generic.List[object]]::new()
$processingFailures = [Collections.Generic.List[object]]::new()
$kbReports = [Collections.Generic.List[object]]::new()
$seenLocalIDs = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
$totals = [ordered]@{
    knowledge_bases = 0
    documents = 0
    files = 0
    manual = 0
    core_ready = 0
    parse_completed = 0
    enrichment_terminal = 0
    enrichment_degraded = 0
    wiki_none = 0
}

$capabilities = Invoke-LocalApi -Path "/api/v1/custom/agent-eval/capabilities"
Assert-Equal -List $mismatches -Scope "runtime" -Field "mode" -Expected "production" -Actual $capabilities.data.mode
Assert-Equal -List $mismatches -Scope "runtime" -Field "recorder_enabled" -Expected $false -Actual $capabilities.data.recorder_enabled

foreach ($sourceKB in @($manifest.knowledge_bases)) {
    $slug = [string]$sourceKB.slug
    if (-not $state.knowledge_bases.Contains($slug)) {
        Add-Mismatch -List $mismatches -Scope $slug -Field "import_binding" -Expected "present" -Actual "missing"
        continue
    }
    $binding = $state.knowledge_bases[$slug]
    $localKBID = [string]$binding.local_knowledge_base_id
    $localKB = (Invoke-LocalApi -Path "/api/v1/knowledge-bases/$localKBID").data
    $items = @(Get-AllKnowledges -KnowledgeBaseID $localKBID)
    $itemsByID = @{}
    foreach ($item in $items) {
        $itemsByID[[string]$item.id] = $item
        if (-not $seenLocalIDs.Add([string]$item.id)) {
            Add-Mismatch -List $mismatches -Scope $slug -Field "duplicate_local_knowledge_id" -Expected "unique" -Actual ([string]$item.id)
        }
    }

    $expectedEmbedding = Get-ExpectedModelID -SourceModel $sourceKB.embedding_model -Purpose "embedding"
    $expectedSummary = Get-ExpectedModelID -SourceModel $sourceKB.summary_model -Purpose "summary"
    $expectedDerivative = Get-ExpectedModelID -SourceModel $sourceKB.derivative_model -Purpose "derivative"
    $expectedVLM = if ([bool]$sourceKB.vlm_config.enabled) {
        $sourceVLM = @($manifest.model_catalog | Where-Object source_id -eq ([string]$sourceKB.vlm_config.model_id))[0]
        Get-ExpectedModelID -SourceModel $sourceVLM -Purpose "vlm"
    } else { "" }
    $expectedASR = if ([bool]$sourceKB.asr_config.enabled) {
        $sourceASR = @($manifest.model_catalog | Where-Object source_id -eq ([string]$sourceKB.asr_config.model_id))[0]
        Get-ExpectedModelID -SourceModel $sourceASR -Purpose "asr"
    } else { "" }

    $scope = "kb:$slug"
    Assert-Equal -List $mismatches -Scope $scope -Field "name" -Expected $sourceKB.name -Actual $localKB.name
    Assert-Equal -List $mismatches -Scope $scope -Field "description" -Expected $sourceKB.description -Actual $localKB.description
    Assert-Equal -List $mismatches -Scope $scope -Field "type" -Expected $sourceKB.type -Actual $localKB.type
    Assert-Equal -List $mismatches -Scope $scope -Field "document_count" -Expected @($sourceKB.documents).Count -Actual $items.Count
    Assert-Equal -List $mismatches -Scope $scope -Field "embedding_model_id" -Expected $expectedEmbedding -Actual $localKB.embedding_model_id
    Assert-Equal -List $mismatches -Scope $scope -Field "summary_model_id" -Expected $expectedSummary -Actual $localKB.summary_model_id
    Assert-Equal -List $mismatches -Scope $scope -Field "derivative_model_id" -Expected $expectedDerivative -Actual $localKB.derivative_model_id
    Assert-Equal -List $mismatches -Scope $scope -Field "storage_provider" -Expected "minio" -Actual $localKB.storage_provider_config.provider
    Assert-Equal -List $mismatches -Scope $scope -Field "vector_store_engine_type" -Expected "postgres" -Actual $localKB.vector_store_engine_type
    Assert-Equal -List $mismatches -Scope $scope -Field "vector_enabled" -Expected $true -Actual $localKB.indexing_strategy.vector_enabled
    Assert-Equal -List $mismatches -Scope $scope -Field "keyword_enabled" -Expected $true -Actual $localKB.indexing_strategy.keyword_enabled
    Assert-Equal -List $mismatches -Scope $scope -Field "graph_enabled" -Expected $false -Actual $localKB.indexing_strategy.graph_enabled
    Assert-Equal -List $mismatches -Scope $scope -Field "wiki_enabled" -Expected $false -Actual $localKB.indexing_strategy.wiki_enabled
    Assert-Equal -List $mismatches -Scope $scope -Field "wiki_capability" -Expected $false -Actual $localKB.capabilities.wiki
    Assert-Equal -List $mismatches -Scope $scope -Field "vlm_enabled" -Expected ([bool]$sourceKB.vlm_config.enabled) -Actual $localKB.vlm_config.enabled
    Assert-Equal -List $mismatches -Scope $scope -Field "vlm_model_id" -Expected $expectedVLM -Actual $localKB.vlm_config.model_id
    Assert-Equal -List $mismatches -Scope $scope -Field "asr_enabled" -Expected ([bool]$sourceKB.asr_config.enabled) -Actual $localKB.asr_config.enabled
    Assert-Equal -List $mismatches -Scope $scope -Field "asr_model_id" -Expected $expectedASR -Actual $localKB.asr_config.model_id
    Assert-Equal -List $mismatches -Scope $scope -Field "question_generation_enabled" -Expected ([bool]$sourceKB.question_generation_config.enabled) -Actual $localKB.question_generation_config.enabled
    Assert-Equal -List $mismatches -Scope $scope -Field "question_count" -Expected ([int]$sourceKB.question_generation_config.question_count) -Actual $localKB.question_generation_config.question_count
    foreach ($field in @("strategy", "chunk_size", "chunk_overlap", "child_chunk_size", "parent_chunk_size", "enable_parent_child")) {
        Assert-Equal -List $mismatches -Scope $scope -Field "chunking_config.$field" -Expected $sourceKB.chunking_config.$field -Actual $localKB.chunking_config.$field
    }
    Assert-Equal -List $mismatches -Scope $scope -Field "chunking_config.separators" -Expected (@($sourceKB.chunking_config.separators) -join "`u{241f}") -Actual (@($localKB.chunking_config.separators) -join "`u{241f}")

    $status = [ordered]@{ core_ready=0; parse_completed=0; enrichment_terminal=0; enrichment_degraded=0; wiki_none=0 }
    foreach ($sourceDocument in @($sourceKB.documents)) {
        $sourceID = [string]$sourceDocument.source_knowledge_id
        if (-not $binding.documents.Contains($sourceID)) {
            Add-Mismatch -List $mismatches -Scope "document:$sourceID" -Field "import_binding" -Expected "present" -Actual "missing"
            continue
        }
        $localID = [string]$binding.documents[$sourceID].local_knowledge_id
        if (-not $itemsByID.ContainsKey($localID)) {
            Add-Mismatch -List $mismatches -Scope "document:$sourceID" -Field "local_row" -Expected $localID -Actual "missing"
            continue
        }
        $local = $itemsByID[$localID]
        $docScope = "document:$sourceID"
        Assert-Equal -List $mismatches -Scope $docScope -Field "type" -Expected $sourceDocument.type -Actual $local.type
        Assert-Equal -List $mismatches -Scope $docScope -Field "title" -Expected $sourceDocument.title -Actual $local.title
        Assert-Equal -List $mismatches -Scope $docScope -Field "channel" -Expected "web" -Actual $local.channel
        Assert-Equal -List $mismatches -Scope $docScope -Field "enable_status" -Expected "enabled" -Actual $local.enable_status
        Assert-Equal -List $mismatches -Scope $docScope -Field "embedding_model_id" -Expected $expectedEmbedding -Actual $local.embedding_model_id
        if ([string]$sourceDocument.type -eq "file") {
            $totals.files++
            Assert-Equal -List $mismatches -Scope $docScope -Field "file_name" -Expected $sourceDocument.file_name -Actual $local.file_name
            Assert-Equal -List $mismatches -Scope $docScope -Field "file_type" -Expected $sourceDocument.file_type -Actual $local.file_type
            Assert-Equal -List $mismatches -Scope $docScope -Field "file_size" -Expected ([long]$sourceDocument.file_size) -Actual $local.file_size
            Assert-Equal -List $mismatches -Scope $docScope -Field "file_hash" -Expected ([string]$sourceDocument.production_file_hash).ToLowerInvariant() -Actual ([string]$local.file_hash).ToLowerInvariant()
            $capturedPath = [IO.Path]::GetFullPath((Join-Path $manifestRoot ([string]$sourceDocument.relative_path)))
            if (-not (Test-Path -LiteralPath $capturedPath)) {
                Add-Mismatch -List $mismatches -Scope $docScope -Field "captured_file" -Expected "present" -Actual "missing"
            } else {
                Assert-Equal -List $mismatches -Scope $docScope -Field "captured_size" -Expected ([long]$sourceDocument.file_size) -Actual (Get-Item -LiteralPath $capturedPath).Length
                Assert-Equal -List $mismatches -Scope $docScope -Field "captured_sha256" -Expected ([string]$sourceDocument.local_sha256).ToLowerInvariant() -Actual (Get-FileHash -LiteralPath $capturedPath -Algorithm SHA256).Hash.ToLowerInvariant()
            }
        } else {
            $totals.manual++
            Assert-Equal -List $mismatches -Scope $docScope -Field "manual_content_sha256" -Expected ([string]$sourceDocument.local_sha256).ToLowerInvariant() -Actual (Get-StringSha256 -Value ([string]$local.metadata.content))
        }
        if ([string]$local.core_status -eq "ready") { $status.core_ready++; $totals.core_ready++ }
        if ([string]$local.parse_status -eq "completed") { $status.parse_completed++; $totals.parse_completed++ }
        if ([string]$local.enrichment_status -in @("completed", "none", "degraded")) { $status.enrichment_terminal++; $totals.enrichment_terminal++ }
        if ([string]$local.enrichment_status -eq "degraded") { $status.enrichment_degraded++; $totals.enrichment_degraded++ }
        if ([string]$local.wiki_status -eq "none") { $status.wiki_none++; $totals.wiki_none++ }
        if ([string]$local.core_status -eq "failed" -or [string]$local.parse_status -eq "failed" -or [string]$local.enrichment_status -eq "failed") {
            $processingFailures.Add([ordered]@{ source_knowledge_id=$sourceID; local_knowledge_id=$localID; core_status=$local.core_status; parse_status=$local.parse_status; enrichment_status=$local.enrichment_status; error_message=$local.error_message })
        }
    }
    $totals.knowledge_bases++
    $totals.documents += $items.Count
    $kbReports.Add([ordered]@{ slug=$slug; name=$sourceKB.name; source_knowledge_base_id=$sourceKB.source_knowledge_base_id; local_knowledge_base_id=$localKBID; expected_documents=@($sourceKB.documents).Count; actual_documents=$items.Count; status=$status })
}

$report = [ordered]@{
    schema_version = 1
    generated_at = [DateTimeOffset]::UtcNow.ToString("o")
    source_environment = "weknora-prod"
    target_environment = "local-production-mode"
    api_base_url = $BaseUrl
    require_terminal = [bool]$RequireTerminal
    source_manifest_sha256 = (Get-FileHash -LiteralPath $ManifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
    allowed_environment_differences = @("object storage: OBS -> MinIO", "retrieval engine: local PostgreSQL", "model IDs: production names mapped to local production-profile IDs", "Wiki forcibly disabled")
    totals = $totals
    static_mismatch_count = $mismatches.Count
    processing_failure_count = $processingFailures.Count
    terminal = ($totals.documents -eq 682 -and $totals.core_ready -eq 682 -and $totals.parse_completed -eq 682 -and $totals.enrichment_terminal -eq 682 -and $totals.wiki_none -eq 682)
    mismatches = @($mismatches)
    processing_failures = @($processingFailures)
    knowledge_bases = @($kbReports)
}

$fullReportPath = [IO.Path]::GetFullPath($ReportPath)
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $fullReportPath) | Out-Null
[IO.File]::WriteAllText($fullReportPath, ($report | ConvertTo-Json -Depth 20), [Text.UTF8Encoding]::new($false))
Write-Host "Verification: KB=$($totals.knowledge_bases)/5 documents=$($totals.documents)/682 files=$($totals.files)/680 manual=$($totals.manual)/2 core=$($totals.core_ready)/682 parse=$($totals.parse_completed)/682 enrichment-terminal=$($totals.enrichment_terminal)/682 degraded=$($totals.enrichment_degraded) wiki-none=$($totals.wiki_none)/682 mismatches=$($mismatches.Count) failures=$($processingFailures.Count)"
Write-Host "Verification report: $fullReportPath"

if ($mismatches.Count -gt 0 -or $processingFailures.Count -gt 0) {
    throw "local production-equivalent KB verification found mismatches or processing failures"
}
if ($RequireTerminal -and -not $report.terminal) {
    throw "local production-equivalent KB processing is not terminal yet"
}
