[CmdletBinding()]
param(
    [string]$KnowledgeTitle = "采购管理办法.docx",
    [string]$PostgresContainer = "WeKnora-agent-eval-postgres-dev",
    [string]$RuntimeContainer = "weknora-agent-eval-runtime-api-1",
    [string]$ProfilePath = (Join-Path $PSScriptRoot "profiles/multiturn-agents.v1.json"),
    [string]$Output = (Join-Path $PSScriptRoot "runner.env")
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Escape-SqlLiteral {
    param([Parameter(Mandatory)] [string]$Value)
    return $Value.Replace("'", "''")
}

function ConvertFrom-StoredSecret {
    param(
        [Parameter(Mandatory)] [string]$Stored,
        [Parameter(Mandatory)] [byte[]]$Key
    )
    $prefix = "enc:v1:"
    if (-not $Stored.StartsWith($prefix, [System.StringComparison]::Ordinal)) {
        return $Stored
    }
    $encoded = $Stored.Substring($prefix.Length).Replace("-", "+").Replace("_", "/")
    while (($encoded.Length % 4) -ne 0) { $encoded += "=" }
    $combined = [System.Convert]::FromBase64String($encoded)
    $nonceSize = 12
    $tagSize = 16
    $cipherSize = $combined.Length - $nonceSize - $tagSize
    if ($cipherSize -lt 1) { throw "invalid stored tenant API key ciphertext" }
    $nonce = [byte[]]::new($nonceSize)
    $ciphertext = [byte[]]::new($cipherSize)
    $tag = [byte[]]::new($tagSize)
    $plaintext = [byte[]]::new($cipherSize)
    [System.Array]::Copy($combined, 0, $nonce, 0, $nonceSize)
    [System.Array]::Copy($combined, $nonceSize, $ciphertext, 0, $cipherSize)
    [System.Array]::Copy($combined, $nonceSize + $cipherSize, $tag, 0, $tagSize)
    $aes = [System.Security.Cryptography.AesGcm]::new($Key, $tagSize)
    try {
        $aes.Decrypt($nonce, $ciphertext, $tag, $plaintext)
    } finally {
        $aes.Dispose()
    }
    return [System.Text.Encoding]::UTF8.GetString($plaintext)
}

function ConvertFrom-HexUtf8 {
    param([Parameter(Mandatory)] [string]$Value)
    return [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromHexString($Value))
}

& docker inspect $PostgresContainer *> $null
if ($LASTEXITCODE -ne 0) {
    throw "isolated eval PostgreSQL container is unavailable: $PostgresContainer"
}
& docker inspect $RuntimeContainer *> $null
if ($LASTEXITCODE -ne 0) {
    throw "isolated eval runtime container is unavailable: $RuntimeContainer"
}

$profileSet = Get-Content -LiteralPath $ProfilePath -Raw | ConvertFrom-Json
$requiredModelID = [string]$profileSet.model.required_model_id
if ([string]::IsNullOrWhiteSpace($requiredModelID)) {
    throw "profile set does not declare model.required_model_id"
}
$title = Escape-SqlLiteral $KnowledgeTitle
$requiredModel = Escape-SqlLiteral $requiredModelID
$sql = @"
SELECT
  tenant.api_key,
  model.id,
  knowledge.id,
  COALESCE(NULLIF(knowledge.file_hash, ''), knowledge.id::text),
  encode(convert_to(model.parameters->>'base_url', 'UTF8'), 'hex'),
  encode(convert_to(model.parameters->>'api_key', 'UTF8'), 'hex'),
  encode(convert_to(model.name, 'UTF8'), 'hex'),
  model.parameters->>'interface_type'
FROM knowledges AS knowledge
JOIN tenants AS tenant ON tenant.id = knowledge.tenant_id
JOIN LATERAL (
  SELECT id, name, parameters
  FROM models
  WHERE tenant_id = knowledge.tenant_id
    AND id = '$requiredModel'
    AND type = 'KnowledgeQA'
    AND status = 'active'
    AND deleted_at IS NULL
  LIMIT 1
) AS model ON true
WHERE knowledge.title = '$title'
  AND knowledge.enable_status = 'enabled'
  AND knowledge.parse_status = 'completed'
  AND knowledge.deleted_at IS NULL
  AND tenant.api_key IS NOT NULL
  AND tenant.api_key <> ''
ORDER BY knowledge.updated_at DESC
LIMIT 1;
"@

$row = & docker exec $PostgresContainer psql -U postgres -d WeKnora -At -F "|" -c $sql
if ($LASTEXITCODE -ne 0) { throw "failed to resolve eval runner bindings" }
$fields = ([string]$row).Trim().Split("|", 8)
if ($fields.Count -ne 8 -or @($fields | Where-Object { [string]::IsNullOrWhiteSpace($_) }).Count -gt 0) {
    throw "no complete eval binding found for knowledge title: $KnowledgeTitle"
}

$storedAPIKey, $modelID, $knowledgeID, $corpusHash, $hexJudgeBaseURL, `
    $hexJudgeAPIKey, $hexJudgeModel, $judgeInterface = $fields
if ($judgeInterface -ne "openai") {
    throw "required eval model is not OpenAI-compatible: interface_type=$judgeInterface"
}
$systemAESKey = [string](& docker exec $RuntimeContainer printenv SYSTEM_AES_KEY)
if ($LASTEXITCODE -ne 0 -or [System.Text.Encoding]::UTF8.GetByteCount($systemAESKey.Trim()) -ne 32) {
    throw "eval runtime does not expose a valid 32-byte SYSTEM_AES_KEY"
}
$apiKey = ConvertFrom-StoredSecret `
    -Stored $storedAPIKey `
    -Key ([System.Text.Encoding]::UTF8.GetBytes($systemAESKey.Trim()))
$judgeBaseURL = (ConvertFrom-HexUtf8 $hexJudgeBaseURL).TrimEnd("/")
$judgeAPIKey = ConvertFrom-StoredSecret `
    -Stored (ConvertFrom-HexUtf8 $hexJudgeAPIKey) `
    -Key ([System.Text.Encoding]::UTF8.GetBytes($systemAESKey.Trim()))
$judgeModel = ConvertFrom-HexUtf8 $hexJudgeModel
if ([string]::IsNullOrWhiteSpace($judgeBaseURL) -or `
    [string]::IsNullOrWhiteSpace($judgeAPIKey) -or `
    [string]::IsNullOrWhiteSpace($judgeModel)) {
    throw "required model does not provide a complete OpenAI-compatible judge binding"
}
$capabilities = Invoke-RestMethod `
    -Uri "http://localhost:18080/api/v1/custom/agent-eval/capabilities" `
    -Headers @{ "X-API-Key" = $apiKey } `
    -TimeoutSec 10
if ($capabilities.data.mode -ne "eval" -or $capabilities.data.recorder_enabled -ne $true) {
    throw "refusing to prepare runner.env: target did not pass the eval-only handshake"
}

$content = @(
    "# Generated from the physically isolated eval database; do not commit.",
    "WEKNORA_E2E_TENANT_API_KEY=$apiKey",
    "AGENT_EVAL_SUMMARY_MODEL_ID=$modelID",
    "AGENT_EVAL_PROCUREMENT_KNOWLEDGE_ID=$knowledgeID",
    "AGENT_EVAL_CORPUS_VERSION=sha256:$corpusHash",
    "AGENT_EVAL_JUDGE_BASE_URL=$judgeBaseURL",
    "AGENT_EVAL_JUDGE_API_KEY=$judgeAPIKey",
    "AGENT_EVAL_JUDGE_MODEL=$judgeModel",
    "AGENT_EVAL_JUDGE_TIMEOUT_SECONDS=180",
    "AGENT_EVAL_JUDGE_MAX_ATTEMPTS=2",
    ""
) -join "`n"

$outputPath = [System.IO.Path]::GetFullPath($Output)
[System.IO.File]::WriteAllText($outputPath, $content, [System.Text.UTF8Encoding]::new($false))
Write-Host "Prepared isolated runner binding: model=$modelID knowledge=$knowledgeID judge_model=$judgeModel"
Write-Host "Tenant and model secret values were written only to $outputPath and were not printed."
