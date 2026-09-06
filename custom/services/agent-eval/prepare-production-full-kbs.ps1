[CmdletBinding()]
param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$RunnerEnv = (Join-Path $PSScriptRoot "runner.env"),
    [string]$ManifestPath = (Join-Path $PSScriptRoot "artifacts/production-replay-v2/kb-manifests/source-manifest.json"),
    [string]$StatePath = (Join-Path $PSScriptRoot "artifacts/production-replay-v2/kb-manifests/import-state.json"),
    [string]$BindingOutput = (Join-Path $PSScriptRoot "artifacts/production-replay-v2/kb-manifests/local-bindings.json"),
    [ValidateRange(1, 12)]
    [int]$UploadConcurrency = 6,
    [ValidateRange(5, 100)]
    [int]$UploadBatchSize = 24,
    [ValidateRange(600, 43200)]
    [int]$ParseTimeoutSeconds = 28800,
    [ValidateRange(5, 120)]
    [int]$PollSeconds = 20,
    # The normal import path targets a production-equivalent runtime.  An
    # explicit opt-in is also useful when rebuilding the same KBs in the
    # isolated Eval runtime; it relaxes only this safety assertion and does
    # not change any KB configuration or import contents.
    [switch]$AllowEvalRuntime
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
    param(
        [Parameter(Mandatory)] [ValidateSet("GET", "POST")] [string]$Method,
        [Parameter(Mandatory)] [string]$Path,
        [object]$Body,
        [int]$TimeoutSeconds = 60
    )
    $arguments = @{
        Uri = $BaseUrl.TrimEnd("/") + $Path
        Method = $Method
        Headers = $script:Headers
        TimeoutSec = $TimeoutSeconds
    }
    if ($PSBoundParameters.ContainsKey("Body")) {
        $arguments.ContentType = "application/json; charset=utf-8"
        $arguments.Body = $Body | ConvertTo-Json -Depth 100 -Compress
    }
    return Invoke-RestMethod @arguments
}

function Get-AllKnowledgeBases {
    return @((Invoke-LocalApi -Method GET -Path "/api/v1/knowledge-bases?page=1&page_size=500").data)
}

function Get-AllKnowledges {
    param([Parameter(Mandatory)] [string]$KnowledgeBaseID)
    $items = [Collections.Generic.List[object]]::new()
    $page = 1
    do {
        $response = Invoke-LocalApi -Method GET -Path "/api/v1/knowledge-bases/$KnowledgeBaseID/knowledge?page=$page&page_size=500"
        foreach ($item in @($response.data)) {
            $items.Add($item)
        }
        $total = [int]$response.total
        $page++
    } while ($items.Count -lt $total)
    return @($items)
}

function Save-JsonAtomic {
    param(
        [Parameter(Mandatory)] [string]$Path,
        [Parameter(Mandatory)] [object]$Value
    )
    $fullPath = [IO.Path]::GetFullPath($Path)
    $directory = Split-Path -Parent $fullPath
    New-Item -ItemType Directory -Force -Path $directory | Out-Null
    $temporary = $fullPath + ".tmp"
    [IO.File]::WriteAllText(
        $temporary,
        ($Value | ConvertTo-Json -Depth 100),
        [Text.UTF8Encoding]::new($false)
    )
    Move-Item -LiteralPath $temporary -Destination $fullPath -Force
}

function Get-NormalizedModelName {
    param([string]$Name)
    if ($null -eq $Name) {
        return ""
    }
    return (($Name.ToLowerInvariant() -replace 'int8','') -replace '[^a-z0-9]+','')
}

function Resolve-LocalModelID {
    param(
        [Parameter(Mandatory)] [object]$SourceModel,
        [Parameter(Mandatory)] [ValidateSet("embedding", "summary", "derivative", "vlm", "asr")] [string]$Purpose
    )
    $sourceName = [string]$SourceModel.name
    $sourceType = [string]$SourceModel.type
    $normalized = Get-NormalizedModelName -Name $sourceName
    $candidates = @($script:LocalModels | Where-Object {
        [string]$_.type -eq $sourceType -and
        (Get-NormalizedModelName -Name ([string]$_.name)) -eq $normalized
    })
    if ($Purpose -eq "derivative") {
        $candidates = @($candidates | Where-Object {
            [string]$_.id -match 'derivative' -or [string]$_.workload_scope -eq 'derivative_only'
        })
    }
    if ($candidates.Count -eq 1) {
        return [string]$candidates[0].id
    }
    $fallback = switch ($Purpose) {
        "embedding" { "prod-qwen3-embedding-8b" }
        "summary" {
            if ($normalized -match 'deepseekv4flash') {
                "prod-deepseek-v4-flash-int8-chat"
            } elseif ($normalized -match 'qwen3627b') {
                "prod-qwen36-27b-chat"
            } else {
                ""
            }
        }
        "derivative" { "prod-qwen36-35b-derivative" }
        "vlm" { "prod-qwen3-vl-32b-vlm" }
        "asr" { "prod-qwen25-omni-7b-asr" }
    }
    if ([string]::IsNullOrWhiteSpace($fallback) -or $fallback -notin @($script:LocalModels.id)) {
        throw "no unique local model mapping for purpose=$Purpose source=$sourceName"
    }
    return $fallback
}

function Resolve-SourceModelByID {
    param([string]$SourceID)
    if ([string]::IsNullOrWhiteSpace($SourceID)) {
        return $null
    }
    $matches = @($script:Manifest.model_catalog | Where-Object { [string]$_.source_id -eq $SourceID })
    if ($matches.Count -ne 1) {
        throw "source model catalog has no unique entry for $SourceID"
    }
    return $matches[0]
}

function Convert-ProcessOverrides {
    param([object]$SourceOverrides)
    if ($null -eq $SourceOverrides) {
        return $null
    }
    $converted = ($SourceOverrides | ConvertTo-Json -Depth 50) | ConvertFrom-Json -Depth 50 -AsHashtable
    if ($converted.ContainsKey("vlm_config") -and $null -ne $converted.vlm_config) {
        $sourceModel = Resolve-SourceModelByID -SourceID ([string]$converted.vlm_config.model_id)
        if ($null -ne $sourceModel) {
            $converted.vlm_config.model_id = Resolve-LocalModelID -SourceModel $sourceModel -Purpose "vlm"
        }
        $converted.vlm_config.Remove("api_key")
        $converted.vlm_config.Remove("base_url")
    }
    if ($converted.ContainsKey("asr_config") -and $null -ne $converted.asr_config) {
        $sourceModel = Resolve-SourceModelByID -SourceID ([string]$converted.asr_config.model_id)
        if ($null -ne $sourceModel) {
            $converted.asr_config.model_id = Resolve-LocalModelID -SourceModel $sourceModel -Purpose "asr"
        }
    }
    if ($converted.ContainsKey("graph_enabled")) {
        $converted.graph_enabled = $false
    }
    return $converted
}

if (-not (Test-Path -LiteralPath $RunnerEnv)) {
    throw "runner.env is missing: $RunnerEnv"
}
if (-not (Test-Path -LiteralPath $ManifestPath)) {
    throw "production full-KB manifest is missing: $ManifestPath"
}
$settings = Read-EnvFile -Path $RunnerEnv
$apiKey = [string]$settings["WEKNORA_E2E_TENANT_API_KEY"]
$apiBase = $BaseUrl.TrimEnd("/")
if ([string]::IsNullOrWhiteSpace($apiKey)) {
    throw "runner.env does not contain WEKNORA_E2E_TENANT_API_KEY"
}
$script:Headers = @{ "X-API-Key" = $apiKey }
$script:Manifest = Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json -Depth 100
if (
    [int]$script:Manifest.schema_version -ne 2 -or
    [int]$script:Manifest.document_count -ne 682 -or
    $script:Manifest.signed_urls_persisted -ne $false -or
    $script:Manifest.wiki_enabled_locally -ne $false
) {
    throw "production full-KB manifest failed its safety/count contract"
}

$manifestRoot = Split-Path -Parent (Split-Path -Parent ([IO.Path]::GetFullPath($ManifestPath)))
$artifactRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot "artifacts"))
if (-not $manifestRoot.StartsWith($artifactRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "manifest must remain under the ignored agent-eval artifacts directory"
}
$manifestSha = (Get-FileHash -LiteralPath $ManifestPath -Algorithm SHA256).Hash.ToLowerInvariant()
$freeBytes = (Get-PSDrive -Name ([IO.Path]::GetPathRoot($manifestRoot).TrimEnd(':\'))).Free
if ($freeBytes -lt 20GB) {
    throw "less than 20 GiB is free; compact unused VHD space before importing"
}

$capabilities = Invoke-LocalApi -Method GET -Path "/api/v1/custom/agent-eval/capabilities"
$runtimeMode = [string]$capabilities.data.mode
$recorderEnabled = [bool]$capabilities.data.recorder_enabled
if (($runtimeMode -ne "production" -or $recorderEnabled) -and -not $AllowEvalRuntime) {
    throw "target must be the local production-mode runtime with eval recording disabled; pass -AllowEvalRuntime only for an explicitly isolated Eval rebuild"
}
if ($AllowEvalRuntime) {
    if ($runtimeMode -ne "eval") {
        throw "-AllowEvalRuntime requires the target capabilities mode to be eval"
    }
    Write-Warning "Eval runtime import explicitly enabled; Eval recording remains isolated from KB contents"
}
$script:LocalModels = @((Invoke-LocalApi -Method GET -Path "/api/v1/models").data)

if (Test-Path -LiteralPath $StatePath) {
    $state = Get-Content -LiteralPath $StatePath -Raw | ConvertFrom-Json -Depth 100 -AsHashtable
    if ([string]$state.manifest_sha256 -ne $manifestSha -or [string]$state.api_base_url -ne $BaseUrl) {
        throw "existing import state belongs to a different manifest or API"
    }
} else {
    $state = [ordered]@{
        schema_version = 1
        manifest_sha256 = $manifestSha
        api_base_url = $BaseUrl
        created_at = [DateTimeOffset]::UtcNow.ToString("o")
        updated_at = [DateTimeOffset]::UtcNow.ToString("o")
        knowledge_bases = [ordered]@{}
    }
    Save-JsonAtomic -Path $StatePath -Value $state
}

$existingKBs = Get-AllKnowledgeBases
$modelBindings = [ordered]@{}
foreach ($kb in $script:Manifest.knowledge_bases) {
    $slug = [string]$kb.slug
    $sourceID = [string]$kb.source_knowledge_base_id
    $embeddingID = Resolve-LocalModelID -SourceModel $kb.embedding_model -Purpose "embedding"
    $summaryID = Resolve-LocalModelID -SourceModel $kb.summary_model -Purpose "summary"
    $derivativeID = Resolve-LocalModelID -SourceModel $kb.derivative_model -Purpose "derivative"
    $modelBindings[$sourceID] = [ordered]@{
        embedding_model_id = $embeddingID
        summary_model_id = $summaryID
        derivative_model_id = $derivativeID
    }
    if ($state.knowledge_bases.Contains($slug)) {
        $localID = [string]$state.knowledge_bases[$slug].local_knowledge_base_id
        $matches = @($existingKBs | Where-Object { [string]$_.id -eq $localID -and [string]$_.name -eq [string]$kb.name })
        if ($matches.Count -ne 1) {
            throw "state references a missing or renamed local knowledge base: $slug"
        }
        continue
    }
    $sameName = @($existingKBs | Where-Object { [string]$_.name -eq [string]$kb.name })
    if ($sameName.Count -gt 0) {
        throw "a local knowledge base already uses the production name without import state: $($kb.name)"
    }
    $sourceVLM = Resolve-SourceModelByID -SourceID ([string]$kb.vlm_config.model_id)
    $vlmID = if ($null -eq $sourceVLM) { "" } else { Resolve-LocalModelID -SourceModel $sourceVLM -Purpose "vlm" }
    $sourceASR = Resolve-SourceModelByID -SourceID ([string]$kb.asr_config.model_id)
    $asrID = if ($null -eq $sourceASR) { "" } else { Resolve-LocalModelID -SourceModel $sourceASR -Purpose "asr" }
    $indexingStrategy = ($kb.local_indexing_strategy | ConvertTo-Json -Depth 20) | ConvertFrom-Json -Depth 20 -AsHashtable
    $indexingStrategy.wiki_enabled = $false
    $body = [ordered]@{
        name = [string]$kb.name
        description = [string]$kb.description
        type = [string]$kb.type
        embedding_model_id = $embeddingID
        summary_model_id = $summaryID
        derivative_model_id = $derivativeID
        storage_provider_config = @{ provider = "minio" }
        chunking_config = $kb.chunking_config
        image_processing_config = $kb.image_processing_config
        vlm_config = @{
            enabled = [bool]$kb.vlm_config.enabled
            model_id = $vlmID
            model_name = ""
            base_url = ""
            api_key = ""
            interface_type = ""
        }
        asr_config = @{
            enabled = [bool]$kb.asr_config.enabled
            model_id = $asrID
            language = [string]$kb.asr_config.language
        }
        extract_config = $kb.extract_config
        faq_config = $kb.faq_config
        question_generation_config = $kb.question_generation_config
        wiki_config = $null
        indexing_strategy = $indexingStrategy
    }
    $created = Invoke-LocalApi -Method POST -Path "/api/v1/knowledge-bases" -Body $body
    $localID = [string]$created.data.id
    if ($localID -notmatch '^[0-9a-f-]{36}$') {
        throw "knowledge-base creation returned an invalid id for $($kb.name)"
    }
    $state.knowledge_bases[$slug] = [ordered]@{
        source_knowledge_base_id = $sourceID
        local_knowledge_base_id = $localID
        name = [string]$kb.name
        expected_document_count = @($kb.documents).Count
        documents = [ordered]@{}
    }
    $state.updated_at = [DateTimeOffset]::UtcNow.ToString("o")
    Save-JsonAtomic -Path $StatePath -Value $state
    Write-Host "Created local production-equivalent KB: $($kb.name) ($localID)"
    $existingKBs = Get-AllKnowledgeBases
}

foreach ($kb in $script:Manifest.knowledge_bases) {
    $slug = [string]$kb.slug
    $binding = $state.knowledge_bases[$slug]
    $localKBID = [string]$binding.local_knowledge_base_id
    $current = @(Get-AllKnowledges -KnowledgeBaseID $localKBID)
    $currentByID = @{}
    foreach ($item in $current) {
        $currentByID[[string]$item.id] = $item
    }
    foreach ($sourceID in @($binding.documents.Keys)) {
        $localID = [string]$binding.documents[$sourceID].local_knowledge_id
        if (-not $currentByID.ContainsKey($localID)) {
            throw "import state references a missing local knowledge row: $sourceID"
        }
    }
    if ($current.Count -ne $binding.documents.Count) {
        throw "local KB contains rows not owned by the resumable import state: $($kb.name)"
    }

    $manualDocuments = @($kb.documents | Where-Object { [string]$_.type -eq "manual" })
    foreach ($document in $manualDocuments) {
        $sourceID = [string]$document.source_knowledge_id
        if ($binding.documents.Contains($sourceID)) {
            continue
        }
        $manualBody = [ordered]@{
            title = [string]$document.title
            content = [string]$document.metadata.content
            status = [string]$document.metadata.status
            tag_ids = @()
            channel = "web"
            process_config = Convert-ProcessOverrides -SourceOverrides $document.metadata.process_overrides
        }
        $created = Invoke-LocalApi -Method POST -Path "/api/v1/knowledge-bases/$localKBID/knowledge/manual" -Body $manualBody -TimeoutSeconds 300
        $localID = [string]$created.data.id
        if ($localID -notmatch '^[0-9a-f-]{36}$') {
            throw "manual knowledge creation returned an invalid id for $sourceID"
        }
        $binding.documents[$sourceID] = [ordered]@{
            local_knowledge_id = $localID
            type = "manual"
            file_name = $null
            expected_hash = [string]$document.local_sha256
        }
        $state.updated_at = [DateTimeOffset]::UtcNow.ToString("o")
        Save-JsonAtomic -Path $StatePath -Value $state
        Write-Host "Created manual knowledge [$($kb.name)] $($document.title)"
    }

    $pendingFiles = @($kb.documents | Where-Object {
        [string]$_.type -eq "file" -and -not $binding.documents.Contains([string]$_.source_knowledge_id)
    })
    for ($offset = 0; $offset -lt $pendingFiles.Count; $offset += $UploadBatchSize) {
        $end = [Math]::Min($offset + $UploadBatchSize - 1, $pendingFiles.Count - 1)
        $batch = @($pendingFiles[$offset..$end] | ForEach-Object {
            $fullPath = [IO.Path]::GetFullPath((Join-Path $manifestRoot ([string]$_.relative_path)))
            if (-not $fullPath.StartsWith($manifestRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
                throw "document path escaped the production replay root"
            }
            if (-not (Test-Path -LiteralPath $fullPath)) {
                throw "captured document is missing: $($_.source_knowledge_id)"
            }
            [pscustomobject]@{
                source_id = [string]$_.source_knowledge_id
                path = $fullPath
                upload_name = [string]$_.file_name
                expected_size = [long]$_.file_size
                expected_hash = [string]$_.production_file_hash
                local_sha256 = [string]$_.local_sha256
                enable_multimodel = [bool]$kb.vlm_config.enabled
            }
        })
        $uploadResults = @(
            $batch | ForEach-Object -Parallel {
                $plan = $_
                try {
                    $form = @{
                        file = Get-Item -LiteralPath $plan.path
                        fileName = $plan.upload_name
                        enable_multimodel = ([string]$plan.enable_multimodel).ToLowerInvariant()
                        channel = "web"
                    }
                    $response = Invoke-WebRequest -Uri "$using:apiBase/api/v1/knowledge-bases/$using:localKBID/knowledge/file" -Method Post -Headers @{ "X-API-Key" = $using:apiKey } -Form $form -TimeoutSec 900 -SkipHttpErrorCheck
                    if ($response.StatusCode -notin @(200, 201)) {
                        return [pscustomobject]@{ source_id=$plan.source_id; status="failed"; local_id=$null; error_type="HTTP$($response.StatusCode)" }
                    }
                    $payload = $response.Content | ConvertFrom-Json
                    $localID = [string]$payload.data.id
                    if ($localID -notmatch '^[0-9a-f-]{36}$') {
                        return [pscustomobject]@{ source_id=$plan.source_id; status="failed"; local_id=$null; error_type="InvalidResponse" }
                    }
                    return [pscustomobject]@{ source_id=$plan.source_id; status="uploaded"; local_id=$localID; error_type=$null }
                } catch {
                    return [pscustomobject]@{ source_id=$plan.source_id; status="failed"; local_id=$null; error_type=$_.Exception.GetType().Name }
                }
            } -ThrottleLimit $UploadConcurrency
        )
        $failed = @($uploadResults | Where-Object status -eq "failed")
        foreach ($result in $uploadResults | Where-Object status -eq "uploaded") {
            $source = @($batch | Where-Object source_id -eq ([string]$result.source_id))[0]
            $binding.documents[[string]$result.source_id] = [ordered]@{
                local_knowledge_id = [string]$result.local_id
                type = "file"
                file_name = [string]$source.upload_name
                expected_size = [long]$source.expected_size
                expected_hash = [string]$source.expected_hash
                local_sha256 = [string]$source.local_sha256
            }
        }
        $state.updated_at = [DateTimeOffset]::UtcNow.ToString("o")
        Save-JsonAtomic -Path $StatePath -Value $state
        if ($failed.Count -gt 0) {
            throw "$($failed.Count) uploads failed in $($kb.name): $(@($failed | ForEach-Object { "$($_.source_id):$($_.error_type)" }) -join ', ')"
        }
        $freeNow = (Get-PSDrive -Name ([IO.Path]::GetPathRoot($manifestRoot).TrimEnd(':\'))).Free
        if ($freeNow -lt 10GB) {
            throw "free disk space fell below 10 GiB during import"
        }
        Write-Host "Uploaded [$($kb.name)] $($binding.documents.Count)/$($binding.expected_document_count)"
    }
}

$deadline = [DateTimeOffset]::UtcNow.AddSeconds($ParseTimeoutSeconds)
do {
    $pending = [Collections.Generic.List[string]]::new()
    $failed = [Collections.Generic.List[string]]::new()
    $totals = [ordered]@{
        documents = 0
        core_ready = 0
        parse_completed = 0
        enrichment_terminal = 0
        enrichment_degraded = 0
        wiki_none = 0
    }
    foreach ($kb in $script:Manifest.knowledge_bases) {
        $binding = $state.knowledge_bases[[string]$kb.slug]
        $items = @(Get-AllKnowledges -KnowledgeBaseID ([string]$binding.local_knowledge_base_id))
        $totals.documents += $items.Count
        if ($items.Count -ne [int]$binding.expected_document_count) {
            $pending.Add("$($kb.name):count=$($items.Count)/$($binding.expected_document_count)")
        }
        foreach ($item in $items) {
            if ([string]$item.parse_status -eq "failed" -or [string]$item.core_status -eq "failed") {
                $failed.Add("$($kb.name)/$($item.id):core")
                continue
            }
            if ([string]$item.enrichment_status -eq "failed") {
                $failed.Add("$($kb.name)/$($item.id):enrichment")
                continue
            }
            if ([string]$item.core_status -eq "ready") { $totals.core_ready++ }
            if ([string]$item.parse_status -eq "completed") { $totals.parse_completed++ }
            if ([string]$item.enrichment_status -in @("completed", "none", "degraded")) { $totals.enrichment_terminal++ }
            if ([string]$item.enrichment_status -eq "degraded") { $totals.enrichment_degraded++ }
            if ([string]$item.wiki_status -eq "none") { $totals.wiki_none++ }
            if (
                [string]$item.core_status -ne "ready" -or
                [string]$item.parse_status -ne "completed" -or
                [string]$item.enrichment_status -notin @("completed", "none", "degraded") -or
                [string]$item.wiki_status -ne "none"
            ) {
                $pending.Add("$($kb.slug)/$($item.id)")
            }
        }
    }
    if ($failed.Count -gt 0) {
        throw "local production-equivalent processing failed: $($failed -join ', ')"
    }
    Write-Host (
        "Processing status documents={0}/682 core={1}/682 parse={2}/682 enrichment-terminal={3}/682 degraded={4} wiki-none={5}/682 pending={6}" -f
        $totals.documents,$totals.core_ready,$totals.parse_completed,$totals.enrichment_terminal,$totals.enrichment_degraded,$totals.wiki_none,$pending.Count
    )
    if ($totals.documents -eq 682 -and $pending.Count -eq 0) {
        break
    }
    if ([DateTimeOffset]::UtcNow -ge $deadline) {
        throw "local production-equivalent processing timed out with $($pending.Count) pending rows"
    }
    Start-Sleep -Seconds $PollSeconds
} while ($true)

$bindingManifest = [ordered]@{
    schema_version = 1
    environment = if ($runtimeMode -eq "eval") { "local-eval" } else { "local-production-mode" }
    api_base_url = $BaseUrl
    source_environment = "weknora-prod"
    source_manifest_sha256 = $manifestSha
    generated_at = [DateTimeOffset]::UtcNow.ToString("o")
    document_count = 682
    file_document_count = 680
    manual_document_count = 2
    storage_provider = "minio"
    retrieve_driver = "postgres"
    wiki_enabled = $false
    model_bindings = $modelBindings
    knowledge_bases = $state.knowledge_bases
}
Save-JsonAtomic -Path $BindingOutput -Value $bindingManifest
Write-Host "Local production-equivalent knowledge bases are ready: $BindingOutput"
