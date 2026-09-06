[CmdletBinding()]
param(
    [string]$Profile = "weknora-prod-ops2",
    [string]$ExpectedTarget = "root@10.14.201.6",
    [string]$OutputRoot = (Join-Path $PSScriptRoot "artifacts/production-replay-v2/conversations")
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$controller = "C:\Users\erjiguan\.codex\skills\xshell-control\scripts\xshell_bridge.py"

function Invoke-Bridge {
    param(
        [Parameter(Mandatory)] [string]$Command,
        [ValidateRange(10, 600)] [int]$TimeoutSeconds = 120
    )
    for ($attempt = 1; $attempt -le 5; $attempt++) {
        $output = @(& python $controller exec $Profile --timeout $TimeoutSeconds --command $Command 2>&1)
        if ($LASTEXITCODE -eq 0) { return $output }
        $safeFailure = [string]($output -join " ")
        if ($safeFailure -match 'WinError 32|being used by another process|另一个程序正在使用此文件') {
            if ($attempt -lt 5) {
                Start-Sleep -Seconds (2 * $attempt)
                continue
            }
        }
        throw "Xshell command failed without exposing its payload: $($output[0])"
    }
    throw "Xshell command exhausted bounded retries"
}

function ConvertFrom-GzipPayload {
    param([Parameter(Mandatory)] [object[]]$Output)
    $lines = @(
        $Output |
            ForEach-Object { ([string]$_).Trim() } |
            Where-Object { $_.StartsWith("PAYLOAD:", [StringComparison]::Ordinal) } |
            ForEach-Object { $_.Substring(8) }
    )
    if ($lines.Count -eq 0) { throw "remote command returned no encoded payload" }
    $compressed = [Convert]::FromBase64String(($lines -join ""))
    $inputStream = [IO.MemoryStream]::new($compressed)
    $gzip = [IO.Compression.GZipStream]::new($inputStream, [IO.Compression.CompressionMode]::Decompress)
    $reader = [IO.StreamReader]::new($gzip, [Text.Encoding]::UTF8)
    try { return $reader.ReadToEnd() }
    finally {
        $reader.Dispose()
        $gzip.Dispose()
        $inputStream.Dispose()
    }
}

function Invoke-RemoteSqlJson {
    param([Parameter(Mandatory)] [string]$Sql)
    $sqlBase64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($Sql))
    $command = 'set -o pipefail; pg=$(kubectl -n weknora get pods -o name | grep "^pod/weknora-postgres-" | head -n1); test -n "$pg"; echo ' +
        $sqlBase64 +
        ' | base64 -d | kubectl -n weknora exec -i "$pg" -- psql -U postgres -d WeKnora -At | gzip -c | base64 -w 76 | sed "s/^/PAYLOAD:/"'
    $raw = Invoke-Bridge -Command $command
    return (ConvertFrom-GzipPayload -Output $raw) | ConvertFrom-Json -Depth 100
}

function Save-JsonAtomic {
    param(
        [Parameter(Mandatory)] [string]$Path,
        [Parameter(Mandatory)] [object]$Value
    )
    $fullPath = [IO.Path]::GetFullPath($Path)
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $fullPath) | Out-Null
    $temporary = $fullPath + ".tmp"
    [IO.File]::WriteAllText($temporary, ($Value | ConvertTo-Json -Depth 100), [Text.UTF8Encoding]::new($false))
    Move-Item -LiteralPath $temporary -Destination $fullPath -Force
}

$status = @(& python $controller status $Profile 2>&1)
if ($LASTEXITCODE -ne 0) { throw "cannot inspect Xshell profile $Profile" }
$statusMap = @{}
foreach ($line in $status) {
    if ([string]$line -match '^(?<key>[^=]+)=(?<value>.*)$') { $statusMap[$Matches.key] = $Matches.value }
}
if ($statusMap.target_hint -ne $ExpectedTarget -or $statusMap.ready -ne "yes" -or $statusMap.live -ne "yes") {
    throw "Xshell profile is not the expected live production target"
}

$sql = @'
WITH target_kbs(id, slug, name) AS (
  VALUES
    ('795248f3-0ee0-4080-bbcc-c6d4922bc55a','expense-ops-qa','报账系统运维问答知识库'),
    ('7e98a44e-19d8-421f-b5dd-cd361d9dbb35','imoutai-private-wecom','爱茅台（企业微信私有部署）'),
    ('95ead990-423e-461e-be88-45d7a4326358','expense-operation-guide','茅台报账系统操作指引')
), target_sessions AS (
  SELECT s.*, kb.slug AS kb_slug, kb.name AS kb_name, kb.id AS source_kb_id
  FROM sessions s
  JOIN LATERAL jsonb_array_elements_text(coalesce(s.agent_config->'knowledge_base_ids','[]'::jsonb)) selected(kb_id) ON true
  JOIN target_kbs kb ON kb.id=selected.kb_id
  WHERE s.deleted_at IS NULL
), user_usage AS (
  SELECT ts.user_id, count(m.id) FILTER (WHERE m.role='user' AND m.deleted_at IS NULL) AS user_turns
  FROM target_sessions ts
  LEFT JOIN messages m ON m.session_id=ts.id
  GROUP BY ts.user_id
), session_payload AS (
  SELECT ts.created_at, jsonb_build_object(
    'source_session_alias','session-'||left(md5(ts.id),12),
    'user_alias','user-'||left(md5(coalesce(ts.user_id,'<null>')),12),
    'high_usage_user',coalesce(uu.user_turns,0)>=5,
    'user_total_turns_in_scope',coalesce(uu.user_turns,0),
    'kb_slug',ts.kb_slug,
    'kb_name',ts.kb_name,
    'source_knowledge_base_id',ts.source_kb_id,
    'agent_kind',coalesce(
      ts.agent_config->>'agent_id',
      CASE WHEN coalesce((ts.agent_config->>'agent_enabled')::boolean,false)
        THEN '<enabled-unspecified>' ELSE 'builtin-quick-answer' END
    ),
    'source_model_id',ts.agent_config->>'model_id',
    'web_search_enabled',coalesce((ts.agent_config->>'web_search_enabled')::boolean,false),
    'created_at',ts.created_at,
    'updated_at',ts.updated_at,
    'messages',(
      SELECT coalesce(jsonb_agg(jsonb_build_object(
        'ordinal',ordered.ordinal,
        'role',ordered.role,
        'content',ordered.content,
        'rendered_content',ordered.rendered_content,
        'is_completed',ordered.is_completed,
        'is_fallback',ordered.is_fallback,
        'agent_duration_ms',ordered.agent_duration_ms,
        'agent_mode',ordered.agent_mode,
        'agent_tool_count',ordered.agent_tool_count,
        'knowledge_references',ordered.knowledge_references,
        'retrieval_stats',ordered.retrieval_stats,
        'has_images',jsonb_array_length(coalesce(ordered.images,'[]'::jsonb))>0,
        'has_attachments',jsonb_array_length(coalesce(ordered.attachments,'[]'::jsonb))>0,
        'created_at',ordered.created_at
      ) ORDER BY ordered.ordinal),'[]'::jsonb)
      FROM (
        SELECT m.*,row_number() OVER (ORDER BY m.created_at,m.id) AS ordinal
        FROM messages m
        WHERE m.session_id=ts.id AND m.deleted_at IS NULL
      ) ordered
    )
  ) AS payload
  FROM target_sessions ts
  JOIN user_usage uu ON uu.user_id IS NOT DISTINCT FROM ts.user_id
)
SELECT jsonb_build_object(
  'schema_version',1,
  'source_environment','weknora-prod',
  'capture_mode','read-only-production-conversation-observations',
  'generated_at',clock_timestamp(),
  'high_usage_threshold_user_turns',5,
  'target_knowledge_bases',(SELECT jsonb_agg(jsonb_build_object('source_id',id,'slug',slug,'name',name) ORDER BY slug) FROM target_kbs),
  'sessions',coalesce(jsonb_agg(payload ORDER BY created_at),'[]'::jsonb)
)
FROM session_payload;
'@

$observations = Invoke-RemoteSqlJson -Sql $sql
$outputRootPath = [IO.Path]::GetFullPath($OutputRoot)
$artifactRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot "artifacts"))
if (-not $outputRootPath.StartsWith($artifactRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "OutputRoot must remain under the ignored agent-eval artifacts directory"
}

$observationPath = Join-Path $outputRootPath "production-conversation-observations.json"
Save-JsonAtomic -Path $observationPath -Value $observations

$replaySessions = @(
    foreach ($session in @($observations.sessions)) {
        $userTurns = @(
            foreach ($message in @($session.messages | Where-Object role -eq "user")) {
                [ordered]@{
                    source_ordinal = [int]$message.ordinal
                    query = [string]$message.content
                    has_images = [bool]$message.has_images
                    has_attachments = [bool]$message.has_attachments
                }
            }
        )
        if ($userTurns.Count -eq 0) { continue }
        [ordered]@{
            source_session_alias = [string]$session.source_session_alias
            user_alias = [string]$session.user_alias
            high_usage_user = [bool]$session.high_usage_user
            kb_slug = [string]$session.kb_slug
            source_agent_kind = [string]$session.agent_kind
            turns = $userTurns
        }
    }
)
$replay = [ordered]@{
    schema_version = 1
    source_environment = "weknora-prod"
    target_environment = "local-production-mode"
    generated_at = [DateTimeOffset]::UtcNow.ToString("o")
    source_answers_in_sut_input = $false
    source_references_in_sut_input = $false
    reference_answers_in_sut_input = $false
    judge_feedback_in_sut_input = $false
    sealed_holdout_accessed = $false
    sessions = $replaySessions
}
$replayPath = Join-Path $outputRootPath "production-replay-inputs.json"
Save-JsonAtomic -Path $replayPath -Value $replay

$allMessages = @($observations.sessions | ForEach-Object { @($_.messages) })
$summary = [ordered]@{
    schema_version = 1
    generated_at = [DateTimeOffset]::UtcNow.ToString("o")
    session_count = @($observations.sessions).Count
    high_usage_session_count = @($observations.sessions | Where-Object high_usage_user).Count
    user_turn_count = @($allMessages | Where-Object role -eq "user").Count
    assistant_turn_count = @($allMessages | Where-Object role -eq "assistant").Count
    incomplete_assistant_count = @($allMessages | Where-Object { $_.role -eq "assistant" -and (-not $_.is_completed -or [string]::IsNullOrWhiteSpace([string]$_.content)) }).Count
    replay_contains_source_answers = $false
    replay_contains_source_references = $false
}
$summaryPath = Join-Path $outputRootPath "capture-summary.json"
Save-JsonAtomic -Path $summaryPath -Value $summary

Write-Host "Production conversations captured read-only: sessions=$($summary.session_count) high-usage-sessions=$($summary.high_usage_session_count) user-turns=$($summary.user_turn_count) assistants=$($summary.assistant_turn_count) incomplete-assistants=$($summary.incomplete_assistant_count)"
Write-Host "Private observations: $observationPath"
Write-Host "SUT-safe replay input: $replayPath"
