[CmdletBinding()]
param(
    [string]$Profile = "weknora-prod-ops2",
    [string]$ExpectedTarget = "root@10.14.201.6",
    [string]$OutputRoot = (Join-Path $PSScriptRoot "artifacts/production-replay-v2"),
    [ValidateRange(1, 16)]
    [int]$DownloadConcurrency = 8,
    [ValidateRange(5, 100)]
    [int]$SigningBatchSize = 20
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$controller = "C:\Users\erjiguan\.codex\skills\xshell-control\scripts\xshell_bridge.py"
$expectedCounts = [ordered]@{
    "company-policies" = 598
    "imoutai-private-wecom" = 6
    "expense-ops-qa" = 11
    "expense-operation-guide" = 19
    "my-work" = 48
}

function Invoke-Bridge {
    param(
        [Parameter(Mandatory)] [string]$Command,
        [ValidateRange(10, 600)] [int]$TimeoutSeconds = 90
    )
    for ($attempt = 1; $attempt -le 5; $attempt++) {
        $output = @(& python $controller exec $Profile --timeout $TimeoutSeconds --command $Command 2>&1)
        if ($LASTEXITCODE -eq 0) {
            return $output
        }
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
    if ($lines.Count -eq 0) {
        throw "remote command returned no encoded payload"
    }
    $compressed = [Convert]::FromBase64String(($lines -join ""))
    $inputStream = [IO.MemoryStream]::new($compressed)
    $gzip = [IO.Compression.GZipStream]::new(
        $inputStream,
        [IO.Compression.CompressionMode]::Decompress
    )
    $reader = [IO.StreamReader]::new($gzip, [Text.Encoding]::UTF8)
    try {
        return $reader.ReadToEnd()
    } finally {
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

function Get-RemoteSignedDownloads {
    param([Parameter(Mandatory)] [object[]]$Rows)
    $payload = $Rows | ConvertTo-Json -Depth 10 -Compress
    $payloadBase64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($payload))
    $signer = @'
import base64
import datetime
import hashlib
import hmac
import json
import os
from urllib.parse import quote, urlsplit

rows = json.loads(base64.b64decode(os.environ["INPUT_B64"]))
endpoint = os.environ["OBS_ENDPOINT"].rstrip("/")
region = os.environ["OBS_REGION"]
access_key = os.environ["OBS_ACCESS_KEY"]
secret_key = os.environ["OBS_SECRET_KEY"]
bucket = os.environ["OBS_BUCKET_NAME"]
proxy = os.environ.get("OBS_PROXY_DOMAIN", "").rstrip("/")
parsed_endpoint = urlsplit(endpoint)
host = parsed_endpoint.netloc

def hmac_sha256(key, message):
    return hmac.new(key, message.encode(), hashlib.sha256).digest()

def object_key(raw):
    canonical_prefix = "obs://" + bucket + "/"
    if raw.startswith(canonical_prefix):
        return raw[len(canonical_prefix):]
    endpoint_prefix = endpoint + "/" + bucket + "/"
    if raw.startswith(endpoint_prefix):
        return raw[len(endpoint_prefix):]
    if proxy and raw.startswith(proxy + "/"):
        return raw[len(proxy) + 1:]
    if "://" not in raw:
        return raw.lstrip("/")
    raise ValueError("unsupported source path")

now = datetime.datetime.now(datetime.timezone.utc)
amz_date = now.strftime("%Y%m%dT%H%M%SZ")
date_stamp = now.strftime("%Y%m%d")
scope = f"{date_stamp}/{region}/s3/aws4_request"

result = []
for row in rows:
    key = object_key(row["file_path"])
    canonical_uri = quote(
        (parsed_endpoint.path.rstrip("/") + "/" + bucket + "/" + key) or "/",
        safe="/-_.~",
    )
    params = {
        "X-Amz-Algorithm": "AWS4-HMAC-SHA256",
        "X-Amz-Credential": access_key + "/" + scope,
        "X-Amz-Date": amz_date,
        "X-Amz-Expires": "21600",
        "X-Amz-SignedHeaders": "host",
    }
    canonical_query = "&".join(
        quote(name, safe="-_.~") + "=" + quote(params[name], safe="-_.~")
        for name in sorted(params)
    )
    canonical_request = (
        "GET\n" + canonical_uri + "\n" + canonical_query +
        "\nhost:" + host + "\n\nhost\nUNSIGNED-PAYLOAD"
    )
    string_to_sign = (
        "AWS4-HMAC-SHA256\n" + amz_date + "\n" + scope + "\n" +
        hashlib.sha256(canonical_request.encode()).hexdigest()
    )
    date_key = hmac_sha256(("AWS4" + secret_key).encode(), date_stamp)
    region_key = hmac_sha256(date_key, region)
    service_key = hmac_sha256(region_key, "s3")
    signing_key = hmac_sha256(service_key, "aws4_request")
    signature = hmac.new(signing_key, string_to_sign.encode(), hashlib.sha256).hexdigest()
    result.append({
        "source_knowledge_id": row["source_knowledge_id"],
        "url": (
            parsed_endpoint.scheme + "://" + host + canonical_uri + "?" +
            canonical_query + "&X-Amz-Signature=" + signature
        ),
    })

print(json.dumps(result, ensure_ascii=False, separators=(",", ":")))
'@
    $signerBase64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($signer))
    $doubleQuote = [char]34
    $command = 'set -o pipefail; app=$(kubectl -n weknora get pods -o name | grep "^pod/weknora-app-" | head -n1); test -n "$app"; kubectl -n weknora exec "$app" -- env INPUT_B64=' +
        $payloadBase64 +
        ' python3 -c ' + $doubleQuote +
        "exec(__import__('base64').b64decode('$signerBase64'))" + $doubleQuote +
        ' | gzip -c | base64 -w 76 | sed "s/^/PAYLOAD:/"'
    $raw = Invoke-Bridge -Command $command
    return @((ConvertFrom-GzipPayload -Output $raw) | ConvertFrom-Json -Depth 10)
}

$status = @(& python $controller status $Profile 2>&1)
if ($LASTEXITCODE -ne 0) {
    throw "cannot inspect Xshell profile $Profile"
}
$statusMap = @{}
foreach ($line in $status) {
    if ([string]$line -match '^(?<key>[^=]+)=(?<value>.*)$') {
        $statusMap[$Matches.key] = $Matches.value
    }
}
if ($statusMap.target_hint -ne $ExpectedTarget -or $statusMap.ready -ne "yes" -or $statusMap.live -ne "yes") {
    throw "Xshell profile is not the expected live production target"
}

$outputRootPath = [IO.Path]::GetFullPath($OutputRoot)
$expectedParent = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot "artifacts"))
if (-not $outputRootPath.StartsWith($expectedParent + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "OutputRoot must remain under the ignored agent-eval artifacts directory"
}
$objectsRoot = Join-Path $outputRootPath "objects"
$manifestsRoot = Join-Path $outputRootPath "kb-manifests"
$logsRoot = Join-Path $outputRootPath "logs"
New-Item -ItemType Directory -Force -Path $objectsRoot, $manifestsRoot, $logsRoot | Out-Null

$targetsSql = @'
WITH targets(source_id, slug, ordinal) AS (
  VALUES
    ('894c8102-6d8c-4b31-a20c-09436d1ea768','company-policies',1),
    ('7e98a44e-19d8-421f-b5dd-cd361d9dbb35','imoutai-private-wecom',2),
    ('795248f3-0ee0-4080-bbcc-c6d4922bc55a','expense-ops-qa',3),
    ('95ead990-423e-461e-be88-45d7a4326358','expense-operation-guide',4),
    ('93acbe0b-d6b6-4a76-9870-ba25bad4dd65','my-work',5)
)
'@

$manifestSql = $targetsSql + @'
, kb_payload AS (
  SELECT t.ordinal, jsonb_build_object(
    'source_knowledge_base_id', kb.id,
    'slug', t.slug,
    'name', kb.name,
    'description', kb.description,
    'type', kb.type,
    'chunking_config', kb.chunking_config,
    'image_processing_config', kb.image_processing_config,
    'vlm_config', jsonb_build_object(
      'enabled',kb.vlm_config->'enabled',
      'model_id',kb.vlm_config->'model_id',
      'model_name',kb.vlm_config->'model_name',
      'interface_type',kb.vlm_config->'interface_type'
    ),
    'extract_config', kb.extract_config,
    'faq_config', kb.faq_config,
    'question_generation_config', kb.question_generation_config,
    'asr_config', kb.asr_config,
    'source_wiki_config', kb.wiki_config,
    'source_indexing_strategy', kb.indexing_strategy,
    'local_indexing_strategy', coalesce(kb.indexing_strategy,'{}'::jsonb) || jsonb_build_object('wiki_enabled',false),
    'embedding_model', jsonb_build_object('source_id',em.id,'name',em.name,'display_name',em.display_name,'type',em.type,'source',em.source),
    'summary_model', jsonb_build_object('source_id',sm.id,'name',sm.name,'display_name',sm.display_name,'type',sm.type,'source',sm.source),
    'derivative_model', jsonb_build_object('source_id',dm.id,'name',dm.name,'display_name',dm.display_name,'type',dm.type,'source',dm.source),
    'source_vector_store_id', kb.vector_store_id,
    'local_overrides', jsonb_build_object(
      'wiki_enabled',false,
      'storage_provider','minio',
      'reason','local production-profile object store; wiki unavailable by operator requirement'
    ),
    'documents', (
      SELECT coalesce(jsonb_agg(jsonb_build_object(
        'source_knowledge_id',k.id,
        'title',k.title,
        'type',k.type,
        'file_name',k.file_name,
        'file_type',k.file_type,
        'file_size',k.file_size,
        'production_file_hash',k.file_hash,
        'parse_status',k.parse_status,
        'enable_status',k.enable_status,
        'core_status',k.core_status,
        'summary_status',k.summary_status,
        'enrichment_status',k.enrichment_status,
        'metadata',CASE WHEN k.type='manual' THEN jsonb_build_object(
          'format',k.metadata->'format',
          'status',k.metadata->'status',
          'content',k.metadata->'content',
          'version',k.metadata->'version',
          'updated_at',k.metadata->'updated_at',
          'process_overrides',(
            coalesce(k.metadata->'process_overrides','{}'::jsonb)
              #- '{vlm_config,api_key}'
              #- '{vlm_config,base_url}'
          )
        ) ELSE NULL END,
        'created_at',k.created_at,
        'updated_at',k.updated_at
      ) ORDER BY k.created_at,k.id),'[]'::jsonb)
      FROM knowledges k
      WHERE k.knowledge_base_id=kb.id
        AND k.deleted_at IS NULL
        AND k.enable_status='enabled'
        AND k.parse_status='completed'
        AND k.core_status='ready'
    )
  ) AS payload
  FROM targets t
  JOIN knowledge_bases kb ON kb.id=t.source_id
  LEFT JOIN models em ON em.id=kb.embedding_model_id
  LEFT JOIN models sm ON sm.id=kb.summary_model_id
  LEFT JOIN models dm ON dm.id=kb.derivative_model_id
  WHERE kb.deleted_at IS NULL
)
SELECT jsonb_build_object(
  'schema_version',2,
  'source_environment','weknora-prod',
  'capture_mode','read-only-production-full-kb',
  'generated_at',clock_timestamp(),
  'signed_urls_persisted',false,
  'wiki_enabled_locally',false,
  'model_catalog',(
    SELECT coalesce(jsonb_agg(jsonb_build_object(
      'source_id',m.id,
      'name',m.name,
      'display_name',m.display_name,
      'type',m.type,
      'source',m.source,
      'status',m.status,
      'workload_scope',m.workload_scope
    ) ORDER BY m.type,m.name,m.id),'[]'::jsonb)
    FROM models m
    WHERE m.deleted_at IS NULL
      AND m.status='active'
      AND m.tenant_id IN (
        SELECT DISTINCT kb.tenant_id
        FROM targets t
        JOIN knowledge_bases kb ON kb.id=t.source_id
      )
  ),
  'knowledge_bases',jsonb_agg(payload ORDER BY ordinal)
)
FROM kb_payload;
'@

$transferSql = $targetsSql + @'
SELECT jsonb_build_object(
  'files',
  jsonb_agg(jsonb_build_object(
    'source_knowledge_base_id',t.source_id,
    'slug',t.slug,
    'source_knowledge_id',k.id,
    'file_name',k.file_name,
    'file_size',k.file_size,
    'production_file_hash',k.file_hash,
    'file_path',k.file_path
  ) ORDER BY t.ordinal,k.created_at,k.id)
)
FROM targets t
JOIN knowledges k ON k.knowledge_base_id=t.source_id
WHERE k.deleted_at IS NULL
  AND k.enable_status='enabled'
  AND k.parse_status='completed'
  AND k.core_status='ready'
  AND k.type='file'
  AND k.file_path IS NOT NULL
  AND btrim(k.file_path)<>'';
'@

Write-Host "Exporting sanitized production KB configuration and membership..."
$manifest = Invoke-RemoteSqlJson -Sql $manifestSql
$transfer = Invoke-RemoteSqlJson -Sql $transferSql
$allDocuments = @($manifest.knowledge_bases | ForEach-Object { @($_.documents) })
$transferRows = @($transfer.files)
if ($manifest.knowledge_bases.Count -ne $expectedCounts.Count -or $allDocuments.Count -ne 682 -or $transferRows.Count -ne 680) {
    throw "production export count mismatch"
}
foreach ($kb in $manifest.knowledge_bases) {
    $slug = [string]$kb.slug
    if (-not $expectedCounts.Contains($slug) -or @($kb.documents).Count -ne $expectedCounts[$slug]) {
        throw "unexpected document count for $slug"
    }
}

$rowByID = @{}
foreach ($row in $transferRows) {
    if ([string]::IsNullOrWhiteSpace([string]$row.file_path)) {
        throw "source file path is missing for $($row.source_knowledge_id)"
    }
    $rowByID[[string]$row.source_knowledge_id] = $row
}

$downloadPlans = [Collections.Generic.List[object]]::new()
for ($offset = 0; $offset -lt $transferRows.Count; $offset += $SigningBatchSize) {
    $end = [Math]::Min($offset + $SigningBatchSize - 1, $transferRows.Count - 1)
    $batch = @($transferRows[$offset..$end])
    $signed = @(Get-RemoteSignedDownloads -Rows $batch)
    if ($signed.Count -ne $batch.Count) {
        throw "production signing batch count mismatch at offset $offset"
    }
    foreach ($item in $signed) {
        $id = [string]$item.source_knowledge_id
        $source = $rowByID[$id]
        if ($null -eq $source -or [string]::IsNullOrWhiteSpace([string]$item.url)) {
            throw "production signing did not return a valid row for $id"
        }
        $extension = [IO.Path]::GetExtension([string]$source.file_name)
        if ([string]::IsNullOrWhiteSpace($extension) -or $extension.Length -gt 16) {
            $extension = ".bin"
        }
        $relativePath = "objects/$([string]$source.slug)/$id$extension"
        $destination = [IO.Path]::GetFullPath((Join-Path $outputRootPath $relativePath))
        if (-not $destination.StartsWith($objectsRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
            throw "computed object path escaped the output root"
        }
        $downloadPlans.Add([pscustomobject]@{
            source_knowledge_id = $id
            url = [string]$item.url
            destination = $destination
            relative_path = $relativePath
            expected_size = [long]$source.file_size
            expected_hash = ([string]$source.production_file_hash).ToLowerInvariant()
        })
    }
    Write-Host "Signed $($end + 1)/$($transferRows.Count) read-only downloads"
}

Write-Host "Downloading complete knowledge bases with concurrency=$DownloadConcurrency..."
$results = @(
    $downloadPlans | ForEach-Object -Parallel {
        $plan = $_
        $destinationDirectory = Split-Path -Parent $plan.destination
        New-Item -ItemType Directory -Force -Path $destinationDirectory | Out-Null
        $algorithm = if ($plan.expected_hash.Length -eq 64) { "SHA256" } else { "MD5" }
        if (Test-Path -LiteralPath $plan.destination) {
            $existing = Get-Item -LiteralPath $plan.destination
            if ($existing.Length -eq $plan.expected_size) {
                $existingHash = (Get-FileHash -LiteralPath $plan.destination -Algorithm $algorithm).Hash.ToLowerInvariant()
                if ($existingHash -eq $plan.expected_hash) {
                    return [pscustomobject]@{
                        id = $plan.source_knowledge_id
                        relative_path = $plan.relative_path
                        status = "reused"
                        sha256 = (Get-FileHash -LiteralPath $plan.destination -Algorithm SHA256).Hash.ToLowerInvariant()
                        size = $existing.Length
                        error_type = $null
                    }
                }
            }
        }
        $partial = $plan.destination + ".part"
        try {
            if (Test-Path -LiteralPath $partial) {
                Remove-Item -LiteralPath $partial -Force
            }
            Invoke-WebRequest -Uri $plan.url -OutFile $partial -TimeoutSec 900
            $downloaded = Get-Item -LiteralPath $partial
            if ($downloaded.Length -ne $plan.expected_size) {
                throw [IO.InvalidDataException]::new("size mismatch")
            }
            $downloadHash = (Get-FileHash -LiteralPath $partial -Algorithm $algorithm).Hash.ToLowerInvariant()
            if ($downloadHash -ne $plan.expected_hash) {
                throw [IO.InvalidDataException]::new("hash mismatch")
            }
            Move-Item -LiteralPath $partial -Destination $plan.destination -Force
            return [pscustomobject]@{
                id = $plan.source_knowledge_id
                relative_path = $plan.relative_path
                status = "downloaded"
                sha256 = (Get-FileHash -LiteralPath $plan.destination -Algorithm SHA256).Hash.ToLowerInvariant()
                size = $downloaded.Length
                error_type = $null
            }
        } catch {
            if (Test-Path -LiteralPath $partial) {
                Remove-Item -LiteralPath $partial -Force
            }
            return [pscustomobject]@{
                id = $plan.source_knowledge_id
                relative_path = $plan.relative_path
                status = "failed"
                sha256 = $null
                size = 0
                error_type = $_.Exception.GetType().Name
            }
        }
    } -ThrottleLimit $DownloadConcurrency
)

$failed = @($results | Where-Object status -eq "failed")
if ($failed.Count -gt 0) {
    $failureLog = [ordered]@{
        generated_at = [DateTimeOffset]::UtcNow.ToString("o")
        failure_count = $failed.Count
        failures = @($failed | Select-Object id,error_type)
    }
    $failurePath = Join-Path $logsRoot "capture-failures.json"
    [IO.File]::WriteAllText($failurePath, ($failureLog | ConvertTo-Json -Depth 5), [Text.UTF8Encoding]::new($false))
    throw "$($failed.Count) production files failed to download; safe failure log: $failurePath"
}

$resultByID = @{}
foreach ($result in $results) {
    $resultByID[[string]$result.id] = $result
}
foreach ($kb in $manifest.knowledge_bases) {
    foreach ($document in $kb.documents) {
        $result = $resultByID[[string]$document.source_knowledge_id]
        if ([string]$document.type -eq "manual") {
            $manualContent = [string]$document.metadata.content
            if ([string]::IsNullOrWhiteSpace($manualContent)) {
                throw "manual knowledge content is missing for $($document.source_knowledge_id)"
            }
            $contentBytes = [Text.Encoding]::UTF8.GetBytes($manualContent)
            $hasher = [Security.Cryptography.SHA256]::Create()
            try {
                $contentSha = [Convert]::ToHexString($hasher.ComputeHash($contentBytes)).ToLowerInvariant()
            } finally {
                $hasher.Dispose()
            }
            $document | Add-Member -NotePropertyName relative_path -NotePropertyValue $null
            $document | Add-Member -NotePropertyName production_hash_algorithm -NotePropertyValue "manual-content"
            $document | Add-Member -NotePropertyName local_sha256 -NotePropertyValue $contentSha
            $document | Add-Member -NotePropertyName local_size -NotePropertyValue $contentBytes.Length
            continue
        }
        if ($null -eq $result) {
            throw "download result missing for $($document.source_knowledge_id)"
        }
        $document | Add-Member -NotePropertyName relative_path -NotePropertyValue ([string]$result.relative_path)
        $document | Add-Member -NotePropertyName production_hash_algorithm -NotePropertyValue $(
            if (([string]$document.production_file_hash).Length -eq 64) { "sha256" } else { "md5" }
        )
        $document | Add-Member -NotePropertyName local_sha256 -NotePropertyValue ([string]$result.sha256)
        $document | Add-Member -NotePropertyName local_size -NotePropertyValue ([long]$result.size)
    }
}
$manifest | Add-Member -NotePropertyName captured_at -NotePropertyValue ([DateTimeOffset]::UtcNow.ToString("o"))
$manifest | Add-Member -NotePropertyName document_count -NotePropertyValue $allDocuments.Count
$manifest | Add-Member -NotePropertyName total_size_bytes -NotePropertyValue (($results | Measure-Object size -Sum).Sum)
$manifestPath = Join-Path $manifestsRoot "source-manifest.json"
[IO.File]::WriteAllText(
    $manifestPath,
    ($manifest | ConvertTo-Json -Depth 100),
    [Text.UTF8Encoding]::new($false)
)

$summary = [ordered]@{
    schema_version = 1
    generated_at = [DateTimeOffset]::UtcNow.ToString("o")
    production_target = $ExpectedTarget
    knowledge_base_count = $manifest.knowledge_bases.Count
    document_count = $allDocuments.Count
    file_document_count = $transferRows.Count
    manual_document_count = @($allDocuments | Where-Object type -eq "manual").Count
    total_size_bytes = [long]$manifest.total_size_bytes
    downloaded = @($results | Where-Object status -eq "downloaded").Count
    reused = @($results | Where-Object status -eq "reused").Count
    failed = 0
    wiki_enabled_locally = $false
    signed_urls_persisted = $false
}
$summaryPath = Join-Path $manifestsRoot "capture-summary.json"
[IO.File]::WriteAllText($summaryPath, ($summary | ConvertTo-Json -Depth 5), [Text.UTF8Encoding]::new($false))
Write-Host "Production KB capture complete: documents=$($summary.document_count) bytes=$($summary.total_size_bytes)"
Write-Host "Sanitized manifest: $manifestPath"
