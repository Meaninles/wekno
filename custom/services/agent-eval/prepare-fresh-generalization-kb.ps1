[CmdletBinding()]
param(
    [string]$BaseUrl = "http://localhost:18080",
    [string]$RunnerEnv = (Join-Path $PSScriptRoot "runner.env"),
    [string]$Fixture = (Join-Path $PSScriptRoot "fixtures/regression-corpora/audio-release-handbook.v1.md"),
    [string]$BindingOutput = (Join-Path $PSScriptRoot "artifacts/fresh-generalization-kb-binding.v1.json"),
    [string]$EnvironmentKey = "AGENT_EVAL_KB_FRESH_GENERALIZATION_MEDIA_ID",
    [string]$KnowledgeBaseName = "Eval回归-Northbank音频交付手册-v1",
    [string]$UploadName = "audio-release-handbook.v1.md",
    [string]$CorpusVersion = "northbank-audio-release-v1",
    [ValidateRange(60, 3600)] [int]$ParseTimeoutSeconds = 1200,
    [ValidateRange(2, 60)] [int]$PollSeconds = 5
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

function Set-EnvValue {
    param(
        [Parameter(Mandatory)] [string]$Path,
        [Parameter(Mandatory)] [string]$Key,
        [Parameter(Mandatory)] [string]$Value
    )
    $lines = [Collections.Generic.List[string]]::new()
    $updated = $false
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ($line -match "^$([regex]::Escape($Key))=") {
            $lines.Add("$Key=$Value")
            $updated = $true
        } else {
            $lines.Add($line)
        }
    }
    if (-not $updated) { $lines.Add("$Key=$Value") }
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

if (-not (Test-Path -LiteralPath $RunnerEnv)) { throw "runner.env is missing: $RunnerEnv" }
if (-not (Test-Path -LiteralPath $Fixture)) { throw "regression fixture is missing: $Fixture" }
$settings = Read-EnvFile -Path $RunnerEnv
$apiKey = [string]$settings["WEKNORA_E2E_TENANT_API_KEY"]
if ([string]::IsNullOrWhiteSpace($apiKey)) { throw "runner.env does not contain the eval tenant API key" }
$script:Headers = @{ "X-API-Key" = $apiKey }

$capabilities = Invoke-EvalApi -Method GET -Path "/api/v1/custom/agent-eval/capabilities"
if ($capabilities.data.mode -ne "eval" -or $capabilities.data.recorder_enabled -ne $true) {
    throw "target API did not pass the eval-only handshake"
}

$fixtureItem = Get-Item -LiteralPath $Fixture
$sourceHash = (Get-FileHash -LiteralPath $fixtureItem.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
$existing = @((Invoke-EvalApi -Method GET -Path "/api/v1/knowledge-bases?page=1&page_size=200").data |
    Where-Object { [string]$_.name -eq $knowledgeBaseName })
if ($existing.Count -gt 1) { throw "multiple fresh regression knowledge bases use the frozen name" }
if (
    $existing.Count -eq 1 -and
    -not ([string]$existing[0].description).Contains("sha256:$sourceHash")
) {
    throw "existing fresh regression knowledge base was built from a different frozen fixture"
}

if ($existing.Count -eq 0) {
    $body = [ordered]@{
        name = $knowledgeBaseName
        description = "Independent post-prompt generalization corpus; sha256:$sourceHash"
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
            parser_engine_rules = @(@{ engine = "builtin"; file_types = @("md", "markdown") })
        }
        question_generation_config = @{ enabled = $false; question_count = 3 }
        indexing_strategy = @{
            vector_enabled = $true
            keyword_enabled = $true
            wiki_enabled = $false
            graph_enabled = $false
        }
    }
    $created = Invoke-EvalApi -Method POST -Path "/api/v1/knowledge-bases" -Body $body
    $knowledgeBaseID = [string]$created.data.id
} else {
    $knowledgeBaseID = [string]$existing[0].id
}
if ($knowledgeBaseID -notmatch '^[0-9a-f-]{36}$') { throw "invalid fresh regression knowledge-base id" }

$documents = @((Invoke-EvalApi -Method GET -Path "/api/v1/knowledge-bases/$knowledgeBaseID/knowledge?page=1&page_size=100").data)
$unexpected = @($documents | Where-Object { [string]$_.file_name -ne $uploadName })
if ($unexpected.Count -gt 0) { throw "fresh regression knowledge base contains unexpected documents" }
if ($documents.Count -eq 0) {
    $upload = Invoke-WebRequest -Uri "$($BaseUrl.TrimEnd('/'))/api/v1/knowledge-bases/$knowledgeBaseID/knowledge/file" `
        -Method Post -Headers $script:Headers -Form @{ file = $fixtureItem; fileName = $uploadName } `
        -TimeoutSec 300 -SkipHttpErrorCheck
    if ($upload.StatusCode -notin @(200, 201)) {
        throw "fresh regression fixture upload failed with HTTP $($upload.StatusCode)"
    }
} elseif ($documents.Count -ne 1 -or [long]$documents[0].file_size -ne [long]$fixtureItem.Length) {
    throw "existing fresh regression document identity differs from the frozen fixture"
}

$deadline = [DateTimeOffset]::UtcNow.AddSeconds($ParseTimeoutSeconds)
do {
    $documents = @((Invoke-EvalApi -Method GET -Path "/api/v1/knowledge-bases/$knowledgeBaseID/knowledge?page=1&page_size=100").data)
    if ($documents.Count -eq 1 -and [string]$documents[0].parse_status -eq "failed") {
        throw "fresh regression document parsing failed"
    }
    $ready = $documents.Count -eq 1 -and
        [string]$documents[0].parse_status -eq "completed" -and
        [string]$documents[0].enable_status -eq "enabled"
    if (-not $ready) {
        if ([DateTimeOffset]::UtcNow -ge $deadline) { throw "fresh regression document parsing timed out" }
        Start-Sleep -Seconds $PollSeconds
    }
} until ($ready)

$document = $documents[0]
$identity = "$EnvironmentKey|$knowledgeBaseID|$($document.id)|$sourceHash"
$hasher = [Security.Cryptography.SHA256]::Create()
try {
    $bindingHash = [Convert]::ToHexString(
        $hasher.ComputeHash([Text.Encoding]::UTF8.GetBytes($identity))
    ).ToLowerInvariant()
} finally {
    $hasher.Dispose()
}
Set-EnvValue -Path $RunnerEnv -Key $EnvironmentKey -Value $knowledgeBaseID
Set-EnvValue -Path $RunnerEnv -Key "AGENT_EVAL_CORPUS_VERSION" -Value $CorpusVersion
Set-EnvValue -Path $RunnerEnv -Key "AGENT_EVAL_KB_BINDINGS_SHA256" -Value $bindingHash
$binding = [ordered]@{
    schema_version = 1
    corpus_version = $CorpusVersion
    binding_identity_sha256 = $bindingHash
    environment_variable = $EnvironmentKey
    knowledge_base_id = $knowledgeBaseID
    knowledge_id = [string]$document.id
    name = $KnowledgeBaseName
    source_file = $UploadName
    source_sha256 = $sourceHash
    source_size = [long]$fixtureItem.Length
}
[IO.File]::WriteAllText(
    [IO.Path]::GetFullPath($BindingOutput),
    (($binding | ConvertTo-Json -Depth 10) + [Environment]::NewLine),
    [Text.UTF8Encoding]::new($false)
)
Write-Host "Regression KB is ready: $KnowledgeBaseName"
Write-Host "Binding artifact: $BindingOutput"
