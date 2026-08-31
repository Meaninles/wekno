[CmdletBinding()]
param(
    [string]$BaseUrl = "http://localhost:18080",
    [string]$RunnerEnv = (Join-Path $PSScriptRoot "runner.env"),
    [string]$FixtureRoot = (Join-Path $PSScriptRoot "fixtures/unseen-corpora"),
    [string]$BindingOutput = (Join-Path $PSScriptRoot "artifacts/unseen-capability-kb-bindings.v1.json"),
    [ValidateRange(60, 3600)]
    [int]$ParseTimeoutSeconds = 1200,
    [ValidateRange(2, 60)]
    [int]$PollSeconds = 5
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
    $lines = [Collections.Generic.List[string]]::new()
    $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::Ordinal)
    foreach ($line in Get-Content -LiteralPath $Path) {
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
        [IO.Path]::GetFullPath($Path),
        (($lines -join [Environment]::NewLine) + [Environment]::NewLine),
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
    return @((Invoke-EvalApi -Method GET -Path "/api/v1/knowledge-bases/$KnowledgeBaseID/knowledge?page=1&page_size=100").data)
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
                @{ engine = "builtin"; file_types = @("md", "markdown") },
                @{ engine = "simple"; file_types = @("txt") }
            )
        }
        question_generation_config = @{ enabled = $false; question_count = 3 }
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
    $arguments = @{
        Uri = "$($BaseUrl.TrimEnd('/'))/api/v1/knowledge-bases/$KnowledgeBaseID/knowledge/file"
        Method = "Post"
        Headers = $script:Headers
        Form = @{ file = Get-Item -LiteralPath $Path; fileName = $UploadName }
        TimeoutSec = 300
        SkipHttpErrorCheck = $true
    }
    $response = Invoke-WebRequest @arguments
    if ($response.StatusCode -notin @(200, 201)) {
        $prefix = $response.Content.Substring(0, [Math]::Min(500, $response.Content.Length))
        throw "upload failed for $UploadName with HTTP $($response.StatusCode): $prefix"
    }
}

function Get-CanonicalHash {
    param([Parameter(Mandatory)] [string[]]$Rows)
    $content = (($Rows | Sort-Object) -join [Environment]::NewLine) + [Environment]::NewLine
    $hasher = [Security.Cryptography.SHA256]::Create()
    try {
        return [Convert]::ToHexString(
            $hasher.ComputeHash([Text.Encoding]::UTF8.GetBytes($content))
        ).ToLowerInvariant()
    } finally {
        $hasher.Dispose()
    }
}

if (-not (Test-Path -LiteralPath $RunnerEnv)) {
    throw "runner.env is missing: $RunnerEnv"
}
$settings = Read-EnvFile -Path $RunnerEnv
$apiKey = [string]$settings["WEKNORA_E2E_TENANT_API_KEY"]
if ([string]::IsNullOrWhiteSpace($apiKey)) {
    throw "runner.env does not contain the eval tenant API key"
}
$script:Headers = @{ "X-API-Key" = $apiKey }

$capabilities = Invoke-EvalApi -Method GET -Path "/api/v1/custom/agent-eval/capabilities"
if ($capabilities.data.mode -ne "eval" -or $capabilities.data.recorder_enabled -ne $true) {
    throw "target API did not pass the eval-only handshake"
}

$fixtureRootPath = [IO.Path]::GetFullPath($FixtureRoot)
$groups = [ordered]@{
    product = [ordered]@{
        name = "Eval未见分布-Orion产品手册-v1"
        env = "AGENT_EVAL_KB_UNSEEN_PRODUCT_MANUAL_ID"
        file = "product-orion-manual.v1.md"
    }
    project = [ordered]@{
        name = "Eval未见分布-Northstar项目手册-v1"
        env = "AGENT_EVAL_KB_UNSEEN_PROJECT_HANDBOOK_ID"
        file = "project-delivery-handbook.v1.md"
    }
    it = [ordered]@{
        name = "Eval未见分布-Atlas运维手册-v1"
        env = "AGENT_EVAL_KB_UNSEEN_IT_RUNBOOK_ID"
        file = "it-operations-runbook.v1.md"
    }
    policy = [ordered]@{
        name = "Eval未见分布-差旅治理制度-v1"
        env = "AGENT_EVAL_KB_UNSEEN_GOVERNANCE_POLICY_ID"
        file = "governance-expense-policy.v1.md"
    }
}

$sourceRows = [Collections.Generic.List[string]]::new()
foreach ($groupName in $groups.Keys) {
    $group = $groups[$groupName]
    $path = Join-Path $fixtureRootPath ([string]$group.file)
    if (-not (Test-Path -LiteralPath $path)) {
        throw "unseen fixture is missing: $path"
    }
    $item = Get-Item -LiteralPath $path
    $group["path"] = $item.FullName
    $group["size"] = [long]$item.Length
    $group["sha256"] = (Get-FileHash -LiteralPath $item.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
    $sourceRows.Add("$groupName|$($group.file)|$($group.size)|$($group.sha256)")
}
$corpusHash = Get-CanonicalHash -Rows $sourceRows

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

$existing = Get-AllKnowledgeBases
$bindings = [ordered]@{}
foreach ($groupName in $groups.Keys) {
    $group = $groups[$groupName]
    $matches = @($existing | Where-Object { [string]$_.name -eq [string]$group.name })
    if ($matches.Count -gt 1) {
        throw "multiple knowledge bases use the frozen name: $($group.name)"
    }
    if ($matches.Count -eq 0) {
        $kbID = New-EvalKnowledgeBase -Name ([string]$group.name) -Description "Codex unseen capability corpus v1; sha256:$corpusHash"
        $existing = Get-AllKnowledgeBases
    } else {
        $kbID = [string]$matches[0].id
    }
    $documents = @(Get-Knowledges -KnowledgeBaseID $kbID)
    $unexpected = @($documents | Where-Object { [string]$_.file_name -ne [string]$group.file })
    if ($unexpected.Count -gt 0) {
        throw "frozen unseen KB '$($group.name)' contains unexpected documents"
    }
    if ($documents.Count -eq 0) {
        Send-Document -KnowledgeBaseID $kbID -Path ([string]$group.path) -UploadName ([string]$group.file)
    } elseif ($documents.Count -ne 1 -or [long]$documents[0].file_size -ne [long]$group.size) {
        throw "existing unseen document identity differs in '$($group.name)'"
    }
    $bindings[$groupName] = [ordered]@{
        knowledge_base_id = $kbID
        name = [string]$group.name
        environment_variable = [string]$group.env
        source_file = [string]$group.file
        source_sha256 = [string]$group.sha256
    }
}

$deadline = [DateTimeOffset]::UtcNow.AddSeconds($ParseTimeoutSeconds)
do {
    $pending = [Collections.Generic.List[string]]::new()
    foreach ($groupName in $groups.Keys) {
        $documents = @(Get-Knowledges -KnowledgeBaseID ([string]$bindings[$groupName].knowledge_base_id))
        if ($documents.Count -ne 1) {
            $pending.Add("$groupName count=$($documents.Count)")
            continue
        }
        if ([string]$documents[0].parse_status -eq "failed") {
            throw "document parsing failed: $groupName/$($documents[0].file_name)"
        }
        if ([string]$documents[0].parse_status -ne "completed" -or [string]$documents[0].enable_status -ne "enabled") {
            $pending.Add("$groupName/$($documents[0].file_name):$($documents[0].parse_status)")
        }
    }
    if ($pending.Count -eq 0) {
        break
    }
    if ([DateTimeOffset]::UtcNow -ge $deadline) {
        throw "document parsing timed out; pending: $($pending -join ', ')"
    }
    Write-Host "Waiting for unseen corpus parsing: pending=$($pending.Count)"
    Start-Sleep -Seconds $PollSeconds
} while ($true)

$identityRows = [Collections.Generic.List[string]]::new()
$manifestGroups = [ordered]@{}
$envUpdates = [ordered]@{ AGENT_EVAL_CORPUS_VERSION = "unseen-corpora-v1" }
foreach ($groupName in $groups.Keys) {
    $binding = $bindings[$groupName]
    $document = @(Get-Knowledges -KnowledgeBaseID ([string]$binding.knowledge_base_id))[0]
    $envUpdates[[string]$binding.environment_variable] = [string]$binding.knowledge_base_id
    $manifestGroups[$groupName] = [ordered]@{
        knowledge_base_id = [string]$binding.knowledge_base_id
        name = [string]$binding.name
        environment_variable = [string]$binding.environment_variable
        source_file = [string]$binding.source_file
        source_sha256 = [string]$binding.source_sha256
        knowledge_id = [string]$document.id
        parse_status = [string]$document.parse_status
        enable_status = [string]$document.enable_status
    }
    $identityRows.Add("$groupName|$($binding.knowledge_base_id)|$($document.id)|$($binding.source_sha256)")
}
$bindingHash = Get-CanonicalHash -Rows $identityRows
$envUpdates["AGENT_EVAL_KB_BINDINGS_SHA256"] = $bindingHash
$manifest = [ordered]@{
    schema_version = 1
    suite = "weknora-unseen-capability-matrix-v1"
    corpus_version = "unseen-corpora-v1"
    corpus_sha256 = $corpusHash
    binding_identity_sha256 = $bindingHash
    generated_at = [DateTimeOffset]::UtcNow.ToString("o")
    knowledge_bases = $manifestGroups
}
$outputDirectory = Split-Path -Parent ([IO.Path]::GetFullPath($BindingOutput))
New-Item -ItemType Directory -Path $outputDirectory -Force | Out-Null
[IO.File]::WriteAllText(
    [IO.Path]::GetFullPath($BindingOutput),
    (($manifest | ConvertTo-Json -Depth 10) + [Environment]::NewLine),
    [Text.UTF8Encoding]::new($false)
)
Set-EnvValues -Path $RunnerEnv -Updates $envUpdates

Write-Host "UNSEEN_CAPABILITY_KBS_READY groups=$($groups.Count) corpus=sha256:$corpusHash"
Write-Host "Bindings written without credentials: $BindingOutput"
Write-Host "runner.env updated without printing secret values."
