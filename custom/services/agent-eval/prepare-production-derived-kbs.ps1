[CmdletBinding()]
param(
    [string]$BaseUrl = "http://localhost:18080",
    [string]$RunnerEnv = (Join-Path $PSScriptRoot "runner.env"),
    [string]$CorpusRoot = (Join-Path $PSScriptRoot "artifacts/production-corpus-v1"),
    [string]$BindingOutput = (Join-Path $PSScriptRoot "artifacts/production-derived-kb-bindings.v1.json"),
    [ValidateRange(60, 7200)]
    [int]$ParseTimeoutSeconds = 3600,
    [ValidateRange(2, 60)]
    [int]$PollSeconds = 10
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$nl = [Environment]::NewLine

function Read-EnvFile {
    param([Parameter(Mandatory)] [string]$Path)
    $values = [ordered]@{}
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ($line -match '^(?<key>[A-Za-z_][A-Za-z0-9_]*)=(?<value>.*)$') {
            $values[$Matches.key] = $Matches.value
        }
    }
    return $values
}

function Set-EnvValues {
    param(
        [Parameter(Mandatory)] [string]$Path,
        [Parameter(Mandatory)] [Collections.IDictionary]$Updates
    )
    $existing = [Collections.Generic.List[string]]::new()
    $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ($line -match '^(?<key>[A-Za-z_][A-Za-z0-9_]*)=') {
            $key = $Matches.key
            if ($Updates.Contains($key)) {
                $existing.Add("$key=$($Updates[$key])")
                [void]$seen.Add($key)
                continue
            }
        }
        $existing.Add($line)
    }
    foreach ($key in $Updates.Keys) {
        if (-not $seen.Contains([string]$key)) {
            $existing.Add("$key=$($Updates[$key])")
        }
    }
    [IO.File]::WriteAllText(
        [IO.Path]::GetFullPath($Path),
        (($existing -join [Environment]::NewLine) + [Environment]::NewLine),
        [Text.UTF8Encoding]::new($false)
    )
}

function Invoke-EvalApi {
    param(
        [Parameter(Mandatory)] [ValidateSet("GET", "POST")] [string]$Method,
        [Parameter(Mandatory)] [string]$Path,
        [object]$Body
    )
    $arguments = @{
        Uri = $BaseUrl.TrimEnd("/") + $Path
        Method = $Method
        Headers = $script:Headers
        TimeoutSec = 60
    }
    if ($PSBoundParameters.ContainsKey("Body")) {
        $arguments.ContentType = "application/json; charset=utf-8"
        $arguments.Body = $Body | ConvertTo-Json -Depth 20 -Compress
    }
    return Invoke-RestMethod @arguments
}

function Get-AllKnowledgeBases {
    return @((Invoke-EvalApi -Method GET -Path "/api/v1/knowledge-bases?page=1&page_size=200").data)
}

function Get-Knowledges {
    param([Parameter(Mandatory)] [string]$KnowledgeBaseID)
    return @((Invoke-EvalApi -Method GET -Path "/api/v1/knowledge-bases/$KnowledgeBaseID/knowledge?page=1&page_size=500").data)
}

function New-EvalKnowledgeBase {
    param(
        [Parameter(Mandatory)] [string]$Name,
        [Parameter(Mandatory)] [string]$Description
    )
    $body = [ordered]@{
        name = $Name
        description = $Description
        type = "document"
        embedding_model_id = "prod-qwen3-embedding-8b"
        summary_model_id = "prod-deepseek-v4-flash-int8-chat"
        derivative_model_id = "prod-qwen36-35b-derivative"
        storage_provider_config = @{ provider = "local" }
        chunking_config = [ordered]@{
            strategy = "auto"
            chunk_size = 512
            separators = @("$nl$nl", $nl, "。", "！", "？", ";", "；")
            chunk_overlap = 80
            child_chunk_size = 384
            parent_chunk_size = 4096
            enable_parent_child = $true
            parser_engine_rules = @(
                @{ engine = "builtin"; file_types = @("pdf") },
                @{ engine = "builtin"; file_types = @("docx", "doc") },
                @{ engine = "builtin"; file_types = @("pptx", "ppt") },
                @{ engine = "builtin"; file_types = @("xlsx", "xls") },
                @{ engine = "builtin"; file_types = @("epub") },
                @{ engine = "builtin"; file_types = @("mhtml") },
                @{ engine = "simple"; file_types = @("csv") },
                @{ engine = "builtin"; file_types = @("md", "markdown") },
                @{ engine = "simple"; file_types = @("txt") },
                @{ engine = "simple"; file_types = @("json") },
                @{ engine = "builtin"; file_types = @("jpg", "jpeg", "png", "gif", "bmp", "tiff", "webp") },
                @{ engine = "simple"; file_types = @("mp3", "wav", "m4a", "flac", "ogg") }
            )
        }
        question_generation_config = @{ enabled = $true; question_count = 3 }
        indexing_strategy = @{
            vector_enabled = $true
            keyword_enabled = $true
            wiki_enabled = $false
            graph_enabled = $false
        }
    }
    $response = Invoke-EvalApi -Method POST -Path "/api/v1/knowledge-bases" -Body $body
    $id = [string]$response.data.id
    if ($id -notmatch '^[0-9a-f-]{36}$') {
        throw "knowledge-base creation returned an invalid id for $Name"
    }
    return $id
}

function Send-Document {
    param(
        [Parameter(Mandatory)] [string]$KnowledgeBaseID,
        [Parameter(Mandatory)] [string]$Path,
        [Parameter(Mandatory)] [string]$UploadName
    )
    $request = @{
        Uri = "$($BaseUrl.TrimEnd('/'))/api/v1/knowledge-bases/$KnowledgeBaseID/knowledge/file"
        Method = "Post"
        Headers = $script:Headers
        Form = @{ file = Get-Item -LiteralPath $Path; fileName = $UploadName }
        TimeoutSec = 300
        SkipHttpErrorCheck = $true
    }
    $response = Invoke-WebRequest @request
    if ($response.StatusCode -notin @(200, 201)) {
        $bodyPrefix = $response.Content.Substring(0, [Math]::Min(500, $response.Content.Length))
        throw "upload failed for $UploadName with HTTP $($response.StatusCode): $bodyPrefix"
    }
    $payload = $response.Content | ConvertFrom-Json
    $id = [string]$payload.data.id
    if ($id -notmatch '^[0-9a-f-]{36}$') {
        throw "upload returned an invalid knowledge id for $UploadName"
    }
    return $id
}

function Get-ContentHash {
    param([Parameter(Mandatory)] [string[]]$Rows)
    $content = (($Rows | Sort-Object) -join [Environment]::NewLine) + [Environment]::NewLine
    $bytes = [Text.Encoding]::UTF8.GetBytes($content)
    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        return [Convert]::ToHexString($sha.ComputeHash($bytes)).ToLowerInvariant()
    } finally {
        $sha.Dispose()
    }
}

if (-not (Test-Path -LiteralPath $RunnerEnv)) {
    throw "runner.env is missing: $RunnerEnv"
}
$settings = Read-EnvFile -Path $RunnerEnv
$apiKey = [string]$settings["WEKNORA_E2E_TENANT_API_KEY"]
if ([string]::IsNullOrWhiteSpace($apiKey)) {
    throw "runner.env does not contain the isolated eval tenant API key"
}
$script:Headers = @{ "X-API-Key" = $apiKey }

$mainContainers = @(
    docker ps --format "{{.Names}}" |
        Where-Object {
            $_ -notmatch "(?i)agent-eval" -and
            ($_ -match "(?i)^WeKnora-" -or $_ -match "(?i)^weknora-runtime-profile-e2e-" -or $_ -match "(?i)^weknora-custom-")
        }
)
if ($mainContainers.Count -gt 0) {
    throw "main WeKnora services must be stopped: $($mainContainers -join ', ')"
}

$capabilities = Invoke-EvalApi -Method GET -Path "/api/v1/custom/agent-eval/capabilities"
if ($capabilities.data.mode -ne "eval" -or $capabilities.data.recorder_enabled -ne $true) {
    throw "target API did not pass the eval-only handshake"
}

$corpusRoot = [IO.Path]::GetFullPath($CorpusRoot)
$corpusManifestPath = Join-Path $corpusRoot "corpus-manifest.json"
if (-not (Test-Path -LiteralPath $corpusManifestPath)) {
    throw "production corpus manifest is missing: $corpusManifestPath"
}
$corpusManifest = Get-Content -LiteralPath $corpusManifestPath -Raw | ConvertFrom-Json
if ([int]$corpusManifest.document_count -ne 26 -or $corpusManifest.signed_urls_persisted -ne $false) {
    throw "production corpus manifest failed its safety/count contract"
}

$groupSpecs = [ordered]@{
    "system-construction-policy" = [ordered]@{
        name = "Eval生产派生-系统建设制度-v1"
        env = "AGENT_EVAL_KB_SYSTEM_POLICY_ID"
        files = @()
    }
    "digital-policies" = [ordered]@{
        name = "Eval生产派生-数字化制度-v1"
        env = "AGENT_EVAL_KB_DIGITAL_POLICIES_ID"
        files = @()
    }
    "smart-moutai" = [ordered]@{
        name = "Eval生产派生-智慧茅台-v1"
        env = "AGENT_EVAL_KB_SMART_MOUTAI_ID"
        files = @()
    }
    "ip-evidence" = [ordered]@{
        name = "Eval生产派生-知识产权证据-v1"
        env = "AGENT_EVAL_KB_IP_EVIDENCE_ID"
        files = @()
    }
    "cloud-platform" = [ordered]@{
        name = "Eval生产派生-茅台云-v1"
        env = "AGENT_EVAL_KB_CLOUD_PLATFORM_ID"
        files = @()
    }
    "weknora-guide" = [ordered]@{
        name = "Eval生产派生-WeKnora使用指南-v1"
        env = "AGENT_EVAL_KB_WEKNORA_GUIDE_ID"
        files = @()
    }
}

foreach ($row in $corpusManifest.files) {
    $group = [string]$row.corpus_group
    if (-not $groupSpecs.Contains($group)) {
        throw "unknown corpus group: $group"
    }
    $path = [IO.Path]::GetFullPath((Join-Path $corpusRoot ([string]$row.relative_path)))
    if (-not $path.StartsWith($corpusRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        throw "corpus file escaped root: $path"
    }
    $actualSha = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualSha -ne [string]$row.local_sha256) {
        throw "corpus SHA-256 mismatch: $path"
    }
    $groupSpecs[$group].files += [pscustomobject]@{
        Path = $path
        UploadName = [string]$row.original_file_name
        Sha256 = $actualSha
        Size = [long]$row.size_bytes
        SourceKnowledgeID = [string]$row.knowledge_id
    }
}

$guideFiles = @(
    @{ Path = "README.md"; Name = "WeKnora-README.md" },
    @{ Path = "docs\QA.md"; Name = "WeKnora-QA.md" },
    @{ Path = "docs\wiki\Home.md"; Name = "WeKnora-Wiki-Home.md" },
    @{ Path = "docs\api\agent.md"; Name = "WeKnora-API-Agent.md" },
    @{ Path = "docs\api\chat.md"; Name = "WeKnora-API-Chat.md" },
    @{ Path = "docs\custom\使用指南\用户使用指南.md"; Name = "WeKnora-用户使用指南.md" },
    @{ Path = "docs\custom\使用指南\智能体开发指南.md"; Name = "WeKnora-智能体开发指南.md" },
    @{ Path = "docs\agent-skills.md"; Name = "WeKnora-Agent-Skills.md" }
)
$repositoryRoot = (& git -C $PSScriptRoot rev-parse --show-toplevel).Trim()
if ($LASTEXITCODE -ne 0) {
    throw "cannot resolve repository root"
}
foreach ($entry in $guideFiles) {
    $path = [IO.Path]::GetFullPath((Join-Path $repositoryRoot $entry.Path))
    if (-not (Test-Path -LiteralPath $path)) {
        throw "guide source is missing: $path"
    }
    $groupSpecs["weknora-guide"].files += [pscustomobject]@{
        Path = $path
        UploadName = [string]$entry.Name
        Sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
        Size = (Get-Item -LiteralPath $path).Length
        SourceKnowledgeID = $null
    }
}

$identityRows = foreach ($group in $groupSpecs.Keys) {
    foreach ($file in $groupSpecs[$group].files) {
        "$group|$($file.UploadName)|$($file.Size)|$($file.Sha256)"
    }
}
$corpusSha = Get-ContentHash -Rows $identityRows
$corpusVersion = "sha256:$corpusSha"

$knownModels = @((Invoke-EvalApi -Method GET -Path "/api/v1/models").data | ForEach-Object { [string]$_.id })
foreach ($requiredModel in @(
    "prod-qwen3-embedding-8b",
    "prod-deepseek-v4-flash-int8-chat",
    "prod-qwen36-35b-derivative"
)) {
    if ($requiredModel -notin $knownModels) {
        throw "required local eval model is unavailable: $requiredModel"
    }
}

$existingKBs = Get-AllKnowledgeBases
$bindings = [ordered]@{}
foreach ($group in $groupSpecs.Keys) {
    $spec = $groupSpecs[$group]
    $matches = @($existingKBs | Where-Object { [string]$_.name -eq [string]$spec.name })
    if ($matches.Count -gt 1) {
        throw "multiple knowledge bases use the frozen name: $($spec.name)"
    }
    $created = $false
    if ($matches.Count -eq 0) {
        $kbID = New-EvalKnowledgeBase -Name ([string]$spec.name) -Description "Codex production-derived eval corpus v1; isolated local copy; $corpusVersion"
        $created = $true
        $existingKBs = Get-AllKnowledgeBases
    } else {
        $kbID = [string]$matches[0].id
    }
    $current = @(Get-Knowledges -KnowledgeBaseID $kbID)
    $expectedNames = @($spec.files | ForEach-Object { [string]$_.UploadName })
    $unexpected = @($current | Where-Object { [string]$_.file_name -notin $expectedNames })
    if ($unexpected.Count -gt 0) {
        throw "frozen eval KB '$($spec.name)' contains unexpected documents: $(@($unexpected.file_name) -join ', ')"
    }
    foreach ($file in $spec.files) {
        $sameName = @($current | Where-Object { [string]$_.file_name -eq [string]$file.UploadName })
        if ($sameName.Count -gt 1) {
            throw "duplicate document name in '$($spec.name)': $($file.UploadName)"
        }
        if ($sameName.Count -eq 1) {
            if ([long]$sameName[0].file_size -ne [long]$file.Size) {
                throw "existing document size mismatch in '$($spec.name)': $($file.UploadName)"
            }
            continue
        }
        [void](Send-Document -KnowledgeBaseID $kbID -Path $file.Path -UploadName $file.UploadName)
        Write-Host "Uploaded [$group] $($file.UploadName)"
        $current = @(Get-Knowledges -KnowledgeBaseID $kbID)
    }
    $bindings[$group] = [ordered]@{
        knowledge_base_id = $kbID
        name = [string]$spec.name
        environment_variable = [string]$spec.env
        created_this_run = $created
        expected_document_count = @($spec.files).Count
    }
}

$deadline = [DateTimeOffset]::UtcNow.AddSeconds($ParseTimeoutSeconds)
do {
    $pending = [Collections.Generic.List[string]]::new()
    $failed = [Collections.Generic.List[string]]::new()
    foreach ($group in $groupSpecs.Keys) {
        $binding = $bindings[$group]
        $items = @(Get-Knowledges -KnowledgeBaseID ([string]$binding.knowledge_base_id))
        if ($items.Count -ne [int]$binding.expected_document_count) {
            $pending.Add("$group count=$($items.Count)/$($binding.expected_document_count)")
            continue
        }
        foreach ($item in $items) {
            $status = [string]$item.parse_status
            if ($status -eq "failed") {
                $failed.Add("$group/$($item.file_name)")
            } elseif ($status -ne "completed" -or [string]$item.enable_status -ne "enabled") {
                $pending.Add("$group/$($item.file_name):$status")
            }
        }
    }
    if ($failed.Count -gt 0) {
        throw "document parsing failed: $($failed -join ', ')"
    }
    if ($pending.Count -eq 0) {
        break
    }
    if ([DateTimeOffset]::UtcNow -ge $deadline) {
        throw "document parsing timed out; pending: $($pending -join ', ')"
    }
    Write-Host "Waiting for isolated parsing: pending=$($pending.Count)"
    Start-Sleep -Seconds $PollSeconds
} while ($true)

$manifestGroups = [ordered]@{}
$envUpdates = [ordered]@{
    AGENT_EVAL_PRODUCTION_CORPUS_VERSION = $corpusVersion
}
foreach ($group in $groupSpecs.Keys) {
    $binding = $bindings[$group]
    $items = @(Get-Knowledges -KnowledgeBaseID ([string]$binding.knowledge_base_id) | Sort-Object file_name,id)
    $envUpdates[[string]$binding.environment_variable] = [string]$binding.knowledge_base_id
    $manifestGroups[$group] = [ordered]@{
        knowledge_base_id = [string]$binding.knowledge_base_id
        name = [string]$binding.name
        environment_variable = [string]$binding.environment_variable
        document_count = $items.Count
        documents = @($items | ForEach-Object {
            [ordered]@{
                knowledge_id = [string]$_.id
                file_name = [string]$_.file_name
                file_size = [long]$_.file_size
                file_hash = [string]$_.file_hash
                parse_status = [string]$_.parse_status
                enable_status = [string]$_.enable_status
            }
        })
    }
}

$bindingManifest = [ordered]@{
    schema_version = 1
    environment = "isolated-eval"
    api_base_url = $BaseUrl
    model_id = "prod-deepseek-v4-flash-int8-chat"
    corpus_version = $corpusVersion
    generated_at = [DateTimeOffset]::UtcNow.ToString("o")
    source_document_count = [int]$corpusManifest.document_count
    local_guide_document_count = $guideFiles.Count
    knowledge_bases = $manifestGroups
}
$bindingIdentity = [ordered]@{
    schema_version = 1
    environment = "isolated-eval"
    model_id = "prod-deepseek-v4-flash-int8-chat"
    corpus_version = $corpusVersion
    source_document_count = [int]$corpusManifest.document_count
    local_guide_document_count = $guideFiles.Count
    knowledge_bases = $manifestGroups
}
$bindingIdentityJson = $bindingIdentity | ConvertTo-Json -Depth 12 -Compress
$bindingIdentityBytes = [Text.Encoding]::UTF8.GetBytes($bindingIdentityJson)
$bindingIdentityHasher = [Security.Cryptography.SHA256]::Create()
try {
    $bindingSha = [Convert]::ToHexString(
        $bindingIdentityHasher.ComputeHash($bindingIdentityBytes)
    ).ToLowerInvariant()
} finally {
    $bindingIdentityHasher.Dispose()
}
$bindingManifest["binding_identity_sha256"] = $bindingSha
$bindingDirectory = Split-Path -Parent ([IO.Path]::GetFullPath($BindingOutput))
New-Item -ItemType Directory -Path $bindingDirectory -Force | Out-Null
[IO.File]::WriteAllText(
    [IO.Path]::GetFullPath($BindingOutput),
    (($bindingManifest | ConvertTo-Json -Depth 12) + [Environment]::NewLine),
    [Text.UTF8Encoding]::new($false)
)
$envUpdates["AGENT_EVAL_KB_BINDINGS_SHA256"] = $bindingSha
Set-EnvValues -Path $RunnerEnv -Updates $envUpdates

Write-Host "PRODUCTION_DERIVED_KBS_READY groups=$($groupSpecs.Count) documents=$($identityRows.Count)"
Write-Host "Bindings written without credentials: $BindingOutput"
Write-Host "runner.env updated without printing secret values."
