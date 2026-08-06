param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$GatewayBaseUrl = "http://weknora-litellm-deepseek-anthropic-test:4000/v1",
    [string]$GatewayClaudeBaseUrl = "http://weknora-litellm-deepseek-anthropic-test:4000",
    [string]$TenantApiKey = $env:WEKNORA_API_KEY,
    [string]$TenantToken = $env:WEKNORA_TOKEN,
    [string]$GatewayApiKey = $env:LITELLM_MASTER_KEY
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($TenantApiKey) -and [string]::IsNullOrWhiteSpace($TenantToken)) {
    throw "WEKNORA_TOKEN/TenantToken or WEKNORA_API_KEY/TenantApiKey is required"
}
if ([string]::IsNullOrWhiteSpace($GatewayApiKey)) {
    throw "LITELLM_MASTER_KEY/GatewayApiKey is required"
}

$headers = @{}
if (-not [string]::IsNullOrWhiteSpace($TenantToken)) {
    $headers.Authorization = "Bearer $TenantToken"
}
else {
    $headers["X-API-Key"] = $TenantApiKey
}

function Invoke-WeKnoraApi {
    param(
        [Parameter(Mandatory)][string]$Method,
        [Parameter(Mandatory)][string]$Path,
        [object]$Body = $null,
        [int[]]$ExpectedStatus = @(200)
    )

    $args = @{
        Uri = "$BaseUrl$Path"
        Method = $Method
        Headers = $headers
        SkipHttpErrorCheck = $true
    }
    if ($null -ne $Body) {
        $args.ContentType = "application/json"
        $args.Body = $Body | ConvertTo-Json -Depth 20 -Compress
    }
    $response = Invoke-WebRequest @args
    if ($response.StatusCode -notin $ExpectedStatus) {
        throw "$Method $Path returned $($response.StatusCode): $($response.Content)"
    }
    if ([string]::IsNullOrWhiteSpace($response.Content)) {
        return $null
    }
    return $response.Content | ConvertFrom-Json
}

function New-Parameters {
    param(
        [hashtable]$ExtraConfig = @{},
        [bool]$SupportsVision = $false,
        [int]$Dimension = 0,
        [int]$TruncatePromptTokens = 0,
        [bool]$SupportsDimensionOverride = $false
    )

    return @{
        base_url = $GatewayBaseUrl
        api_key = $GatewayApiKey
        provider = "generic"
        interface_type = "openai"
        parameter_size = ""
        extra_config = $ExtraConfig
        custom_headers = @{}
        supports_vision = $SupportsVision
        embedding_parameters = @{
            dimension = $Dimension
            truncate_prompt_tokens = $TruncatePromptTokens
            supports_dimension_override = $SupportsDimensionOverride
        }
    }
}

$chatExtra = @{
    thinking_control = "chat_template_kwargs"
    general_agent_claude_base_url = $GatewayClaudeBaseUrl
}

$models = @(
    @{ name = "DeepSeek-V4-Flash-INT8-local"; display_name = "DeepSeek-V4-Flash-INT8-local"; type = "KnowledgeQA"; workload_scope = "interactive"; parameters = (New-Parameters -ExtraConfig $chatExtra) },
    @{ name = "Qwen3.6-27B-tool-local"; display_name = "Qwen3.6-27B-tool-local"; type = "KnowledgeQA"; workload_scope = "interactive"; parameters = (New-Parameters -ExtraConfig $chatExtra) },
    @{ name = "DeepSeek-V4-Flash-INT8-local"; display_name = "DeepSeek-V4-Flash-INT8-local（派生任务）"; type = "KnowledgeQA"; workload_scope = "derivative_only"; parameters = (New-Parameters -ExtraConfig $chatExtra) },
    @{ name = "Qwen3.6-27B-tool-local"; display_name = "Qwen3.6-27B-tool-local（派生任务）"; type = "KnowledgeQA"; workload_scope = "derivative_only"; parameters = (New-Parameters -ExtraConfig $chatExtra) },
    @{ name = "Qwen3-VL-32B-local"; display_name = "Qwen3-VL-32B-local"; type = "VLLM"; workload_scope = "interactive"; parameters = (New-Parameters -SupportsVision $true) },
    @{ name = "Qwen2.5-Omni-7B-local"; display_name = "Qwen2.5-Omni-7B-local（多模态）"; type = "VLLM"; workload_scope = "interactive"; parameters = (New-Parameters -SupportsVision $true) },
    @{ name = "Qwen2.5-Omni-7B-local"; display_name = "Qwen2.5-Omni-7B-local（语音转写）"; type = "ASR"; workload_scope = "interactive"; parameters = (New-Parameters -ExtraConfig @{ asr_transport = "openai_chat_audio"; asr_max_tokens = "4096"; asr_response_format = "json"; asr_chat_audio_format = "input_audio" }) },
    @{ name = "Qwen3-Embedding-8B-local"; display_name = "Qwen3-Embedding-8B-local"; type = "Embedding"; workload_scope = "interactive"; parameters = (New-Parameters -Dimension 4096 -TruncatePromptTokens 8192) },
    @{ name = "bge-reranker-v2-m3-local"; display_name = "bge-reranker-v2-m3-local"; type = "Rerank"; workload_scope = "interactive"; parameters = (New-Parameters) }
)

$existing = @((Invoke-WeKnoraApi -Method Get -Path "/api/v1/models").data)
$results = [System.Collections.Generic.List[object]]::new()

foreach ($model in $models) {
    $match = @($existing | Where-Object {
        $_.name -eq $model.name -and
        $_.type -eq $model.type -and
        $_.workload_scope -eq $model.workload_scope
    })
    if ($match.Count -gt 1) {
        throw "multiple existing rows match $($model.type)/$($model.name)/$($model.workload_scope)"
    }

    $body = @{
        name = $model.name
        display_name = $model.display_name
        type = $model.type
        source = "remote"
        description = "llmgateway-prod non-GLM mirror; only the public alias has the -local suffix"
        workload_scope = $model.workload_scope
        parameters = $model.parameters
    }

    if ($match.Count -eq 0) {
        $created = Invoke-WeKnoraApi -Method Post -Path "/api/v1/models" -Body $body -ExpectedStatus @(201)
        $results.Add([pscustomobject]@{ action = "created"; id = $created.data.id; type = $model.type; name = $model.name; workload_scope = $model.workload_scope })
        continue
    }

    $id = [string]$match[0].id
    $updateBody = $body.Clone()
    $updateParameters = $model.parameters.Clone()
    $updateParameters.Remove("api_key")
    $updateBody.parameters = $updateParameters
    $null = Invoke-WeKnoraApi -Method Put -Path "/api/v1/models/$id" -Body $updateBody
    $null = Invoke-WeKnoraApi -Method Put -Path "/api/v1/models/$id/credentials" -Body @{ api_key = $GatewayApiKey }
    $results.Add([pscustomobject]@{ action = "updated"; id = $id; type = $model.type; name = $model.name; workload_scope = $model.workload_scope })
}

$refreshed = @((Invoke-WeKnoraApi -Method Get -Path "/api/v1/models").data)
$derivativeModels = @($refreshed | Where-Object {
    $_.type -eq "KnowledgeQA" -and
    $_.workload_scope -eq "derivative_only" -and
    $_.name -in @("DeepSeek-V4-Flash-INT8-local", "Qwen3.6-27B-tool-local")
})
if ($derivativeModels.Count -ne 2) {
    throw "expected exactly two local derivative models, found $($derivativeModels.Count)"
}
foreach ($derivativeModel in $derivativeModels) {
    $null = Invoke-WeKnoraApi -Method Post -Path "/api/v1/custom/derivative-control/models" -Body @{
        model_id = [string]$derivativeModel.id
        model_tenant_id = [uint64]$derivativeModel.tenant_id
    }
    $results.Add([pscustomobject]@{ action = "published"; id = $derivativeModel.id; type = $derivativeModel.type; name = $derivativeModel.name; workload_scope = $derivativeModel.workload_scope })
}

$qwenDerivative = @($derivativeModels | Where-Object { $_.name -eq "Qwen3.6-27B-tool-local" })
if ($qwenDerivative.Count -ne 1) {
    throw "expected exactly one Qwen3.6-27B-tool-local derivative model"
}
$null = Invoke-WeKnoraApi -Method Put -Path "/api/v1/custom/derivative-control/default" -Body @{
    model_id = [string]$qwenDerivative[0].id
}
$results.Add([pscustomobject]@{ action = "set_derivative_default"; id = $qwenDerivative[0].id; type = $qwenDerivative[0].type; name = $qwenDerivative[0].name; workload_scope = $qwenDerivative[0].workload_scope })

$results | ConvertTo-Json -Depth 5
