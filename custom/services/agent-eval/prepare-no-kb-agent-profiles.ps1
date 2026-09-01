[CmdletBinding()]
param(
    [string]$BaseUrl = "http://localhost:18080",
    [string]$RunnerEnv = (Join-Path $PSScriptRoot "runner.env"),
    [string]$BindingOutput = (Join-Path $PSScriptRoot "artifacts/no-kb-agent-bindings.v1.json")
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

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
        [Parameter(Mandatory)] [ValidateSet("GET", "POST", "PUT")] [string]$Method,
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
        $arguments.Body = $Body | ConvertTo-Json -Depth 50 -Compress
    }
    return Invoke-RestMethod @arguments
}

function Get-Sha256 {
    param([Parameter(Mandatory)] [string]$Value)
    $hasher = [Security.Cryptography.SHA256]::Create()
    try {
        return [Convert]::ToHexString(
            $hasher.ComputeHash([Text.Encoding]::UTF8.GetBytes($Value))
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

$specifications = @(
    [ordered]@{
        key = "quick-answer"
        source_id = "builtin-quick-answer"
        name = "Eval隔离-快速问答-无知识库-v1"
        env = "AGENT_EVAL_AGENT_QUICK_NO_KB_ID"
    },
    [ordered]@{
        key = "rag-reasoning"
        source_id = "builtin-smart-reasoning"
        name = "Eval隔离-RAG推理-无知识库-v1"
        env = "AGENT_EVAL_AGENT_RAG_NO_KB_ID"
    },
    [ordered]@{
        key = "general-agent"
        source_id = "builtin-general-agent"
        name = "Eval隔离-通用智能体-无知识库-v1"
        env = "AGENT_EVAL_AGENT_GENERAL_NO_KB_ID"
    }
)

$listedAgents = @((Invoke-EvalApi -Method GET -Path "/api/v1/agents?page=1&page_size=500").data)
$updates = [ordered]@{}
$bindings = [Collections.Generic.List[object]]::new()

foreach ($specification in $specifications) {
    $sourceResponse = Invoke-EvalApi -Method GET -Path "/api/v1/agents/$($specification.source_id)"
    $source = $sourceResponse.data
    if ($null -eq $source -or $source.is_builtin -ne $true) {
        throw "source built-in agent is unavailable: $($specification.source_id)"
    }

    # Copy the complete production configuration and change only the knowledge
    # selection boundary. This keeps the same quick/ReAct/sidecar runtime path
    # while giving Eval a genuine no-knowledge-base execution profile.
    $config = $source.config | ConvertTo-Json -Depth 50 | ConvertFrom-Json
    $config.kb_selection_mode = "none"
    $config.knowledge_bases = @()

    $matches = @($listedAgents | Where-Object {
        $_.is_builtin -ne $true -and [string]$_.name -eq [string]$specification.name
    })
    if ($matches.Count -gt 1) {
        throw "multiple Eval no-KB agents use the reserved name: $($specification.name)"
    }

    $body = [ordered]@{
        name = [string]$specification.name
        description = "Eval-only production-path clone of $($specification.source_id); knowledge selection is none."
        avatar = [string]$source.avatar
        config = $config
    }
    if ($matches.Count -eq 0) {
        $response = Invoke-EvalApi -Method POST -Path "/api/v1/agents" -Body $body
    } else {
        $response = Invoke-EvalApi -Method PUT -Path "/api/v1/agents/$($matches[0].id)" -Body $body
    }
    $prepared = $response.data
    if ($null -eq $prepared -or [string]::IsNullOrWhiteSpace([string]$prepared.id)) {
        throw "failed to prepare Eval no-KB agent for $($specification.key)"
    }
    if ([string]$prepared.config.kb_selection_mode -ne "none" -or @($prepared.config.knowledge_bases).Count -ne 0) {
        throw "prepared Eval agent is not isolated from knowledge bases: $($specification.key)"
    }
    if ([string]$prepared.config.agent_mode -ne [string]$source.config.agent_mode -or
        [string]$prepared.config.agent_type -ne [string]$source.config.agent_type) {
        throw "prepared Eval agent changed the runtime mode for $($specification.key)"
    }

    $updates[[string]$specification.env] = [string]$prepared.id
    $configJson = $prepared.config | ConvertTo-Json -Depth 50 -Compress
    $bindings.Add([ordered]@{
        profile = [string]$specification.key
        environment_variable = [string]$specification.env
        source_agent_id = [string]$specification.source_id
        prepared_agent_id = [string]$prepared.id
        agent_mode = [string]$prepared.config.agent_mode
        agent_type = [string]$prepared.config.agent_type
        kb_selection_mode = [string]$prepared.config.kb_selection_mode
        knowledge_base_count = @($prepared.config.knowledge_bases).Count
        prepared_config_sha256 = Get-Sha256 -Value $configJson
    })
}

Set-EnvValues -Path $RunnerEnv -Updates $updates
$artifact = [ordered]@{
    schema_version = 1
    scope = "isolated-eval-tenant-only"
    invariant = "source production config with only kb_selection_mode=none and knowledge_bases=[]"
    bindings = $bindings
}
[IO.Directory]::CreateDirectory((Split-Path -Parent ([IO.Path]::GetFullPath($BindingOutput)))) | Out-Null
[IO.File]::WriteAllText(
    [IO.Path]::GetFullPath($BindingOutput),
    (($artifact | ConvertTo-Json -Depth 20) + [Environment]::NewLine),
    [Text.UTF8Encoding]::new($false)
)

Write-Host "Eval no-KB agent profiles are ready for quick-answer, rag-reasoning, and general-agent."
Write-Host "Binding artifact: $BindingOutput"
